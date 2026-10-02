// Command server is the browserjs sessions backend.
//
// It holds no state: everything it decides from is an object in the
// cluster, so it runs as any number of replicas, of more than one version
// at a time (docs/stateless-backend.md says where each fact lives). Every
// replica serves requests. The periodic passes (the idle sweep, billing's
// sweep and balance pass, Stripe's reconciles) are run by one replica at a
// time, elected over a Lease (internal/leader).
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
	coordinationv1 "k8s.io/client-go/kubernetes/typed/coordination/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/r33drichards/computer-use/backend/internal/api"
	"github.com/r33drichards/computer-use/backend/internal/auth"
	"github.com/r33drichards/computer-use/backend/internal/authz"
	"github.com/r33drichards/computer-use/backend/internal/config"
	"github.com/r33drichards/computer-use/backend/internal/idle"
	"github.com/r33drichards/computer-use/backend/internal/leader"
	"github.com/r33drichards/computer-use/backend/internal/metrics"
	"github.com/r33drichards/computer-use/backend/internal/policy"
	"github.com/r33drichards/computer-use/backend/internal/proxy"
	"github.com/r33drichards/computer-use/backend/internal/sessions"
	"github.com/r33drichards/computer-use/backend/internal/tokens"
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

// newReplica names this process among the replicas, for the marks it puts on
// sessions (sessions.AnnInFlightPrefix) and, with the host's name, in the
// election. A process that starts again is a new replica: what the old one
// vouched for runs out.
func newReplica() string {
	b := make([]byte, 5)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
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
	// Before the claims are recovered: a claim may carry a policy to make.
	if cfg.PolicyOperatorURL != "" {
		store.EnablePolicies(dyn, cfg.Namespace, policy.Unrestricted())
		slog.Info("sessions have policies", "operator", cfg.PolicyOperatorURL)
	}
	if cfg.WarmPool != "" {
		store.EnableWarmPool(cfg.WarmPool, cfg.WarmPoolWait)
	}
	verifier, err := auth.NewJWKSVerifier(ctx, cfg.PomeriumJWKSURL, cfg.AdminEmails)
	if err != nil {
		return err
	}
	if len(cfg.APISigningKey) == 0 {
		slog.Warn("API_SIGNING_KEY is not set: VNC tickets and API access tokens are signed with a key of this process, and another replica refuses them. Run one replica, or set the key")
	}
	leases, err := coordinationv1.NewForConfig(kube)
	if err != nil {
		return err
	}
	replica := newReplica()
	host, _ := os.Hostname()
	slog.Info("replica", "replica", replica, "host", host)
	if err := serveMetrics(ctx, cfg.MetricsAddr); err != nil {
		return err
	}

	// This replica's part in idleness: it writes, on each session it
	// proxies to, when the session was used. The sweep is the leader's.
	tracker := idle.New(store, replica, cfg.IdleAfter, time.Now)
	go tracker.Run(ctx)
	if err := metrics.Sessions(metrics.Registry, tracker.States); err != nil {
		return err
	}

	// Metering and billing (BILLING): billing.go. Nil while it is off.
	bill, err := newBilling(ctx, cfg, dyn, store)
	if err != nil {
		return err
	}
	// Stripe (STRIPE_MODE): stripe.go. Nil without it. Before billing runs:
	// its balance pass asks Stripe's side for auto-recharge.
	payments, err := newStripe(ctx, cfg, bill)
	if err != nil {
		return err
	}
	handler, px := newHandlerWith(cfg, verifier, store, tracker, bill)
	// The periodic passes, on one replica at a time. Each keeps nothing
	// between runs and reads what it decides from off the cluster.
	// A replica a release is still checking (ACTIVE_FILE) does not campaign.
	go leader.Run(ctx, leases, cfg.Namespace, host+"_"+replica, leader.DefaultTiming, leader.Active(cfg.ActiveFile), func(ctx context.Context) {
		metrics.Leader.Set(1)
		defer metrics.Leader.Set(0)
		// What a backend that died in the middle of a create left undone.
		// The leader's, so that it is done once and not by a replica that
		// is only being checked; doing it again changes nothing.
		if cfg.WarmPool != "" {
			if err := store.RecoverClaims(ctx); err != nil {
				slog.Error("warm pool: claims left unfinished", "err", err)
			}
		}
		go idle.Run(ctx, store, idle.Rule{After: cfg.IdleAfter, Margin: idle.DefaultMargin, Source: tracker}, time.Minute)
		bill.run(ctx, px)
		if payments != nil {
			go payments.Run(ctx)
		}
		<-ctx.Done()
	})
	// API tokens and the API host (API_URL): tokens.go.
	if cfg.APIURL != "" {
		tokenStore := tokens.NewStore(dyn, cfg.Namespace)
		bill.revokeTokensWith(tokenStore)
		if handler, err = withAPITokens(cfg, verifier, tokenStore, handler); err != nil {
			return err
		}
		slog.Info("API host", "url", cfg.APIURL, "tokens", cfg.APITokens(), "signingKeyKept", len(cfg.APISigningKey) > 0)
	}
	// In front of the API host, which knows nothing of Stripe's webhook.
	handler = withStripe(cfg, verifier, payments, handler)

	handler = bill.withWebhooks(cfg, handler)

	srv := &http.Server{
		Handler: metrics.Instrument(func(r *http.Request) (string, string) {
			path, session := px.Route(r)
			return metrics.Classify(session, r.Method, path)
		}, handler),
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
	err = serve(ctx, srv, ln, 10*time.Second, px)
	// What this replica knew of its sessions' last use and had not written
	// yet (it writes every half minute) is written before it goes.
	flush, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tracker.Flush(flush)
	return err
}

// serveMetrics serves /metrics at addr until ctx is done: on a port of its
// own, which nothing routes to from outside the cluster and the
// NetworkPolicy opens to the collector only. "off" or "" serves nothing.
func serveMetrics(ctx context.Context, addr string) error {
	if addr == "" || addr == "off" {
		return nil
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: metrics.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	go func() {
		if err := srv.Serve(ln); err != http.ErrServerClosed {
			slog.Error("metrics server stopped", "err", err)
		}
	}()
	slog.Info("metrics", "addr", addr)
	return nil
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
// Pomerium, for the app's host or for the sessions'; the proxy tells them
// apart and serves the sessions' itself.
func newHandler(cfg config.Config, verifier auth.Verifier, store *sessions.Store, tracker *idle.Tracker) (http.Handler, *proxy.Proxy) {
	return newHandlerWith(cfg, verifier, store, tracker, nil)
}

// newHandlerWith is newHandler with metering and billing (nil for none).
func newHandlerWith(cfg config.Config, verifier auth.Verifier, store *sessions.Store, tracker *idle.Tracker, bill *billingParts) (http.Handler, *proxy.Proxy) {
	owners := authz.NewOwners(store, ownerTTL)
	// Nil, and so no policy routes and no gate, unless the store has
	// policies enabled.
	policies := policy.New(store, policy.NewOperator(cfg.PolicyOperatorURL, cfg.OperatorAPIToken))
	px := &proxy.Proxy{
		Verifier: verifier,
		Authz:    owners,
		// A session's pod is not sent anything before the session's first
		// policy is in force.
		Waker: &proxy.Waker{Store: policy.Gate(store, policies), Timeout: cfg.ReadyTimeout, RestoreTimeout: cfg.RestoreTimeout,
			Poll: time.Second, RunningTTL: 2 * time.Second},
		Idle:         tracker,
		TicketKey:    proxy.TicketKey(cfg.APISigningKey),
		URLs:         cfg.SessionURLs,
		LegacyURLs:   cfg.LegacySessionURLs,
		MaxFileBytes: cfg.MaxFileBytes,
	}

	apiMux := http.NewServeMux()
	sessionAPI := api.New(store, owners, cfg.SessionURLs, cfg.MaxSessionsPerUser)
	sessionAPI.EnablePolicies(policies)
	bill.enable(sessionAPI, px, apiMux)
	sessionAPI.Register(apiMux)
	px.RegisterApp(apiMux)

	app := http.NewServeMux()
	app.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	apiHandler := auth.Middleware(verifier)(apiMux)
	app.Handle("/api/", apiHandler)
	app.Handle("/api", apiHandler) // or the mux redirects it to /api/
	// The app's host has no OAuth metadata (the sessions' host has, from
	// Pomerium). A client probing here must be told there is none, not handed
	// the UI.
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
