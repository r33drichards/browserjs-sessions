package stripe_test

import (
	"errors"
	"fmt"
	"github.com/r33drichards/computer-use/backend/internal/billing/stripe/stripetest"
	"net/http"
	"testing"
	"time"

	"github.com/r33drichards/computer-use/backend/internal/auth"
	"github.com/r33drichards/computer-use/backend/internal/billing"
	"github.com/r33drichards/computer-use/backend/internal/billing/stripe"
)

func wantError(t *testing.T, what string, code int, out map[string]any, status int, errCode string) {
	t.Helper()
	if code != status || out["code"] != errCode {
		t.Errorf("%s: status %d, body %v; want %d %s", what, code, out, status, errCode)
		return
	}
	if msg, _ := out["error"].(string); msg == "" {
		t.Errorf("%s: no sentence for a person in %v", what, out)
	}
	if out["billingUrl"] != publicURL+"/billing" {
		t.Errorf("%s: billingUrl = %v", what, out["billingUrl"])
	}
}

func TestCheckoutSavesACard(t *testing.T) {
	w := newWorld(t)
	code, out := w.call(alice, "POST", "/api/billing/checkout", "")
	if code != http.StatusOK || out["url"] == "" {
		t.Fatalf("status %d, body %v", code, out)
	}
	acct := w.account(alice)
	sessions := w.stripe.CheckoutSessions()
	if len(sessions) != 1 {
		t.Fatalf("%d checkout sessions, want 1", len(sessions))
	}
	cs := sessions[0]
	equal(t, "the session", []any{cs.Mode, cs.Customer, cs.ClientReferenceID, cs.Metadata, cs.URL},
		[]any{"setup", acct.Spec.StripeCustomerID, acct.Name, map[string]string{"account": acct.Name, "kind": "setup"}, out["url"]})
	if acct.Spec.StripeCustomerID == "" {
		t.Error("the customer was not written to the Account")
	}
	// The customer is made once.
	w.startCheckout(alice, "")
	if got := w.stripe.Calls["CreateCustomer"]; got != 1 {
		t.Errorf("%d customers made, want 1", got)
	}
	equal(t, "grants: nothing is granted for starting", w.live(), []string(nil))
}

