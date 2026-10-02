package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"k8s.io/client-go/dynamic"

	"github.com/r33drichards/browserjs-sessions/backend/internal/api"
	"github.com/r33drichards/browserjs-sessions/backend/internal/auth"
	"github.com/r33drichards/browserjs-sessions/backend/internal/billing"
	"github.com/r33drichards/browserjs-sessions/backend/internal/billing/kube"
	"github.com/r33drichards/browserjs-sessions/backend/internal/billing/metronome"
	"github.com/r33drichards/browserjs-sessions/backend/internal/config"
	"github.com/r33drichards/browserjs-sessions/backend/internal/proxy"
	"github.com/r33drichards/browserjs-sessions/backend/internal/sessions"
	"github.com/r33drichards/browserjs-sessions/backend/internal/tokens"
)

// How often the stop sequence is swept, and the deletion pass made.
const (
	billingSweepInterval  = 30 * time.Second
	billingDeleteInterval = time.Hour
)

// billingParts is metering and billing as the server wires it. A nil
// *billingParts is BILLING off: every method then does nothing, and the
// server is what it was before billing.
type billingParts struct {
	enforcer *billing.Enforcer
	handlers *billing.Handlers
	// pass is the balance pass, and webhook Metronome's webhook; either is
	// nil where there is none.
	pass    *metronome.Pass
	webhook http.Handler
	every   time.Duration // how often the pass is made
	// What the Stripe side is made over (stripe.go).
	accounts  billing.Accounts
	ledger    billing.Ledger
	catalogue billing.CatalogueSource
}

// newBilling makes billing's parts over the cluster and Metronome, nil
// when BILLING is off. It fails when the Account resource is not served,
// or the catalogue cannot be read.
func newBilling(ctx context.Context, cfg config.Config, dyn dynamic.Interface, store *sessions.Store) (*billingParts, error) {
	if cfg.Billing.Mode == billing.Off {
		return nil, nil
	}
	catalogue, err := billing.LoadCatalogue(cfg.BillingCatalogue)
	if err != nil {
		return nil, err
	}
	clock := billing.SystemClock{}
	// test with Metronome's sandbox, or live with its production
	// environment: the Account keeps each apart.
	accounts := kube.NewAccounts(dyn, cfg.Namespace, kube.ModeOf(cfg.Billing.Payments))
	if err := accounts.Check(ctx); err != nil {
		return nil, err
	}
	// Neither holds anything but its client: every answer is read when it
	// is asked for.
	ledger := billing.NewLedger(metronome.New(cfg.MetronomeURL, cfg.MetronomeToken.Reveal()), accounts, clock, catalogue)
	enforcer := billing.NewEnforcer(cfg.Billing, accounts, ledger, billing.Store{Store: store}, clock, catalogue)
	parts := &billingParts{
		enforcer: enforcer,
		handlers: &billing.Handlers{Enforcer: enforcer, Observer: kube.NewObserver(dyn, cfg.Namespace)},
		pass:     &metronome.Pass{Accounts: accounts, Ledger: ledger, Sessions: store, Clock: clock},
		every:    enforcer.Config().BalancePass,
		accounts: accounts, ledger: ledger, catalogue: catalogue,
	}
	if cfg.MetronomeWebhookSecret != "" {
		parts.webhook = &metronome.Webhook{Accounts: accounts, Ledger: ledger, Clock: clock, Secret: cfg.MetronomeWebhookSecret.Reveal()}
	} else {
		slog.Warn("METRONOME_WEBHOOK_SECRET is not set: no Metronome webhook; the balance pass alone notices credit running out")
	}
	slog.Info("metering and billing", "mode", string(cfg.Billing.Mode), "payments", cfg.Billing.Payments,
		"exempt", len(cfg.Billing.ExemptEmails), "zeroBalanceDelete", cfg.Billing.ZeroBalanceDelete, "balancePass", parts.every)
	return parts, nil
}

// metronomeWebhookPath is where Metronome posts its notifications, on the
// API host.
const metronomeWebhookPath = "/metronome/webhook"

// withWebhooks puts Metronome's webhook in front of the server's handler:
// on the API host, where nobody signs in and the signature is the
// credential. Everything else is next's, untouched.
func (b *billingParts) withWebhooks(cfg config.Config, next http.Handler) http.Handler {
	if b == nil || b.webhook == nil || cfg.APIURL == "" {
		return next
	}
	api, err := url.Parse(cfg.APIURL)
	if err != nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == metronomeWebhookPath && auth.SameHost(r.Host, api.Host) {
			b.webhook.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// enable puts billing at the enforcement points (create and resume in the
// API, wake and drain in the proxy), adds its routes, and starts its sweep
// with the proxy's calls in flight.
func (b *billingParts) enable(sessionAPI *api.API, px *proxy.Proxy, mux *http.ServeMux) {
	if b == nil {
		return
	}
	sessionAPI.EnableBilling(b.enforcer)
	px.Billing = b.enforcer
	b.handlers.Register(mux)
}

// run sweeps until ctx is done.
func (b *billingParts) run(ctx context.Context, px *proxy.Proxy) {
	if b == nil {
		return
	}
	go b.enforcer.Run(ctx, px, billingSweepInterval, billingDeleteInterval)
	if b.pass != nil {
		go b.pass.Loop(ctx, b.every)
	}
}

// revokeTokensWith lets the deletion of an account delete its API tokens.
func (b *billingParts) revokeTokensWith(store *tokens.Store) {
	if b == nil {
		return
	}
	b.handlers.RevokeTokens = func(ctx context.Context, owner string) error {
		list, err := store.List(ctx, owner)
		if err != nil {
			return err
		}
		for _, t := range list {
			if err := store.Delete(ctx, t.ID); err != nil {
				return err
			}
		}
		return nil
	}
}
