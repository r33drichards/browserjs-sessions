package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/r33drichards/browserjs-sessions/backend/internal/auth"
	"github.com/r33drichards/browserjs-sessions/backend/internal/billing"
	"github.com/r33drichards/browserjs-sessions/backend/internal/billing/billingtest"
	bstripe "github.com/r33drichards/browserjs-sessions/backend/internal/billing/stripe"
	"github.com/r33drichards/browserjs-sessions/backend/internal/billing/stripe/stripetest"
	"github.com/r33drichards/browserjs-sessions/backend/internal/config"
	"github.com/r33drichards/browserjs-sessions/backend/internal/sessions/sessionstest"
)

// A made-up secret: the tests sign their own events with it.
const whsec = "whsec_test"

// stripeServer is the whole route table with the API host and then Stripe
// in front, as run() builds it. plain is the same without Stripe.
type stripeServer struct {
	*server
	plain    http.Handler
	accounts *billingtest.Accounts
	stripe   *stripetest.Stripe
}

func stripeConfig() config.Config {
	return config.Config{PublicURL: sessionstest.PublicURL, SessionURLs: sessionstest.URLs(), APIURL: "https://" + apiHost}
}

func newStripeServer(t *testing.T, cfg config.Config, withService bool) *stripeServer {
	t.Helper()
	s, _ := tokenServer(t, alice)
	verifier, err := auth.NewAssertionVerifier(func(*jwt.Token) (any, error) { return &s.key.PublicKey, nil }, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	out := &stripeServer{server: s, plain: s.handler}
	var svc *bstripe.Service
	if withService {
		clock := billingtest.NewClock(time.Now())
		out.accounts, out.stripe = billingtest.NewAccounts(clock), stripetest.NewStripe(clock)
		data, err := os.ReadFile("../../../docs/contracts/billing/catalogue.yaml")
		if err != nil {
			t.Fatal(err)
		}
		cat, err := billing.ParseCatalogue(data)
		if err != nil {
			t.Fatal(err)
		}
		ledger := billing.NewLedger(billingtest.NewMetronome(clock, billingtest.NewSessions(clock), cat), out.accounts, clock, cat)
		svc, err = bstripe.New(out.accounts, ledger, out.stripe, clock, bstripe.Options{
			Mode: "test", WebhookSecret: whsec, PublicURL: cfg.PublicURL, Catalogue: cat,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	s.handler = withStripe(cfg, verifier, svc, s.handler)
	return out
}

// same makes a request to the server and to the same server without Stripe
// and requires the same answer, byte for byte.
func (s *stripeServer) same(method, host, path, assertion string, headers ...string) {
	s.t.Helper()
	var got [2]*httptest.ResponseRecorder
	for i, h := range []http.Handler{s.handler, s.plain} {
		req := httptest.NewRequest(method, path, strings.NewReader("{}"))
		req.Host = host
		if assertion != "" {
			req.Header.Set(auth.AssertionHeader, assertion)
		}
		for j := 0; j+1 < len(headers); j += 2 {
			req.Header.Set(headers[j], headers[j+1])
		}
		got[i] = httptest.NewRecorder()
		h.ServeHTTP(got[i], req)
	}
	if got[0].Code != got[1].Code || got[0].Body.String() != got[1].Body.String() {
		s.t.Errorf("%s %s%s: %d %q; without Stripe %d %q", method, host, path, got[0].Code, got[0].Body, got[1].Code, got[1].Body)
	}
}

// With STRIPE_MODE unset and billing off, nothing is in front of the
// server: it is the very handler it was.
func TestStripeOffIsToday(t *testing.T) {
	reached := 0
	app := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { reached++; w.WriteHeader(http.StatusTeapot) })
	payments, err := newStripe(context.Background(), stripeConfig(), nil)
	if err != nil || payments != nil {
		t.Fatalf("newStripe with no STRIPE_MODE: %v, %v", payments, err)
	}
	h := withStripe(stripeConfig(), nil, payments, app)
	for _, r := range [][3]string{
		{"POST", apiHost, bstripe.WebhookPath}, {"POST", appHost, bstripe.WebhookPath},
		{"POST", appHost, "/api/billing/checkout"}, {"GET", appHost, "/api/billing/checkout/cs_test_1"},
		{"POST", appHost, "/api/billing/portal"}, {"PUT", appHost, "/api/billing/auto-recharge"},
		{"GET", appHost, "/api/sessions"},
	} {
		req := httptest.NewRequest(r[0], r[2], nil)
		req.Host = r[1]
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusTeapot {
			t.Errorf("%s %s%s: %d: something other than the server answered", r[0], r[1], r[2], rec.Code)
		}
	}
	if reached != 7 {
		t.Errorf("%d of 7 requests reached the server", reached)
	}

	// And through the real route table: the routes are as unknown as ever.
	s := newStripeServer(t, stripeConfig(), false)
	for _, path := range []string{"/api/billing/checkout", "/api/billing/portal"} {
		if rec := s.do("POST", appHost, path, alice, "{}"); rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s: %d, want it not to exist", path, rec.Code)
		}
		s.same("POST", appHost, path, s.assertion(alice, appHost))
	}
	s.same("POST", apiHost, bstripe.WebhookPath, "")
	if rec := s.send("POST", apiHost, bstripe.WebhookPath, "", "{}"); rec.Code != http.StatusNotFound {
		t.Errorf("the webhook exists with no Stripe: %d", rec.Code)
	}
}

// STRIPE_MODE with billing off is refused, and the error says no key.
func TestStripeModeNeedsBilling(t *testing.T) {
	cfg := stripeConfig()
	cfg.StripeMode, cfg.StripeAPIKey, cfg.StripeWebhookSecret = "test", "rk_test_madeUpForTests", whsec
	_, err := newStripe(context.Background(), cfg, nil)
	if err == nil || strings.Contains(err.Error(), "madeUpForTests") || strings.Contains(err.Error(), whsec) {
		t.Fatalf("err = %v", err)
	}
}

func TestStripeRoutes(t *testing.T) {
	s := newStripeServer(t, stripeConfig(), true)

	// The buying routes: the app's host, a signed-in user.
	rec := s.do("POST", appHost, "/api/billing/checkout", alice, "{}")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"url"`) {
		t.Fatalf("checkout: %d %s", rec.Code, rec.Body)
	}
	if rec := s.do("POST", appHost, "/api/billing/checkout", "", "{}"); rec.Code != http.StatusUnauthorized {
		t.Errorf("checkout with nobody signed in: %d, want 401", rec.Code)
	}
	if rec := s.do("POST", appHost, "/api/billing/portal", alice, ""); rec.Code != http.StatusOK {
		t.Errorf("portal: %d %s", rec.Code, rec.Body)
	}
	if rec := s.do("GET", appHost, "/api/billing/checkout/cs_test_9", alice, ""); rec.Code != http.StatusNotFound {
		t.Errorf("a checkout that is not there: %d", rec.Code)
	}
	// AUTO_RECHARGE is off.
	if rec := s.do("PUT", appHost, "/api/billing/auto-recharge", alice, `{"enabled":false}`); rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "auto_recharge_off") {
		t.Errorf("auto-recharge: %d %s", rec.Code, rec.Body)
	}

	// The webhook: the API host, no sign-in, a signature.
	acct, _ := s.accounts.Ensure(context.Background(), alice)
	_, ev := s.stripe.AttachCard(acct.Spec.StripeCustomerID, stripetest.Card{Fingerprint: "fpA", Funding: "credit"})
	post := func(host, signature string) int {
		req := httptest.NewRequest("POST", bstripe.WebhookPath, strings.NewReader(string(ev.Payload)))
		req.Host = host
		req.Header.Set("Stripe-Signature", signature)
		rec := httptest.NewRecorder()
		s.handler.ServeHTTP(rec, req)
		return rec.Code
	}
	signature := stripetest.Sign(ev.Payload, whsec, time.Now())
	if code := post(apiHost, "t=1,v1=00"); code != http.StatusBadRequest {
		t.Errorf("a bad signature: %d, want 400", code)
	}
	// Signed, and anywhere but the API host: not the webhook.
	for _, host := range []string{appHost, sessionsHost} {
		post(host, signature)
		s.same("POST", host, bstripe.WebhookPath, "", "Stripe-Signature", signature)
	}
	if acct, _ = s.accounts.Ensure(context.Background(), alice); acct.Spec.PaymentMethod != nil {
		t.Fatal("an event was handled that was not posted to the API host")
	}
	if code := post(apiHost, signature); code != http.StatusOK {
		t.Fatalf("the webhook: %d, want 200", code)
	}
	if acct, _ = s.accounts.Ensure(context.Background(), alice); acct.Spec.PaymentMethod == nil || !acct.Spec.PaymentMethod.Present {
		t.Errorf("paymentMethod = %+v after the event", acct.Spec.PaymentMethod)
	}

	// Everything else is the server's, as it was.
	_, token := s.newToken(alice, auth.Scopes...)
	for _, r := range [][4]string{
		{"POST", sessionsHost, "/api/billing/checkout", alice}, // the sessions' host is not the app's
		{"POST", appHost, "//api/billing/checkout", alice},     // no clean-path redirects
		{"POST", appHost, "/api/billing/checkout/", alice},
		{"POST", appHost, "/api/billing/../billing/checkout", alice},
		{"GET", appHost, "/api/sessions", alice},
		{"GET", appHost, "/api/billing", alice}, // not this track's
		{"GET", apiHost, "/v1/sessions", ""},
		{"GET", apiHost, "/stripe/webhook/x", ""},
	} {
		assertion := ""
		if r[3] != "" {
			assertion = s.assertion(r[3], r[1])
		}
		s.same(r[0], r[1], r[2], assertion)
	}
	// Buying is not for an API token: on the API host the routes do not
	// exist at all.
	for _, path := range []string{"/api/billing/checkout", "/v1/billing/checkout"} {
		if rec := s.bearer("POST", apiHost, path, token, "{}"); rec.Code != http.StatusNotFound {
			t.Errorf("POST %s on the API host with a token: %d, want 404", path, rec.Code)
		}
	}
	if n := s.stripe.Calls["CreateCheckout"]; n != 1 {
		t.Errorf("%d checkouts made, want 1", n)
	}
}

// Billing on and no Stripe: the UI is told nothing can be bought, and there
// is no webhook.
func TestStripePaymentsOff(t *testing.T) {
	cfg := stripeConfig()
	cfg.Billing.Mode = billing.Meter
	s := newStripeServer(t, cfg, false)
	for _, r := range [][2]string{
		{"POST", "/api/billing/checkout"}, {"GET", "/api/billing/checkout/cs_test_1"},
		{"POST", "/api/billing/portal"}, {"PUT", "/api/billing/auto-recharge"},
	} {
		rec := s.do(r[0], appHost, r[1], alice, "{}")
		if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), `"code":"payments_off"`) {
			t.Errorf("%s %s: %d %s, want 503 payments_off", r[0], r[1], rec.Code, rec.Body)
		}
		if rec := s.do(r[0], appHost, r[1], "", "{}"); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s with nobody signed in: %d, want 401", r[0], r[1], rec.Code)
		}
	}
	s.same("POST", apiHost, bstripe.WebhookPath, "")
	s.same("GET", appHost, "/api/sessions", s.assertion(alice, appHost))
}
