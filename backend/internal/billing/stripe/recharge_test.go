package stripe_test

import (
	"errors"
	"github.com/r33drichards/browserjs-sessions/backend/internal/billing/stripe/stripetest"
	"testing"
	"time"

	"github.com/r33drichards/browserjs-sessions/backend/internal/billing"
	"github.com/r33drichards/browserjs-sessions/backend/internal/billing/stripe"
)

func (w *world) recharge(owner string) {
	w.t.Helper()
	if err := w.svc.Recharge(ctx, w.account(owner).Name, w.balances[owner]); err != nil {
		w.t.Fatalf("Recharge: %v", err)
	}
}

// balance is what the balance pass reads for owner from now on.
func (w *world) balance(owner string, micros int64) { w.balances[owner] = micros }

func TestAutoRechargeAtTheThreshold(t *testing.T) {
	w := newWorld(t)
	acct := w.saveCard(alice, cardA)
	w.rechargeOn(alice)

	// At the threshold, and above it: nothing.
	for _, micros := range []int64{2_000_000, 7_000_000} {
		w.balance(alice, micros)
		w.recharge(alice)
	}
	if got := w.stripe.Calls["CreateRecharge"]; got != 0 {
		t.Fatalf("%d charges at or above the threshold", got)
	}
	// Under it: one charge of the pack, on the saved card, and its Grant.
	w.balance(alice, 1_999_999)
	w.recharge(alice)
	made := w.stripe.PaymentIntentsMade()
	if len(made) != 1 {
		t.Fatalf("%d PaymentIntents, want 1", len(made))
	}
	pi := made[0]
	equal(t, "the charge", []any{pi.Customer, pi.AmountCents, pi.Metadata},
		[]any{acct.Spec.StripeCustomerID, 2000, map[string]string{
			"account": acct.Name, "kind": "recharge", "item": pack20, "month": "2026-10", "seq": "1"}})
	equal(t, "grants", w.live(), []string{"recharge/" + pi.ID, "signup/fpA"})
	a := w.account(alice).Spec.AutoRecharge
	equal(t, "autoRecharge", []any{a.Enabled, a.Month, a.Seq, a.ChargedCents, a.Last.Status, a.Last.PaymentIntent},
		[]any{true, "2026-10", 1, 2000, "succeeded", pi.ID})

	// The balance has not caught up yet (the operator's next tick): no
	// second charge inside ten minutes.
	w.clock.Advance(9 * time.Minute)
	w.balance(alice, 1_000_000)
	w.recharge(alice)
	if got := len(w.stripe.PaymentIntentsMade()); got != 1 {
		t.Fatalf("%d PaymentIntents nine minutes later, want 1", got)
	}
	// Still under it after ten: the next one, with the next seq.
	w.clock.Advance(2 * time.Minute)
	w.balance(alice, 1_000_000)
	w.recharge(alice)
	a = w.account(alice).Spec.AutoRecharge
	equal(t, "the second", []any{len(w.stripe.PaymentIntentsMade()), a.Seq, a.ChargedCents}, []any{2, 2, 4000})
}

func TestAutoRechargeUsesTheDefaultCardElseTheNewest(t *testing.T) {
	w := newWorld(t)
	customer := w.saveCard(alice, cardA).Spec.StripeCustomerID
	w.clock.Advance(time.Minute)
	newest, ev := w.stripe.AttachCard(customer, stripetest.Card{Fingerprint: "fpB", Funding: "credit"})
	w.ok(ev)
	w.rechargeOn(alice)
	w.recharge(alice)
	if got := w.stripe.RechargeCards(); len(got) != 1 || got[0] != newest {
		t.Fatalf("charged %v, want the newest card %s", got, newest)
	}
	oldest := w.account(alice).Spec.PaymentMethod.IDs[0]
	w.ok(w.stripe.SetDefault(customer, oldest))
	w.clock.Advance(11 * time.Minute)
	w.balance(alice, 0)
	w.recharge(alice)
	if got := w.stripe.RechargeCards(); len(got) != 2 || got[1] != oldest {
		t.Fatalf("charged %v, want the default card %s second", got, oldest)
	}
}

