// Command server is the browserjs sessions backend.
package main

import (
	"context"
	"log/slog"
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
	verifier, err := auth.NewJWKSVerifier(ctx, cfg.OIDCJWKSURL, cfg.OIDCIssuer, cfg.AdminRole)
	if err != nil {
		return err
	}
	var az authz.Authorizer = authz.NewMemory() // replaced by Topaz in Task 12

	tracker := idle.New(cfg.IdleAfter, time.Now)
	go idle.Run(ctx, store, tracker, time.Minute)

	mux := newMux(cfg, verifier, store, az, tracker)

	srv := &http.Server{Addr: cfg.Addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	slog.Info("listening", "addr", cfg.Addr, "namespace", cfg.Namespace)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		return err
	}
	return nil
}

// newMux builds the server's whole route table.
func newMux(cfg config.Config, verifier auth.Verifier, store *sessions.Store, az authz.Authorizer, tracker *idle.Tracker) *http.ServeMux {
	sessionsAPI := api.New(store, az, cfg.MaxSessionsPerUser)
	apiMux := http.NewServeMux()
	sessionsAPI.Register(apiMux)

	px := &proxy.Proxy{
		Verifier:  verifier,
		Authz:     az,
		Waker:     &proxy.Waker{Store: store, Timeout: cfg.ReadyTimeout, Poll: time.Second},
		Idle:      tracker,
		PublicURL: cfg.PublicURL,
		Issuer:    cfg.OIDCIssuer,
		SyncAdmin: sessionsAPI.SyncAdmin,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	px.Register(mux) // registers the more specific /api/sessions/{id}/vnc-ticket itself
	mux.Handle("/api/", auth.Middleware(verifier)(apiMux))
	mux.Handle("/", webHandler(cfg))
	return mux
}