func TestCheckoutBuys(t *testing.T) {
	w := newWorld(t)
	acct := w.saveCard(alice, cardA)
	for item, want := range map[string][]string{
		starter: {"subscription", "plan"},
		pack20:  {"payment", "purchase"},
	} {
		id := w.startCheckout(alice, item)
		cs, err := w.stripe.Checkout(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		equal(t, item, []any{cs.Mode, cs.Customer, cs.ClientReferenceID, cs.Metadata},
			[]any{want[0], acct.Spec.StripeCustomerID, acct.Name, map[string]string{"account": acct.Name, "kind": want[1], "item": item}})
	}
}

func TestCheckoutRefusals(t *testing.T) {
	t.Run("an API token", func(t *testing.T) {
		w := newWorld(t)
		token := auth.User{Subject: alice, Token: &auth.TokenInfo{Name: "ci", Scopes: []string{"sessions:write"}}}
		for _, route := range [][2]string{
			{"POST", "/api/billing/checkout"}, {"GET", "/api/billing/checkout/cs_test_1"},
			{"POST", "/api/billing/portal"}, {"PUT", "/api/billing/auto-recharge"},
		} {
			code, out := w.as(token, route[0], route[1], "{}")
			wantError(t, route[1], code, out, http.StatusForbidden, "ui_only")
		}
		if len(w.accounts.All()) != 0 || w.stripe.Calls["CreateCustomer"] != 0 {
			t.Error("a token made an Account or a customer")
		}
	})
	t.Run("nobody", func(t *testing.T) {
		w := newWorld(t)
		rec, r := newRequest("POST", "/api/billing/checkout")
		w.mux.ServeHTTP(rec, r)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status %d, want 401", rec.Code)
		}
	})
	t.Run("a blocked account, a deleted account", func(t *testing.T) {
		w := newWorld(t)
		now := w.clock.Now()
		for owner, change := range map[string]func(*billing.AccountSpec) error{
			alice: func(s *billing.AccountSpec) error { s.Blocked = &billing.Blocked{Reason: "abuse"}; return nil },
			bob:   func(s *billing.AccountSpec) error { s.DeletedAt = &now; return nil },
		} {
			if _, err := w.accounts.Update(ctx, w.account(owner).Name, change); err != nil {
				t.Fatal(err)
			}
			for _, body := range []string{"{}", `{"item":"` + pack20 + `"}`} {
				code, out := w.call(owner, "POST", "/api/billing/checkout", body)
				wantError(t, owner, code, out, http.StatusForbidden, "account_blocked")
			}
		}
		if w.stripe.Calls["CreateCustomer"]+w.stripe.Calls["CreateCheckout"] != 0 {
			t.Error("Stripe was called for a blocked account")
		}
	})
	t.Run("an item that is not sold", func(t *testing.T) {
		w := newWorld(t)
		// Not in the catalogue; in it and not enabled; enabled with no
		// price at Stripe.
		w.saveCard(alice, cardA)
		for _, item := range []string{"cu_nothing_v1", "cu_scale_monthly_v1", "credit-20"} {
			code, out := w.call(alice, "POST", "/api/billing/checkout", `{"item":"`+item+`"}`)
			wantError(t, item, code, out, http.StatusBadRequest, "unknown_item")
		}
		if got := w.stripe.Calls["CreateCheckout"]; got != 1 {
			t.Errorf("%d checkouts made, want only the card's", got)
		}
	})
	t.Run("a body that is not JSON", func(t *testing.T) {
		w := newWorld(t)
		if code, _ := w.call(alice, "POST", "/api/billing/checkout", `item=x`); code != http.StatusBadRequest {
			t.Errorf("status %d, want 400", code)
		}
	})
	t.Run("subscribe while subscribed", func(t *testing.T) {
		for _, status := range []string{"active", "trialing", "past_due", "incomplete"} {
			w := newWorld(t)
			w.saveCard(alice, cardA)
			sub := w.subscribe(alice, starter)
			w.ok(w.stripe.ChangeSubscription(sub, func(s *billing.Subscription) { s.Status = status }))
			made := w.stripe.Calls["CreateCheckout"]
			code, out := w.call(alice, "POST", "/api/billing/checkout", `{"item":"`+pro+`"}`)
			wantError(t, status, code, out, http.StatusConflict, "already_subscribed")
			if w.stripe.Calls["CreateCheckout"] != made {
				t.Errorf("%s: a checkout was made", status)
			}
			// A pack can still be bought.
			w.startCheckout(alice, pack20)
		}
		// One that has ended is no obstacle.
		w := newWorld(t)
		w.saveCard(alice, cardA)
		sub := w.subscribe(alice, starter)
		w.ok(w.stripe.ChangeSubscription(sub, func(s *billing.Subscription) { s.Status = "canceled" }))
		w.startCheckout(alice, pro)
	})
	t.Run("a sixth card", func(t *testing.T) {
		w := newWorld(t)
		customer := w.saveCard(alice, cardA).Spec.StripeCustomerID
		for i := 0; i < 4; i++ {
			_, ev := w.stripe.AttachCard(customer, stripetest.Card{Fingerprint: fmt.Sprint("fp", i)})
			w.ok(ev)
		}
		code, out := w.call(alice, "POST", "/api/billing/checkout", "{}")
		wantError(t, "five cards saved", code, out, http.StatusConflict, "too_many_cards")
		// Buying is not saving a card.
		w.startCheckout(alice, pack20)
	})
	t.Run("a sixth setup checkout in a day", func(t *testing.T) {
		w := newWorld(t)
		for i := 0; i < 5; i++ {
			w.startCheckout(alice, "")
			w.clock.Advance(time.Hour)
		}
		code, out := w.call(alice, "POST", "/api/billing/checkout", "{}")
		wantError(t, "the sixth", code, out, http.StatusTooManyRequests, "rate_limited")
		if got := len(w.stripe.CheckoutSessions()); got != 5 {
			t.Errorf("%d checkout sessions, want 5", got)
		}
		w.startCheckout(alice, pack20)
		// A day after the first, one more.
		w.clock.Advance(20 * time.Hour)
		w.startCheckout(alice, "")
	})
	t.Run("Stripe cannot be reached", func(t *testing.T) {
		w := newWorld(t)
		w.stripe.Err = errors.New("timeout")
		code, out := w.call(alice, "POST", "/api/billing/checkout", "{}")
		wantError(t, "no customer yet", code, out, http.StatusBadGateway, "stripe_unavailable")
		w.stripe.Err = nil
		id := w.startCheckout(alice, "")
		w.stripe.Err = errors.New("timeout")
		code, out = w.call(alice, "POST", "/api/billing/checkout", `{"item":"`+pack20+`"}`)
		wantError(t, "with a customer", code, out, http.StatusBadGateway, "stripe_unavailable")
		code, out = w.call(alice, "GET", "/api/billing/checkout/"+id, "")
		wantError(t, "reading a checkout", code, out, http.StatusBadGateway, "stripe_unavailable")
		code, out = w.call(alice, "POST", "/api/billing/portal", "")
		wantError(t, "the portal", code, out, http.StatusBadGateway, "stripe_unavailable")
	})
}

