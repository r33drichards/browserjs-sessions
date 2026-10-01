// Command server is the browserjs sessions backend.
//
// It must run as exactly one replica (a Deployment with replicas: 1 and the
// Recreate strategy). Two things are kept in this process's memory and are
// not shared: the VNC tickets (internal/proxy), so a ticket issued by one
// replica is refused by another, and the idle tracker (internal/idle), so a
// replica would put to sleep sessions that are busy on the other.
package main

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/r33drichards/browserjs-sessions/backend/internal/api"
	"github.com/r33drichards/browserjs-sessions/backend/internal/auth"
	"github.com/r33drichards/browserjs-sessions/backend/internal/authz"
	"github.com/r33drichards/browserjs-sessions/backend/internal/config"
	"github.com/r33drichards/browserjs-sessions/backend/internal/idle"
	"github.com/r33drichards/browserjs-sessions/backend/internal/proxy"
	"github.com/r33drichards/browserjs-sessions/backend/internal/sessions"
)

// Limits on this process's requests to the API server.
const (
	kubeQPS   = 50
	kubeBurst = 100
)

func kubeConfig() (*rest.Config, error) {
	if cfg, err := rest.InClusterConfig(); err == nil {
		return cfg, nil
	}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(), nil).ClientConfig()
}

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, err := config.FromEnv(os.Getenv)
	if err != nil {
		return err
	}
	kube, err := kubeConfig()
	if err != nil {
		return err
	}
	// client-go's defaults (5 requests a second, bursts of 10) are for a
	// controller, not for something that asks on behalf of user requests.
	kube.QPS, kube.Burst = kubeQPS, kubeBurst
	dyn, err := dynamic.NewForConfig(kube)
	if err != nil {
		return err
	}
	blueprint, err := os.ReadFile(cfg.BlueprintPath)
	if err != nil {
		return err
	}
	store, err := sessions.NewStore(dyn, cfg.Namespace, string(blueprint), cfg.PublicURL)
	if err != nil {
		return err
	}
	verifier, err := auth.NewJWKSVerifier(ctx, cfg.OIDCJWKSURL, cfg.OIDCIssuer, cfg.AdminRole, cfg.AllowedClients)
	if err != nil {
		return err
	}
	var az authz.Authorizer = authz.NewMemory() // replaced by Topaz in Task 12
	slog.Warn("session ownership is kept in memory: sessions that exist already have no owner after a restart")
	slog.Warn("VNC tickets and idle tracking are per-process: run exactly one replica of this backend")

	tracker := idle.New(cfg.IdleAfter, time.Now)
	go idle.Run(ctx, store, tracker, time.Minute)

	mux, px := newMux(cfg, verifier, store, az, tracker)

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		// No read or write timeout: MCP streams and uploads run long. The
		// upload route, which has no login, bounds its own body.
	}
	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return err
	}
	slog.Info("listening", "addr", cfg.Addr, "namespace", cfg.Namespace)
	return serve(ctx, srv, ln, 10*time.Second, px)
}

// serve answers requests on ln until ctx is done, then shuts down: viewer
// connections are closed (the server neither closes nor waits for hijacked
// connections), requests in flight get up to grace to finish, and only then
// does serve return.
func serve(ctx context.Context, srv *http.Server, ln net.Listener, grace time.Duration, px *proxy.Proxy) error {
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), grace)
		defer cancel()
		if err := px.Shutdown(shutdown); err != nil {
			slog.Warn("viewer connections still open at shutdown", "err", err)
		}
		if err := srv.Shutdown(shutdown); err != nil {
			slog.Warn("requests still in flight at shutdown; closing them", "err", err)
			_ = srv.Close()
		}
	}()
	// Serve returns as soon as Shutdown is called, not when it is done.
	if err := srv.Serve(ln); err != http.ErrServerClosed {
		return err
	}
	<-stopped
	return nil
}

// newMux builds the server's whole route table.
func newMux(cfg config.Config, verifier auth.Verifier, store *sessions.Store, az authz.Authorizer, tracker *idle.Tracker) (*http.ServeMux, *proxy.Proxy) {
	sessionsAPI := api.New(store, az, cfg.MaxSessionsPerUser)
	apiMux := http.NewServeMux()
	sessionsAPI.Register(apiMux)

	px := &proxy.Proxy{
		Verifier:  verifier,
		Authz:     az,
		Waker:     &proxy.Waker{Store: store, Timeout: cfg.ReadyTimeout, Poll: time.Second, RunningTTL: 2 * time.Second},
		Idle:      tracker,
		PublicURL: cfg.PublicURL,
		Issuer:    cfg.OIDCIssuer,
		SyncAdmin: sessionsAPI.SyncAdmin,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	px.Register(mux) // registers the more specific /api/sessions/{id}/vnc-ticket itself
	mux.Handle("/api/", auth.Middleware(verifier)(apiMux))
	// Only the metadata documents registered above exist here. An MCP client
	// probing for others must be told so, not handed the UI.
	mux.Handle("/.well-known/", http.NotFoundHandler())
	mux.Handle("/", webHandler(cfg))
	return mux, px
}
