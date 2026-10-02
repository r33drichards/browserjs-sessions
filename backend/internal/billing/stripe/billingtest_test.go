package stripe_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/r33drichards/computer-use/backend/internal/auth"
	"github.com/r33drichards/computer-use/backend/internal/billing"
	"github.com/r33drichards/computer-use/backend/internal/billing/billingtest"
	"github.com/r33drichards/computer-use/backend/internal/billing/stripe"
)

// The Stripe half of the card gate (testing.md, scenario 1: b1 to b3, c1,
// e1) on billingtest's fake Stripe, which is the one the enforcement
// scenarios run on: the real routes and the real webhook handler, the
// events that fake returns, signed the way it signs them. What
// billingtest's stub does for those scenarios, this package does.
func TestCardGateOnBillingtestFakes(t *testing.T) {
	const user = "u@example.com"
	ctx := context.Background()
	clock := billingtest.NewClock(time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC))
	accounts := billingtest.NewAccounts(clock)
	cat := contractCatalogue(t)
	metronome := billingtest.NewMetronome(clock, billingtest.NewSessions(clock), cat)
	ledger := billing.NewLedger(metronome, accounts, clock, cat)
	fake := billingtest.NewStripe(clock)
	svc, err := stripe.New(accounts, ledger, fake, clock, stripe.Options{
		Mode: "test", WebhookSecret: billingtest.WebhookSecret, PublicURL: publicURL, AutoRecharge: true, Catalogue: cat,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.RefreshPrices(ctx); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	svc.Register(mux)
	webhook := svc.Webhook()

	call := func(method, path, body string) (int, map[string]any) {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, r.WithContext(auth.WithUser(r.Context(), auth.User{Subject: user})))
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	post := func(ev billingtest.Event) {
		t.Helper()
		body := ev.Body()
		r := httptest.NewRequest(http.MethodPost, stripe.WebhookPath, bytes.NewReader(body))
		r.Header.Set("Stripe-Signature", billingtest.Sign(billingtest.WebhookSecret, body, time.Now()))
		rec := httptest.NewRecorder()
		webhook.ServeHTTP(rec, r)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d, want 200", ev.Type, rec.Code)
		}
	}
	account := func() billing.Account {
		t.Helper()
		acct, err := accounts.Get(ctx, billing.AccountName(user))
		if err != nil {
			t.Fatal(err)
		}
		return acct
	}
	grants := func() []string {
		var keys []string
		for _, c := range metronome.AllCredits() {
			if !c.Archived {
				keys = append(keys, c.CustomFields[billing.FieldGrantKey])
			}
		}
		sort.Strings(keys)
		return keys
	}

	// b1: a setup Checkout for the account's customer.
	code, out := call("POST", "/api/billing/checkout", "{}")
	sessions := fake.CheckoutSessions()
	if code != http.StatusOK || out["url"] == "" || len(sessions) != 1 {
		t.Fatalf("checkout: %d %v; %d sessions", code, out, len(sessions))
	}
	customer := account().Spec.StripeCustomerID
	if cs := sessions[0]; cs.Mode != billing.ModeSetup || cs.Customer != customer || customer == "" || cs.URL != out["url"] {
		t.Fatalf("session = %+v, customer %q", cs, customer)
	}

	// b2: the card is saved; the signed checkout.session.completed.
	completed := fake.CompleteCheckout(sessions[0].ID, &billingtest.Card{Fingerprint: "fpA", Funding: "credit"})
	post(completed)
	acct := account()
	if pm := acct.Spec.PaymentMethod; pm == nil || !pm.Present || len(pm.IDs) != 1 {
		t.Fatalf("paymentMethod = %+v, want present", pm)
	}
	if sc := acct.Spec.SignupCredit; sc == nil || sc.State != billing.SignupGranted {
		t.Fatalf("signupCredit = %+v, want granted", sc)
	}
	equal(t, "grants", grants(), []string{"signup/fpA"})
	if c := metronome.AllCredits()[0]; c.AmountMicros != 5_000_000 {
		t.Errorf("the credit is %+v", c)
	}
	if credit := acct.Spec.Credit; credit == nil || credit.Exhausted || credit.BalanceMicros != 5_000_000 {
		t.Errorf("credit = %+v, want 5000000 and not exhausted: credit arriving is written at once", credit)
	}
	cardA := acct.Spec.PaymentMethod.IDs[0]

	// b3: the same event again; then payment_method.attached for the card.
	post(completed)
	attached := billingtest.Event{ID: "evt_again", Object: "event", Type: "payment_method.attached"}
	attached.Data.Object, _ = json.Marshal(map[string]any{"id": cardA, "object": "payment_method", "customer": customer})
	post(attached)
	equal(t, "grants: still exactly one", grants(), []string{"signup/fpA"})

	// The return from Checkout says the same.
	code, out = call("GET", "/api/billing/checkout/"+sessions[0].ID, "")
	equal(t, "the checkout's state", []any{code, out}, []any{200, map[string]any{
		"status": "complete", "kind": "setup", "signupCredit": map[string]any{"state": "granted", "amountMicros": 5_000_000}}})

	// c1: the card is removed; the event names no customer.
	post(fake.DetachCard(cardA))
	if pm := account().Spec.PaymentMethod; pm.Present || pm.RemovedAt == nil {
		t.Fatalf("paymentMethod = %+v, want absent with removedAt", pm)
	}
	equal(t, "grants: the credit is kept", grants(), []string{"signup/fpA"})

	// e1: another card. A card is back; the sign-up credit was decided.
	_, attachedB := fake.AttachCard(customer, billingtest.Card{Fingerprint: "fpB"})
	post(attachedB)
	if pm := account().Spec.PaymentMethod; !pm.Present || pm.RemovedAt != nil {
		t.Fatalf("paymentMethod = %+v, want present, removedAt cleared", pm)
	}
	equal(t, "grants: still exactly one", grants(), []string{"signup/fpA"})

	// A pack, as scenario 2 buys one.
	code, out = call("POST", "/api/billing/checkout", `{"item":"`+pack20+`"}`)
	if code != http.StatusOK {
		t.Fatalf("checkout of a pack: %d %v", code, out)
	}
	url, _ := out["url"].(string)
	pack := url[strings.LastIndex(url, "/")+1:]
	post(fake.CompleteCheckout(pack, nil))
	equal(t, "grants", grants(), []string{"purchase/" + pack, "signup/fpA"})

	// Auto-recharge, from the balance pass: this fake's charges do not say
	// which attempt they are, and the outcome is written all the same.
	if code, out := call("PUT", "/api/billing/auto-recharge", `{"enabled":true,"agree":true}`); code != http.StatusOK {
		t.Fatalf("auto-recharge: %d %v", code, out)
	}
	if err := svc.Recharge(ctx, acct.Name, 1_000_000); err != nil {
		t.Fatal(err)
	}
	a := account().Spec.AutoRecharge
	equal(t, "autoRecharge", []any{a.Enabled, a.Seq, a.ChargedCents, a.Last.Status}, []any{true, 1, 2000, "succeeded"})
	equal(t, "grants", grants(), []string{"purchase/" + pack, "recharge/" + a.Last.PaymentIntent, "signup/fpA"})
}