func TestCheckoutState(t *testing.T) {
	t.Run("a setup, from open to complete, fulfilled with no webhook", func(t *testing.T) {
		w := newWorld(t)
		id := w.startCheckout(alice, "")
		code, out := w.call(alice, "GET", "/api/billing/checkout/"+id, "")
		equal(t, "open", []any{code, out}, []any{200, map[string]any{"status": "open", "kind": "setup", "signupCredit": map[string]any{"state": "pending"}}})

		w.stripe.CompleteCheckout(id, cardA) // the event never arrives
		code, out = w.call(alice, "GET", "/api/billing/checkout/"+id, "")
		equal(t, "complete", []any{code, out}, []any{200, map[string]any{
			"status": "complete", "kind": "setup", "signupCredit": map[string]any{"state": "granted", "amountMicros": 5_000_000}}})
		if pm := w.account(alice).Spec.PaymentMethod; pm == nil || !pm.Present {
			t.Errorf("paymentMethod = %+v: the return from Checkout did not fulfil it", pm)
		}
		equal(t, "grants", w.live(), []string{"signup/fpA"})
		// And again: the same answer, nothing more.
		w.call(alice, "GET", "/api/billing/checkout/"+id, "")
		equal(t, "grants", w.live(), []string{"signup/fpA"})
	})
	t.Run("a refused sign-up credit", func(t *testing.T) {
		w := newWorld(t)
		id := w.startCheckout(alice, "")
		w.stripe.CompleteCheckout(id, stripetest.Card{Fingerprint: "fpP", Funding: "prepaid"})
		_, out := w.call(alice, "GET", "/api/billing/checkout/"+id, "")
		equal(t, "body", out, map[string]any{"status": "complete", "kind": "setup", "signupCredit": map[string]any{"state": "refused", "reason": "prepaid"}})
	})
	t.Run("a pack and a plan", func(t *testing.T) {
		w := newWorld(t)
		w.saveCard(alice, cardA)
		for item, kind := range map[string]string{pack20: "purchase", starter: "plan"} {
			id := w.startCheckout(alice, item)
			_, out := w.call(alice, "GET", "/api/billing/checkout/"+id, "")
			equal(t, item+" open", out, map[string]any{"status": "open", "kind": kind, "item": item})
			w.stripe.CompleteCheckout(id, cardA)
			_, out = w.call(alice, "GET", "/api/billing/checkout/"+id, "")
			equal(t, item+" complete", out, map[string]any{"status": "complete", "kind": kind, "item": item})
		}
		if got := len(w.live()); got != 3 {
			t.Errorf("grants = %v, want the sign-up credit, the pack and the plan", w.live())
		}
	})
	t.Run("abandoned", func(t *testing.T) {
		w := newWorld(t)
		id := w.startCheckout(alice, "")
		w.stripe.ExpireCheckout(id)
		_, out := w.call(alice, "GET", "/api/billing/checkout/"+id, "")
		if out["status"] != "expired" {
			t.Errorf("body %v, want expired", out)
		}
	})
	t.Run("a checkout session that belongs to another account", func(t *testing.T) {
		w := newWorld(t)
		w.saveCard(alice, cardA)
		w.saveCard(bob, stripetest.Card{Fingerprint: "fpB", Funding: "credit"})
		id := w.startCheckout(alice, pack20)
		w.stripe.CompleteCheckout(id, cardA)
		before := w.state()
		for _, path := range []string{id, "cs_test_9999", "pi_1", "cs_test_.."} {
			code, out := w.call(bob, "GET", "/api/billing/checkout/"+path, "")
			if code != http.StatusNotFound {
				t.Errorf("%s: status %d, body %v; want 404", path, code, out)
			}
		}
		// Asking did not fulfil it either: that is its owner's to do.
		if after := w.state(); after != before {
			t.Errorf("another account's question changed the state:\nbefore %s\nafter %s", before, after)
		}
	})
}

// A paid Checkout whose reference, metadata and customer do not agree
// grants nothing.
func TestPurchaseMustMatchItsAccount(t *testing.T) {
	for name, change := range map[string]func(*billing.CheckoutSession, billing.Account){
		"another account's reference": func(cs *billing.CheckoutSession, other billing.Account) { cs.ClientReferenceID = other.Name },
		"another account's metadata":  func(cs *billing.CheckoutSession, other billing.Account) { cs.Metadata["account"] = other.Name },
		"another customer":            func(cs *billing.CheckoutSession, other billing.Account) { cs.Customer = other.Spec.StripeCustomerID },
		"an item that is not a pack":  func(cs *billing.CheckoutSession, _ billing.Account) { cs.Metadata["item"] = starter },
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			w.saveCard(alice, cardA)
			other := w.saveCard(bob, stripetest.Card{Fingerprint: "fpB", Funding: "credit"})
			id := w.startCheckout(alice, pack20)
			ev := w.stripe.CompleteCheckout(id, cardA)
			w.stripe.ChangeCheckout(id, func(cs *billing.CheckoutSession) { change(cs, other) })
			w.ok(ev)
			equal(t, "grants", w.live(), []string{"signup/fpA", "signup/fpB"})
		})
	}
}