func TestAutoRechargeAtTheCap(t *testing.T) {
	w := newWorld(t)
	w.saveCard(alice, cardA)
	w.rechargeOn(alice) // $20 a time, at most $50 a month
	for i := 0; i < 2; i++ {
		w.balance(alice, 0)
		w.recharge(alice)
		w.clock.Advance(11 * time.Minute)
	}
	// A third would make $60.
	w.balance(alice, 0)
	w.recharge(alice)
	a := w.account(alice).Spec.AutoRecharge
	equal(t, "at the cap", []any{len(w.stripe.PaymentIntentsMade()), a.Enabled, a.DisabledReason, a.ChargedCents},
		[]any{2, false, "cap-reached", 4000})
	// It stays off for the month.
	w.clock.Advance(24 * time.Hour)
	w.balance(alice, 0)
	w.recharge(alice)
	if got := len(w.stripe.PaymentIntentsMade()); got != 2 {
		t.Fatalf("%d PaymentIntents, want 2", got)
	}
	// The month turns: it goes on, counting from nothing.
	w.clock.Advance(31 * 24 * time.Hour)
	w.balance(alice, 0)
	w.recharge(alice)
	a = w.account(alice).Spec.AutoRecharge
	equal(t, "the next month", []any{len(w.stripe.PaymentIntentsMade()), a.Enabled, a.DisabledReason, a.Month, a.Seq, a.ChargedCents},
		[]any{3, true, "", "2026-11", 1, 2000})
}

// A user who raises the cap turns it on again themselves.
func TestAutoRechargeCapRaised(t *testing.T) {
	w := newWorld(t)
	w.saveCard(alice, cardA)
	w.rechargeOn(alice)
	for i := 0; i < 3; i++ {
		w.balance(alice, 0)
		w.recharge(alice)
		w.clock.Advance(11 * time.Minute)
	}
	if a := w.account(alice).Spec.AutoRecharge; a.Enabled {
		t.Fatal("still on at the cap")
	}
	if code, out := w.call(alice, "PUT", "/api/billing/auto-recharge", `{"enabled":true,"agree":true,"monthlyCapCents":10000}`); code != 200 || out["chargedCents"] != float64(4000) {
		t.Fatalf("status %d, body %v", code, out)
	}
	w.balance(alice, 0)
	w.recharge(alice)
	a := w.account(alice).Spec.AutoRecharge
	equal(t, "after the cap was raised", []any{len(w.stripe.PaymentIntentsMade()), a.Enabled, a.Seq, a.ChargedCents}, []any{3, true, 3, 6000})
}

func TestAutoRechargeOnFailure(t *testing.T) {
	for decline, reason := range map[string]string{
		"card_declined":           "payment-failed",
		"authentication_required": "authentication-required",
	} {
		t.Run(decline, func(t *testing.T) {
			w := newWorld(t)
			w.saveCard(alice, cardA)
			w.rechargeOn(alice)
			w.stripe.Decline = decline
			w.recharge(alice)
			made := w.stripe.PaymentIntentsMade()
			a := w.account(alice).Spec.AutoRecharge
			equal(t, "after the failure", []any{len(made), a.Enabled, a.DisabledReason, a.ChargedCents, a.Last.Status, a.Last.PaymentIntent},
				[]any{1, false, reason, 0, "failed", made[0].ID})
			equal(t, "grants", w.live(), []string{"signup/fpA"})

			// It is not retried, whatever the card does now.
			w.stripe.Decline = ""
			w.clock.Advance(time.Hour)
			w.balance(alice, 0)
			w.recharge(alice)
			if got := len(w.stripe.PaymentIntentsMade()); got != 1 {
				t.Fatalf("%d PaymentIntents: a failed charge was retried", got)
			}
			// The user turns it on again; the old failure, read again by the
			// reconcile or sent again by Stripe, does not turn it off.
			if code, _ := w.call(alice, "PUT", "/api/billing/auto-recharge", `{"enabled":true,"agree":true}`); code != 200 {
				t.Fatalf("status %d", code)
			}
			w.ok(w.stripe.PaymentIntentEvent("payment_intent.payment_failed", made[0].ID))
			w.svc.ReconcilePurchases(ctx, w.clock.Now().Add(-35*24*time.Hour))
			if a := w.account(alice).Spec.AutoRecharge; !a.Enabled || a.DisabledReason != "" {
				t.Errorf("autoRecharge = %+v: an old failure turned it off again", a)
			}
			w.recharge(alice)
			a = w.account(alice).Spec.AutoRecharge
			equal(t, "the next attempt", []any{len(w.stripe.PaymentIntentsMade()), a.Seq, a.Last.Status, a.ChargedCents}, []any{2, 2, "succeeded", 2000})
		})
	}
}

