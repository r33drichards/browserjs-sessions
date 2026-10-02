package scenarios

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/r33drichards/computer-use/backend/internal/billing"
	"github.com/r33drichards/computer-use/backend/internal/billing/billingtest"
	"github.com/r33drichards/computer-use/backend/internal/sessions"
)

// paidUp is a user with a card and 3000 micro-dollars, and one running
// session: a minute of use from being out of credit.
func paidUp(t *testing.T) (w *world, id string) {
	t.Helper()
	w = newWorld(t, func(c *billing.Config) { c.SignupCredit = false })
	w.saveCard(user, billingtest.Card{Fingerprint: "fpA"})
	w.grant(user, 3000)
	w.tick()
	return w, w.create(user)
}

func (w *world) credit(owner string) billing.AccountCredit {
	w.t.Helper()
	c := w.account(owner).Spec.Credit
	if c == nil {
		w.t.Fatalf("%s has no credit recorded", owner)
	}
	return *c
}

// TestMetronomeWebhook is the first half of scenario 5 of
// docs/contracts/billing/testing.md: the alert's handler believes
// Metronome, not the event.
func TestMetronomeWebhook(t *testing.T) {
	t.Run("the alert sets exhausted; twice is the same; a stale one is harmless", func(t *testing.T) {
		w, _ := paidUp(t)
		customer := w.account(user).Spec.MetronomeCustomerID
		w.clock.Advance(time.Minute)
		alerts := w.metronome.Tick(w.clock.Now())
		if len(alerts) != 1 || alerts[0].Properties.CustomerID != customer {
			t.Fatalf("alerts %+v, want one for %s", alerts, customer)
		}
		if c := w.credit(user); c.Exhausted {
			t.Fatalf("exhausted before the alert arrived: %+v", c)
		}
		if res := w.alert(alerts[0]); res.Code != http.StatusOK {
			t.Fatalf("webhook = %d", res.Code)
		}
		first := w.credit(user)
		if !first.Exhausted || first.ExhaustedAt == nil || !first.ExhaustedAt.Equal(w.clock.Now()) || first.BalanceMicros != 0 {
			t.Fatalf("after the alert: %+v", first)
		}
		w.clock.Advance(time.Minute)
		if res := w.alert(alerts[0]); res.Code != http.StatusOK {
			t.Fatalf("the alert again = %d", res.Code)
		}
		if again := w.credit(user); !again.Exhausted || !again.ExhaustedAt.Equal(*first.ExhaustedAt) {
			t.Fatalf("after the alert again: %+v", again)
		}
		// Credit is bought; the old alert arrives late.
		w.buy(user, "cu_credit_5_v1")
		if res := w.alert(alerts[0]); res.Code != http.StatusOK {
			t.Fatalf("a stale alert = %d", res.Code)
		}
		if c := w.credit(user); c.Exhausted || c.ExhaustedAt != nil || c.BalanceMicros != 5_000_000 {
			t.Fatalf("a stale alert was believed: %+v", c)
		}
	})

	t.Run("a bad signature, and a date six minutes old", func(t *testing.T) {
		w, _ := paidUp(t)
		w.clock.Advance(time.Minute)
		alert := w.metronome.Tick(w.clock.Now())[0]
		body := alert.Body()
		post := func(date, signature string) int {
			req := httptest.NewRequest("POST", "https://"+apiHost+"/metronome/webhook", bytes.NewReader(body))
			req.Host = apiHost
			req.Header.Set("X-Metronome-Date", date)
			req.Header.Set("Metronome-Webhook-Signature", signature)
			rec := httptest.NewRecorder()
			w.handler.ServeHTTP(rec, req)
			return rec.Code
		}
		now := w.clock.Now().Format(time.RFC3339)
		old := w.clock.Now().Add(-6 * time.Minute).Format(time.RFC3339)
		for name, code := range map[string]int{
			"signed with another secret": post(now, billingtest.SignMetronome("another", now, body)),
			"not signed":                 post(now, ""),
			"signed for another date":    post(now, billingtest.SignMetronome(billingtest.MetronomeSecret, old, body)),
			"six minutes old":            post(old, billingtest.SignMetronome(billingtest.MetronomeSecret, old, body)),
			"no date":                    post("", billingtest.SignMetronome(billingtest.MetronomeSecret, "", body)),
		} {
			if code != http.StatusBadRequest {
				t.Errorf("%s: %d, want 400", name, code)
			}
		}
		if c := w.credit(user); c.Exhausted {
			t.Fatalf("something changed: %+v", c)
		}
		// And the same event, properly signed, is taken.
		if code := post(now, billingtest.SignMetronome(billingtest.MetronomeSecret, now, body)); code != http.StatusOK || !w.credit(user).Exhausted {
			t.Fatalf("a good signature: %d", code)
		}
	})

	t.Run("a customer no Account has, and another type of event", func(t *testing.T) {
		w, _ := paidUp(t)
		before := w.credit(user)
		calls := w.metronome.Calls()
		unknown := w.metronome.Alert("mcus-nobody")
		other := w.metronome.Alert(w.account(user).Spec.MetronomeCustomerID)
		other.Type = "alerts.spend_threshold_reached"
		for _, e := range []billingtest.MetronomeEvent{unknown, other} {
			if res := w.alert(e); res.Code != http.StatusOK {
				t.Errorf("%s for %s: %d, want 200", e.Type, e.Properties.CustomerID, res.Code)
			}
		}
		if after := w.credit(user); after != before && (after.Exhausted != before.Exhausted || after.BalanceMicros != before.BalanceMicros) {
			t.Fatalf("something changed: %+v, was %+v", after, before)
		}
		if w.metronome.Calls() != calls {
			t.Errorf("Metronome was read for an event that is not ours")
		}
	})

	t.Run("the alert never arrives: the balance pass sets exhausted", func(t *testing.T) {
		w, id := paidUp(t)
		w.clock.Advance(time.Minute)
		if alerts := w.metronome.Tick(w.clock.Now()); len(alerts) != 1 {
			t.Fatalf("alerts %+v", alerts)
		} // and it is lost
		if c := w.credit(user); c.Exhausted {
			t.Fatalf("exhausted with no alert and no pass: %+v", c)
		}
		w.clock.Advance(time.Minute)
		w.balancePass() // the account has an awake session
		c := w.credit(user)
		if !c.Exhausted || c.ExhaustedAt == nil || !c.ExhaustedAt.Equal(w.clock.Now()) {
			t.Fatalf("after the pass: %+v", c)
		}
		refusedWith(t, "create", w.api(user, "POST", "/api/sessions", `{}`), http.StatusPaymentRequired, billing.CodeOutOfCredit)
		// And the stop sequence follows from there.
		w.clock.Advance(5 * time.Minute)
		w.sweep()
		if s := w.session(id); s.State != sessions.Asleep || s.StoppedBy != sessions.StoppedByCredit {
			t.Fatalf("the session is %s, stopped by %q", s.State, s.StoppedBy)
		}
	})

	t.Run("a credit's end passes with nothing awake: the balance pass notices", func(t *testing.T) {
		w := newWorld(t, func(c *billing.Config) { c.SignupCredit = false })
		w.saveCard(user, billingtest.Card{Fingerprint: "fpA"})
		until := w.clock.Now().Add(2 * time.Hour)
		if _, _, err := w.ledger.EnsureGrant(t.Context(), billing.Grant{Account: billing.AccountName(user), Source: billing.SourcePlan,
			AmountMicros: 10_000_000, ValidFrom: w.clock.Now(), ExpiresAt: &until, Key: "plan/sub_1/1"}); err != nil {
			t.Fatal(err)
		}
		if c := w.credit(user); c.Exhausted || c.NextExpiryAt == nil || !c.NextExpiryAt.Equal(until) {
			t.Fatalf("after the grant: %+v", c)
		}
		// Before its end the pass has no reason to read this account.
		w.clock.Advance(time.Hour)
		calls := w.metronome.Calls()
		w.balancePass()
		if w.metronome.Calls() != calls || w.credit(user).Exhausted {
			t.Fatalf("the pass read an account with nothing awake and nothing due")
		}
		w.clock.Advance(2 * time.Hour)
		if alerts := w.metronome.Tick(w.clock.Now()); len(alerts) != 0 {
			t.Fatalf("an alert for an expiry: %+v", alerts)
		}
		w.balancePass()
		if c := w.credit(user); !c.Exhausted || c.BalanceMicros != 0 || c.NextExpiryAt != nil {
			t.Fatalf("after its end: %+v", c)
		}
		refusedWith(t, "create", w.api(user, "POST", "/api/sessions", `{}`), http.StatusPaymentRequired, billing.CodeOutOfCredit)
	})
}

