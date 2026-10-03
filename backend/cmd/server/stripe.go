package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/r33drichards/computer-use/backend/internal/auth"
	"github.com/r33drichards/computer-use/backend/internal/billing"
	bstripe "github.com/r33drichards/computer-use/backend/internal/billing/stripe"
	"github.com/r33drichards/computer-use/backend/internal/config"
)

// The pause between two accounts of a reconcile: far below Stripe's rate
// limits (each account is a handful of reads).
const reconcilePace = 200 * time.Millisecond

// newStripe makes the Stripe side of billing over billing's account store
// and ledger. Its reconcile is one of the leader's passes (main.go). It is
// nil, with no error, while STRIPE_MODE is unset. It gives billing what
// billing asks of Stripe: the client that account deletion cancels and
// detaches with, and auto-recharge for the balance pass.
func newStripe(ctx context.Context, cfg config.Config, bill *billingParts) (*bstripe.Service, error) {
	if cfg.StripeMode == "" {
		return nil, nil
	}
	if bill == nil {
		return nil, errors.New("STRIPE_MODE requires BILLING to be meter or enforce")
	}
	client := bstripe.NewAPI(cfg.StripeAPIKey.Reveal(), bill.catalogue.Catalogue().Currency, "").IDsFile(cfg.BillingIDs, cfg.StripeMode)
	svc, err := bstripe.New(bill.accounts, bill.ledger, client, billing.SystemClock{}, bstripe.Options{
		Mode:           cfg.StripeMode,
		WebhookSecret:  cfg.StripeWebhookSecret.Reveal(),
		PublicURL:      cfg.PublicURL,
		AutoRecharge:   cfg.Billing.AutoRecharge,
		NoSignupCredit: !cfg.Billing.SignupCredit,
		Catalogue:      bill.catalogue,
		Pace:           reconcilePace,
	})
	if err != nil {
		return nil, err
	}
	bill.handlers.Stripe = client
	if cfg.Billing.AutoRecharge && bill.pass != nil {
		bill.pass.Recharge = svc
	}
	// Before the first request: without prices nothing can be bought.
	// They are read again every hour, by every replica; the reconciles are
	// the leader's (passes, main.go).
	if err := svc.RefreshPrices(ctx); err != nil {
		slog.Error("Stripe: prices not read; nothing is offered until they are", "err", err)
	}
	go svc.KeepPrices(ctx)
	slog.Info("Stripe", "mode", cfg.StripeMode, "autoRecharge", cfg.Billing.AutoRecharge, "webhook", cfg.APIURL+bstripe.WebhookPath)
	return svc, nil
}

// withStripe puts Stripe's routes in front of the server's handler (app,
// the whole of it, the API host included):
//
//   - On the API host, POST /stripe/webhook is Stripe's: nobody is signed
//     in, and the request's signature is its credential.
//   - On the app's host, the checkout, portal and auto-recharge routes
//     under /api/billing, for a signed-in user.
//
// With svc nil there is no Stripe: app is returned untouched, unless
// billing is on, when the same routes on the app's host
// answer 503 payments_off and there is still no webhook.
//
// Everything else is app's, untouched.
func withStripe(cfg config.Config, verifier auth.Verifier, svc *bstripe.Service, app http.Handler) http.Handler {
	if svc == nil && cfg.Billing.Mode != billing.Meter && cfg.Billing.Mode != billing.Enforce {
		return app
	}
	hostOf := func(raw string) string {
		u, err := url.Parse(raw)
		if err != nil {
			return ""
		}
		return u.Host
	}
	apiHostName, appHostName := hostOf(cfg.APIURL), hostOf(cfg.PublicURL)

	var webhook http.Handler
	buying := bstripe.PaymentsOff(cfg.PublicURL)
	if svc != nil {
		webhook = svc.Webhook()
		mux := http.NewServeMux()
		svc.Register(mux)
		buying = mux
	}
	buying = auth.Middleware(verifier)(buying)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case webhook != nil && apiHostName != "" && auth.SameHost(r.Host, apiHostName) && p == bstripe.WebhookPath:
			webhook.ServeHTTP(w, r)
		// A path spelled any other way is app's to refuse: the API has no
		// redirects (noAPIRedirects).
		case auth.SameHost(r.Host, appHostName) && stripePath(p) && path.Clean(p) == p && r.URL.RawPath == "":
			buying.ServeHTTP(w, r)
		default:
			app.ServeHTTP(w, r)
		}
	})
}

// stripePath reports whether p is one of the Stripe side's routes on the
// app's host.
func stripePath(p string) bool {
	for _, route := range bstripe.Paths {
		if p == route || (strings.HasSuffix(route, "/") && strings.HasPrefix(p, route)) {
			return true
		}
	}
	return false
}