func TestAutoRechargeDoesNothing(t *testing.T) {
	// Each starts from an account that would be charged, and takes one
	// condition away.
	for name, c := range map[string]struct {
		opt    func(*stripe.Options)
		change func(w *world)
		reason string // what it is turned off for; "" for left as it was
	}{
		"AUTO_RECHARGE is off": {opt: func(o *stripe.Options) { o.AutoRecharge = false }},
		"the user turned it off": {change: func(w *world) {
			w.call(alice, "PUT", "/api/billing/auto-recharge", `{"enabled":false}`)
		}},
		"the balance is at the threshold": {change: func(w *world) { w.balance(alice, 2_000_000) }},
		"the account is blocked": {change: func(w *world) {
			w.accounts.Update(ctx, w.account(alice).Name, func(s *billing.AccountSpec) error {
				s.Blocked = &billing.Blocked{Reason: "abuse"}
				return nil
			})
		}},
		"the card was removed": {change: func(w *world) {
			w.ok(w.stripe.DetachCard(w.account(alice).Spec.PaymentMethod.IDs[0]))
		}, reason: "no-card"},
		"the card was removed and no event said so": {change: func(w *world) {
			w.stripe.DetachCard(w.account(alice).Spec.PaymentMethod.IDs[0])
		}, reason: "no-card"},
		"the pack is no longer sold": {change: func(w *world) {
			w.accounts.Update(ctx, w.account(alice).Name, func(s *billing.AccountSpec) error {
				s.AutoRecharge.Pack = "credit-9"
				return nil
			})
		}},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t, func(o *stripe.Options) {
				if c.opt != nil {
					c.opt(o)
				}
			})
			w.saveCard(alice, cardA)
			now := w.clock.Now()
			if _, err := w.accounts.Update(ctx, w.account(alice).Name, func(s *billing.AccountSpec) error {
				s.AutoRecharge = &billing.AutoRecharge{Enabled: true, Pack: "credit-20", ThresholdMicros: 2_000_000, MonthlyCapCents: 5000, AgreedAt: &now}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			w.balance(alice, 1_000_000)
			if c.change != nil {
				c.change(w)
			}
			before := w.account(alice).Spec.AutoRecharge
			w.recharge(alice)
			if got := w.stripe.Calls["CreateRecharge"]; got != 0 {
				t.Fatalf("%d charges", got)
			}
			after := w.account(alice).Spec.AutoRecharge
			if c.reason == "" {
				equal(t, "autoRecharge", after, before)
			} else if after.Enabled || after.DisabledReason != c.reason {
				t.Errorf("autoRecharge = %+v, want off for %s", after, c.reason)
			}
			equal(t, "grants", w.live(), []string{"signup/fpA"})
		})
	}
}

func TestAutoRechargeStripeDown(t *testing.T) {
	w := newWorld(t)
	w.saveCard(alice, cardA)
	acct := w.rechargeOn(alice)
	w.stripe.Err = errors.New("timeout")
	if err := w.svc.Recharge(ctx, acct.Name, 1_000_000); err == nil {
		t.Fatal("no error")
	}
	// Nothing was written: the card could not even be read.
	if a := w.account(alice).Spec.AutoRecharge; a.Seq != 0 || a.Last != nil || !a.Enabled {
		t.Errorf("autoRecharge = %+v", a)
	}
}

// A charge that does not carry its attempt (metadata.month, metadata.seq)
// is matched to the pending attempt by when it was made: an old one's
// outcome is not written over a newer attempt.
func TestAutoRechargeWithoutAttemptMetadata(t *testing.T) {
	w := newWorld(t)
	w.stripe.NoAttemptMetadata = true
	w.saveCard(alice, cardA)
	w.rechargeOn(alice)
	w.stripe.Decline = "card_declined"
	w.recharge(alice)
	failed := w.stripe.PaymentIntentsMade()[0].ID
	if a := w.account(alice).Spec.AutoRecharge; a.Enabled || a.Last.Status != "failed" {
		t.Fatalf("autoRecharge = %+v, want off after the failure", a)
	}

	// Turned on again; the next attempt's answer is lost.
	w.stripe.Decline = ""
	w.clock.Advance(11 * time.Minute)
	if code, _ := w.call(alice, "PUT", "/api/billing/auto-recharge", `{"enabled":true,"agree":true}`); code != 200 {
		t.Fatalf("status %d", code)
	}
	w.stripe.LoseRechargeResponse = true
	if err := w.svc.Recharge(ctx, w.account(alice).Name, 0); err == nil {
		t.Fatal("the lost answer was not an error")
	}
	w.stripe.LoseRechargeResponse = false

	// The old failure, sent again, is not this attempt's.
	w.ok(w.stripe.PaymentIntentEvent("payment_intent.payment_failed", failed))
	if a := w.account(alice).Spec.AutoRecharge; !a.Enabled || a.Last.Status != "pending" {
		t.Fatalf("autoRecharge = %+v: an old failure was written over a pending attempt", a)
	}
	// This attempt's own event is.
	second := w.stripe.PaymentIntentsMade()[1].ID
	w.ok(w.stripe.PaymentIntentEvent("payment_intent.succeeded", second))
	a := w.account(alice).Spec.AutoRecharge
	equal(t, "autoRecharge", []any{a.Enabled, a.Seq, a.ChargedCents, a.Last.Status, a.Last.PaymentIntent}, []any{true, 2, 2000, "succeeded", second})
	equal(t, "grants", w.live(), []string{"recharge/" + second, "signup/fpA"})
}
