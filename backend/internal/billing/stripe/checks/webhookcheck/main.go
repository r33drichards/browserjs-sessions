// Command webhookcheck serves the backend's real Stripe webhook handler on
// in-memory fakes, for sandbox check 1: seeing that an event Stripe sends
// arrives with a signature that verifies. It has no cluster and no Stripe
// key; the only secret it takes is the webhook's signing secret, from
// STRIPE_WEBHOOK_SECRET, which it never prints.
//
// Every event that verifies is answered 200 (the fakes hold no account, so
// nothing is done with it); one that does not is answered 400.
package main

import (
	"flag"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/r33drichards/computer-use/backend/internal/billing"
	"github.com/r33drichards/computer-use/backend/internal/billing/billingtest"
	"github.com/r33drichards/computer-use/backend/internal/billing/stripe"
	"github.com/r33drichards/computer-use/backend/internal/billing/stripe/stripetest"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:8099", "the address to listen on")
	mode := flag.String("mode", "test", "test or live: the mode of the events to accept")
	catalogue := flag.String("catalogue", "../docs/contracts/billing/catalogue.yaml", "the catalogue file")
	flag.Parse()

	cat, err := billing.LoadCatalogue(*catalogue)
	if err != nil {
		slog.Error("catalogue", "err", err)
		os.Exit(1)
	}
	clock := billingtest.NewClock(time.Now())
	accounts := billingtest.NewAccounts(clock)
	ledger := billing.NewLedger(billingtest.NewMetronome(clock, billingtest.NewSessions(clock), cat), accounts, clock, cat)
	svc, err := stripe.New(accounts, ledger, stripetest.NewStripe(clock), clock, stripe.Options{
		Mode: *mode, WebhookSecret: os.Getenv("STRIPE_WEBHOOK_SECRET"), PublicURL: "http://localhost",
		Catalogue: cat,
	})
	if err != nil {
		slog.Error("STRIPE_WEBHOOK_SECRET must be set to the signing secret (stripe listen --print-secret)", "err", err)
		os.Exit(1)
	}
	webhook := svc.Webhook()
	mux := http.NewServeMux()
	mux.HandleFunc(stripe.WebhookPath, func(w http.ResponseWriter, r *http.Request) {
		rec := &status{ResponseWriter: w, code: http.StatusOK}
		webhook.ServeHTTP(rec, r)
		slog.Info("event", "answered", rec.code, "verified", rec.code != http.StatusBadRequest, "bytes", r.ContentLength)
	})
	slog.Info("the webhook handler, on fakes", "listen", *listen, "path", stripe.WebhookPath)
	if err := http.ListenAndServe(*listen, mux); err != nil {
		slog.Error("listen", "err", err)
		os.Exit(1)
	}
}

type status struct {
	http.ResponseWriter
	code int
}

func (s *status) WriteHeader(code int) {
	s.code = code
	s.ResponseWriter.WriteHeader(code)
}
