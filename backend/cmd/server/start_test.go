package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"

	"github.com/r33drichards/computer-use/backend/internal/auth"
	"github.com/r33drichards/computer-use/backend/internal/billing"
	"github.com/r33drichards/computer-use/backend/internal/billing/kube"
	"github.com/r33drichards/computer-use/backend/internal/config"
	"github.com/r33drichards/computer-use/backend/internal/idle"
	"github.com/r33drichards/computer-use/backend/internal/sessions/sessionstest"
)

// The backend starts with BILLING=meter and STRIPE_MODE=test: the
// configuration is read from the environment as the Deployment gives it,
// and billing's parts and Stripe's are made as run() makes them, over a
// cluster that serves the Account resource. Stripe and Metronome are not
// reached: neither is needed to start.
func TestStartsWithMeterAndStripeTest(t *testing.T) {
	// Metronome is down; Stripe is never asked (the context is done before
	// its first read, as at a shutdown).
	metronome := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) }))
	t.Cleanup(metronome.Close)
	env := map[string]string{
		"PUBLIC_URL": sessionstest.PublicURL, "SESSION_URL_TEMPLATE": sessionstest.URLTemplate,
		"POMERIUM_JWKS_URL": "http://pomerium/jwks.json", "API_URL": "https://api.example.com", "ADMIN_EMAILS": root,
		"BILLING": "meter", "BILLING_CATALOGUE": "../../../docs/contracts/billing/catalogue.yaml",
		"METRONOME_API_TOKEN": "made-up", "METRONOME_WEBHOOK_SECRET": "made-up-too", "METRONOME_URL": metronome.URL,
		"STRIPE_MODE": "test", "STRIPE_API_KEY": "rk_" + "test_madeup", "STRIPE_WEBHOOK_SECRET": "whsec_" + "madeup",
	}
	cfg, err := config.FromEnv(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Billing.Mode != billing.Meter || cfg.Billing.Payments != "test" {
		t.Fatalf("mode %s, payments %s", cfg.Billing.Mode, cfg.Billing.Payments)
	}

	cluster := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		kube.AccountGVR: "AccountList",
		{Group: "coordination.k8s.io", Version: "v1", Resource: "leases"}: "LeaseList",
	})
	store, _ := sessionstest.New(t)
	ctx, stop := context.WithCancel(t.Context())
	stop()
	bill, err := newBilling(ctx, cfg, cluster, store)
	if err != nil || bill == nil {
		t.Fatalf("newBilling: %v, %v", bill, err)
	}
	payments, err := newStripe(ctx, cfg, bill)
	if err != nil || payments == nil {
		t.Fatalf("newStripe: %v, %v", payments, err)
	}
	if bill.handlers.Stripe == nil || bill.webhook == nil {
		t.Fatalf("billing was not given Stripe's client (%v) or has no Metronome webhook (%v)", bill.handlers.Stripe, bill.webhook)
	}

	s := newServer(t)
	handler, _ := newHandlerWith(cfg, verifierOf(t, s), store, idle.New(store, "test", 15*time.Minute, time.Now), bill)
	s.handler = bill.withWebhooks(cfg, withStripe(cfg, verifierOf(t, s), payments, handler))

	// The account is made in the cluster on first sight, in test mode;
	// with Metronome down the page says so and refuses nobody.
	rec := s.do("GET", appHost, "/api/billing", alice, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"mode":"meter"`) || !strings.Contains(rec.Body.String(), `"payments":"test"`) ||
		!strings.Contains(rec.Body.String(), `"state":"no_card"`) || !strings.Contains(rec.Body.String(), `"ledger":"pending"`) {
		t.Fatalf("GET /api/billing: %d %s", rec.Code, rec.Body)
	}
	// Meter mode refuses nothing.
	if rec := s.do("POST", appHost, "/api/sessions", alice, `{}`); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	// Both webhooks are on the API host, and refuse what is not signed.
	for _, path := range []string{"/stripe/webhook", "/metronome/webhook"} {
		if rec := s.do("POST", "api.example.com", path, "", `{}`); rec.Code != http.StatusBadRequest {
			t.Errorf("POST %s unsigned = %d, want 400", path, rec.Code)
		}
	}
	// Stripe's routes for a signed-in user exist.
	if rec := s.do("POST", appHost, "/api/billing/portal", alice, `{}`); rec.Code == http.StatusNotFound || rec.Code == http.StatusServiceUnavailable {
		t.Errorf("portal = %d: the route is not Stripe's", rec.Code)
	}

	// The same environment with BILLING=enforce starts too; without
	// STRIPE_MODE it does not.
	env["BILLING"] = "enforce"
	if _, err := config.FromEnv(func(k string) string { return env[k] }); err != nil {
		t.Errorf("enforce with Stripe: %v", err)
	}
	delete(env, "STRIPE_MODE")
	if _, err := config.FromEnv(func(k string) string { return env[k] }); err == nil {
		t.Error("enforce without STRIPE_MODE started")
	}
}

// verifierOf verifies the assertions s signs.
func verifierOf(t *testing.T, s *server) auth.Verifier {
	t.Helper()
	v, err := auth.NewAssertionVerifier(func(*jwt.Token) (any, error) { return &s.key.PublicKey, nil }, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	return v
}