// TestMetronomeUnreachable is the second half of scenario 5: with
// Metronome down nobody is refused and nobody is stopped for it, because
// no decision calls it.
func TestMetronomeUnreachable(t *testing.T) {
	t.Run("an account with credit: create, resume and wake are allowed, and Metronome is not called", func(t *testing.T) {
		w := newWorld(t, nil)
		w.saveCard(user, billingtest.Card{Fingerprint: "fpA"})
		w.tick()
		id := w.create(user)
		if err := w.sessions.Sleep(t.Context(), id, sessions.StoppedByIdle, nil); err != nil {
			t.Fatal(err)
		}

		w.metronome.Unreachable(true)
		calls := w.metronome.Calls()
		second := w.create(user)
		if res := w.mcp(user, id); res.Code != http.StatusOK || w.session(id).State != sessions.Running {
			t.Fatalf("wake = %d %s", res.Code, res.body())
		}
		if res := w.api(user, "PATCH", "/api/sessions/"+second, `{"action":"stop"}`); res.Code != http.StatusOK {
			t.Fatal(res.body())
		}
		if res := w.api(user, "PATCH", "/api/sessions/"+second, `{"action":"resume"}`); res.Code != http.StatusOK {
			t.Fatalf("resume = %d %s", res.Code, res.body())
		}
		if got := w.metronome.Calls() - calls; got != 0 {
			t.Fatalf("the decisions made %d calls to Metronome", got)
		}
		// Nor is anything stopped: the sweep and the pass leave it be.
		w.clock.Advance(time.Hour)
		w.balancePass()
		w.sweep()
		if s := w.session(id); s.State != sessions.Running || s.Draining != "" {
			t.Fatalf("stopped while Metronome was down: %s, draining %q", s.State, s.Draining)
		}
		if c := w.credit(user); c.Exhausted {
			t.Fatalf("the flag changed while Metronome was down: %+v", c)
		}

		// GET /api/billing says what it last knew, and that it is stale.
		res := w.api(user, "GET", "/api/billing", "")
		var b billing.BillingView
		res.json(t, &b)
		if res.Code != http.StatusOK || b.Ledger != "stale" || b.BalanceMicros != w.credit(user).BalanceMicros || b.BalanceMicros != 5_000_000 {
			t.Fatalf("GET /api/billing = %d, ledger %q, balance %d (stored %d)", res.Code, b.Ledger, b.BalanceMicros, w.credit(user).BalanceMicros)
		}
		if res := w.api(user, "GET", "/api/billing/usage", ""); res.Code != http.StatusOK {
			t.Fatalf("usage = %d %s", res.Code, res.body())
		}
		w.metronome.Unreachable(false)
		if b := w.billingOf(user); b.Ledger != "ok" {
			t.Fatalf("back up: ledger %q", b.Ledger)
		}
	})

	t.Run("an exhausted account stays refused", func(t *testing.T) {
		w, _ := paidUp(t)
		w.clock.Advance(time.Minute)
		w.tick()
		w.metronome.Unreachable(true)
		refusedWith(t, "create", w.api(user, "POST", "/api/sessions", `{}`), http.StatusPaymentRequired, billing.CodeOutOfCredit)
	})

	t.Run("a purchase waits for Metronome: 500, then the same event makes one credit", func(t *testing.T) {
		w, _ := paidUp(t)
		w.clock.Advance(time.Minute)
		w.tick()
		if !w.credit(user).Exhausted {
			t.Fatal("setup: not exhausted")
		}
		if res := w.api(user, "POST", "/api/billing/checkout", `{"item":"cu_credit_5_v1"}`); res.Code != http.StatusOK {
			t.Fatal(res.body())
		}
		all := w.stripe.CheckoutSessions()
		paid := w.stripe.CompleteCheckout(all[len(all)-1].ID, nil)

		w.metronome.Unreachable(true)
		if res := w.webhook(paid); res.Code != http.StatusInternalServerError {
			t.Fatalf("with Metronome down: %d, want 500 so that Stripe sends it again", res.Code)
		}
		if !w.credit(user).Exhausted {
			t.Fatal("exhausted was cleared with no credit made")
		}
		w.metronome.Unreachable(false)
		for range 2 { // Stripe's retry, and a replay
			if res := w.webhook(paid); res.Code != http.StatusOK {
				t.Fatalf("with Metronome back: %d", res.Code)
			}
		}
		packs := 0
		for _, c := range w.credits() {
			if c.CustomFields[billing.FieldSource] == billing.SourcePurchase {
				packs++
			}
		}
		if c := w.credit(user); packs != 1 || c.Exhausted || c.BalanceMicros != 5_000_000 {
			t.Fatalf("%d pack credits, credit %+v; want one, and not exhausted", packs, c)
		}
	})

	t.Run("a new account while Metronome is down: made, and its customer made later", func(t *testing.T) {
		w := newWorld(t, nil)
		w.metronome.Unreachable(true)
		res := w.api(other, "GET", "/api/billing", "")
		var b billing.BillingView
		res.json(t, &b)
		if res.Code != http.StatusOK || b.Ledger != "pending" || b.State != billing.StateNoCard {
			t.Fatalf("GET /api/billing = %d, ledger %q, state %s", res.Code, b.Ledger, b.State)
		}
		refusedWith(t, "create", w.api(other, "POST", "/api/sessions", `{}`), http.StatusPaymentRequired, billing.CodePaymentMethodRequired)
		w.metronome.Unreachable(false)
		if b := w.billingOf(other); b.Ledger != "ok" || w.account(other).Spec.MetronomeCustomerID == "" {
			t.Fatalf("back up: ledger %q, customer %q", b.Ledger, w.account(other).Spec.MetronomeCustomerID)
		}
	})
}