func TestPortal(t *testing.T) {
	w := newWorld(t)
	code, out := w.call(alice, "POST", "/api/billing/portal", "")
	wantError(t, "never at Stripe", code, out, http.StatusConflict, "no_customer")
	customer := w.saveCard(alice, cardA).Spec.StripeCustomerID
	code, out = w.call(alice, "POST", "/api/billing/portal", "")
	equal(t, "portal", []any{code, out["url"]}, []any{200, "https://billing.stripe.test/p/" + customer + "?return=" + publicURL + "/billing"})
}

func TestPaymentsOff(t *testing.T) {
	rec, r := newRequest("POST", "/api/billing/checkout")
	stripe.PaymentsOff(publicURL).ServeHTTP(rec, r)
	if rec.Code != http.StatusServiceUnavailable || !contains(rec.Body.String(), `"code":"payments_off"`) {
		t.Errorf("status %d, body %s", rec.Code, rec.Body)
	}
}

func TestAutoRechargeSetting(t *testing.T) {
	put := func(w *world, body string) (int, map[string]any) {
		return w.call(alice, "PUT", "/api/billing/auto-recharge", body)
	}
	t.Run("off for the product", func(t *testing.T) {
		w := newWorld(t, func(o *stripe.Options) { o.AutoRecharge = false })
		w.saveCard(alice, cardA)
		code, out := put(w, `{"enabled":true,"agree":true}`)
		wantError(t, "AUTO_RECHARGE off", code, out, http.StatusNotFound, "auto_recharge_off")
		if w.account(alice).Spec.AutoRecharge != nil {
			t.Error("the setting was written")
		}
	})
	t.Run("on needs a card and agreement", func(t *testing.T) {
		w := newWorld(t)
		code, out := put(w, `{"enabled":true,"agree":true}`)
		wantError(t, "no card", code, out, http.StatusPaymentRequired, "payment_method_required")
		w.saveCard(alice, cardA)
		for _, body := range []string{
			`{"enabled":true}`, `{"enabled":true,"agree":false}`, `{}`, `{"agree":true}`,
			`{"enabled":true,"agree":true,"monthlyCapCents":50001}`,
			`{"enabled":true,"agree":true,"monthlyCapCents":1999}`,
			`{"enabled":true,"agree":true,"thresholdMicros":-1}`,
		} {
			if code, out := put(w, body); code != http.StatusBadRequest {
				t.Errorf("%s: status %d, body %v; want 400", body, code, out)
			}
		}
		for _, pack := range []string{"credit-9", "cu_credit_20_v1"} {
			code, out := put(w, `{"enabled":true,"agree":true,"pack":"`+pack+`"}`)
			wantError(t, pack, code, out, http.StatusBadRequest, "unknown_item")
		}
		if w.account(alice).Spec.AutoRecharge != nil {
			t.Error("a refused setting was written")
		}
	})
	t.Run("on, changed, off", func(t *testing.T) {
		w := newWorld(t)
		w.saveCard(alice, cardA)
		code, out := put(w, `{"enabled":true,"agree":true}`)
		equal(t, "the catalogue's defaults", []any{code, out}, []any{200, map[string]any{
			"available": true, "enabled": true, "pack": "credit-20", "thresholdMicros": 2_000_000, "monthlyCapCents": 5000, "chargedCents": 0}})
		a := w.account(alice).Spec.AutoRecharge
		if a.AgreedAt == nil || !a.AgreedAt.Equal(w.clock.Now()) {
			t.Errorf("agreedAt = %v, want now", a.AgreedAt)
		}
		_, out = put(w, `{"enabled":true,"agree":true,"pack":"credit-5","thresholdMicros":500000,"monthlyCapCents":1500}`)
		equal(t, "changed", out, map[string]any{
			"available": true, "enabled": true, "pack": "credit-5", "thresholdMicros": 500_000, "monthlyCapCents": 1500, "chargedCents": 0})
		// Turning it off needs no agreement and keeps the rest.
		_, out = put(w, `{"enabled":false}`)
		equal(t, "off", out, map[string]any{
			"available": true, "enabled": false, "pack": "credit-5", "thresholdMicros": 500_000, "monthlyCapCents": 1500, "chargedCents": 0})
	})
}
