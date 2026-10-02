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
	"path"
	"strings"
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

// How long a session's owner, once read, is taken to still be its owner
// (it never changes) rather than read again for the next request.
const ownerTTL = 2 * time.Second

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
	store, err := sessions.NewStore(dyn, cfg.Namespace, string(blueprint), cfg.PublicURL, cfg.SessionURLs)
	if err != nil {
		return err
	}
	if cfg.Snapshots {
		store.EnableSnapshots(dyn, cfg.Namespace, sessions.SnapshotOptions{Timeout: cfg.SnapshotTimeout})
		slog.Info("idle sessions sleep to Pod Snapshots", "timeout", cfg.SnapshotTimeout, "restoreTimeout", cfg.RestoreTimeout)
	}
	if cfg.WarmPool != "" {
		store.EnableWarmPool(cfg.WarmPool, cfg.WarmPoolWait)
		if err := store.RecoverClaims(ctx); err != nil {
			slog.Error("warm pool: claims left unfinished", "err", err)
		}
	}
	verifier, err := auth.NewJWKSVerifier(ctx, cfg.PomeriumJWKSURL, cfg.AdminEmails)
	if err != nil {
		return err
	}
	slog.Warn("VNC tickets and idle tracking are per-process: run exactly one replica of this backend")

	tracker := idle.New(cfg.IdleAfter, time.Now)
	go idle.Run(ctx, store, tracker, time.Minute)

	handler, px := newHandler(cfg, verifier, store, tracker)

	srv := &http.Server{
		Handler:           handler,
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

// newHandler builds the server's whole route table. Requests arrive through
// Pomerium, for the app's host or for a session's; the proxy tells them
// apart and serves the sessions' itself.
func newHandler(cfg config.Config, verifier auth.Verifier, store *sessions.Store, tracker *idle.Tracker) (http.Handler, *proxy.Proxy) {
	owners := authz.NewOwners(store, ownerTTL)
	px := &proxy.Proxy{
		Verifier: verifier,
		Authz:    owners,
		Waker: &proxy.Waker{Store: store, Timeout: cfg.ReadyTimeout, RestoreTimeout: cfg.RestoreTimeout,
			Poll: time.Second, RunningTTL: 2 * time.Second},
		Idle: tracker,
		URLs: cfg.SessionURLs,
	}

	apiMux := http.NewServeMux()
	api.New(store, owners, cfg.SessionURLs, cfg.MaxSessionsPerUser).Register(apiMux)
	px.RegisterApp(apiMux)

	app := http.NewServeMux()
	app.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	apiHandler := auth.Middleware(verifier)(apiMux)
	app.Handle("/api/", apiHandler)
	app.Handle("/api", apiHandler) // or the mux redirects it to /api/
	// The app's host has no OAuth metadata (a session's host has its own, see
	// proxy/metadata.go). A client probing here must be told there is none,
	// not handed the UI.
	app.Handle("/.well-known/", http.NotFoundHandler())
	app.Handle("/", webHandler(cfg))
	return px.Handler(noAPIRedirects(app)), px
}

// noAPIRedirects answers 404 where the mux would redirect an API path to
// its clean spelling ("//api/sessions", "/api/x/../sessions", "/api"). The
// UI reads any redirect from the API as "signed out", so the API has none.
func noAPIRedirects(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clean := path.Clean("/" + r.URL.Path)
		if (clean == "/api" || strings.HasPrefix(clean, "/api/")) && clean != strings.TrimSuffix(r.URL.Path, "/") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not found"}` + "\n"))
			return
		}
		next.ServeHTTP(w, r)
	})
}
