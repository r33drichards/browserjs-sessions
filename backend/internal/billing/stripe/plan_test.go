package stripe_test

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/r33drichards/browserjs-sessions/backend/internal/auth"
	"github.com/r33drichards/browserjs-sessions/backend/internal/billing"
)

func (w *world) changePlan(owner, item string) (int, map[string]any) {
	w.t.Helper()
	return w.call(owner, "POST", "/api/billing/subscription", `{"item":"`+item+`"}`)
}

func planKey(sub string, start time.Time) string {
	return fmt.Sprintf("plan/%s/%d", sub, start.Unix())
}

// An upgrade is now: a new period, the new plan's full credit, and what was
// left of the old period's credit gone. Asked twice, it is done once.
func TestUpgradePlan(t *testing.T) {
	w := newWorld(t)
	w.saveCard(alice, cardA)
	sub := w.subscribe(alice, starter)
	old := planKey(sub, w.clock.Now())
	w.clock.Advance(10 * 24 * time.Hour)
	now := w.clock.Now()

	code, out := w.changePlan(alice, pro)
	equal(t, "the answer", []any{code, out}, []any{200, map[string]any{"change": "upgraded", "item": pro, "effectiveAt": now}})
	spec := w.account(alice).Spec.Subscription
	if spec.PriceLookupKey != pro || !spec.CurrentPeriodStart.Equal(now) || !spec.CurrentPeriodEnd.Equal(now.AddDate(0, 1, 0)) {
		t.Errorf("subscription = %+v, want Pro from now for a month", spec)
	}
	equal(t, "grants", w.live(), []string{planKey(sub, now), "signup/fpA"})
	if g := w.grant(planKey(sub, now)); g.AmountMicros != 44_000_000 {
		t.Errorf("the new period's Grant is %d", g.AmountMicros)
	}
	if g := w.grant(old); g.Revoked == nil || g.Revoked.Reason != "superseded" {
		t.Errorf("the old period's Grant: revoked = %+v, want superseded", g.Revoked)
	}

	// Again, and the event that follows, and the reconcile: nothing more.
	before := w.state()
	code, out = w.changePlan(alice, pro)
	equal(t, "asked again", []any{code, out}, []any{200, map[string]any{"change": "kept", "item": pro}})
	w.ok(w.stripe.SubscriptionEvent("customer.subscription.updated", sub))
	w.reconcile()
	if after := w.state(); after != before {
		t.Errorf("the second request changed the state:\nbefore %s\nafter %s", before, after)
	}
	if n := w.stripe.Calls["UpgradeSubscription"]; n != 1 {
		t.Errorf("%d upgrades at Stripe, want 1", n)
	}
}

// A downgrade waits for the end of the period that is paid for.
func TestDowngradePlan(t *testing.T) {
	w := newWorld(t)
	w.saveCard(alice, cardA)
	sub := w.subscribe(alice, pro)
	start := w.clock.Now()
	end := start.AddDate(0, 1, 0)
	before := w.state()

	for i := 0; i < 2; i++ {
		code, out := w.changePlan(alice, starter)
		equal(t, "the answer", []any{code, out}, []any{200, map[string]any{"change": "scheduled", "item": starter, "effectiveAt": end}})
	}
	if got := w.stripe.Scheduled(sub); got != starter {
		t.Fatalf("scheduled %q, want Starter", got)
	}
	// Nothing changes now: the plan, its credit and its period are as paid.
	if after := w.state(); after != before {
		t.Errorf("a downgrade changed something at once:\nbefore %s\nafter %s", before, after)
	}

	// The period ends: Starter, with Starter's credit.
	w.clock.Advance(end.Sub(start))
	w.ok(w.stripe.EndPeriod(sub))
	if got := w.account(alice).Spec.Subscription.PriceLookupKey; got != starter {
		t.Errorf("after the period: %q, want Starter", got)
	}
	if g := w.grant(planKey(sub, end)); g.AmountMicros != 10_000_000 {
		t.Errorf("the new period's Grant is %d, want Starter's", g.AmountMicros)
	}
}

// Choosing the plan one is on drops a downgrade that was waiting; an
// upgrade drops it too.
func TestScheduledChangeIsDropped(t *testing.T) {
	w := newWorld(t)
	w.saveCard(alice, cardA)
	sub := w.subscribe(alice, pro)
	w.changePlan(alice, starter)
	code, out := w.changePlan(alice, pro)
	equal(t, "the answer", []any{code, out}, []any{200, map[string]any{"change": "kept", "item": pro}})
	if got := w.stripe.Scheduled(sub); got != "" {
		t.Errorf("still scheduled: %q", got)
	}
	w.clock.Advance(31 * 24 * time.Hour)
	w.ok(w.stripe.EndPeriod(sub))
	if got := w.account(alice).Spec.Subscription.PriceLookupKey; got != pro {
		t.Errorf("after the period: %q, want Pro still", got)
	}
}

func TestChangePlanRefusals(t *testing.T) {
	t.Run("an API token, nobody, a blocked account", func(t *testing.T) {
		w := newWorld(t)
		code, out := w.as(auth.User{Subject: alice, Token: &auth.TokenInfo{Name: "ci"}}, "POST", "/api/billing/subscription", `{"item":"`+pro+`"}`)
		wantError(t, "a token", code, out, http.StatusForbidden, "ui_only")
		w.saveCard(alice, cardA)
		w.subscribe(alice, starter)
		w.accounts.Update(ctx, w.account(alice).Name, func(s *billing.AccountSpec) error {
			s.Blocked = &billing.Blocked{Reason: "abuse"}
			return nil
		})
		code, out = w.changePlan(alice, pro)
		wantError(t, "blocked", code, out, http.StatusForbidden, "account_blocked")
		if n := w.stripe.Calls["UpgradeSubscription"]; n != 0 {
			t.Errorf("%d upgrades", n)
		}
	})
	t.Run("no subscription; one that has ended; one that is ending", func(t *testing.T) {
		w := newWorld(t)
		w.saveCard(alice, cardA)
		code, out := w.changePlan(alice, pro)
		wantError(t, "never subscribed", code, out, http.StatusConflict, "not_subscribed")
		sub := w.subscribe(alice, starter)
		end := w.clock.Now().AddDate(0, 1, 0)
		w.ok(w.stripe.ChangeSubscription(sub, func(s *billing.Subscription) { s.CancelAt = &end }))
		code, out = w.changePlan(alice, pro)
		wantError(t, "cancelled at the period's end", code, out, http.StatusConflict, "subscription_ending")
		// Ended, and no event said so: Stripe is read, not the Account.
		w.stripe.ChangeSubscription(sub, func(s *billing.Subscription) { s.Status, s.CancelAt = "canceled", nil })
		code, out = w.changePlan(alice, pro)
		wantError(t, "ended", code, out, http.StatusConflict, "not_subscribed")
		if n := w.stripe.Calls["UpgradeSubscription"] + w.stripe.Calls["SchedulePrice"]; n != 0 {
			t.Errorf("%d changes at Stripe", n)
		}
	})
	t.Run("a payment that is owed", func(t *testing.T) {
		w := newWorld(t)
		w.saveCard(alice, cardA)
		sub := w.subscribe(alice, starter)
		w.stripe.ChangeSubscription(sub, func(s *billing.Subscription) { s.Status = "past_due" })
		code, out := w.changePlan(alice, pro)
		wantError(t, "past_due", code, out, http.StatusPaymentRequired, "payment_failed")
	})
	t.Run("what is not a plan on sale", func(t *testing.T) {
		w := newWorld(t)
		w.saveCard(alice, cardA)
		w.subscribe(alice, starter)
		for _, item := range []string{pack20, "cu_scale_monthly_v1", "cu_nothing_v1", ""} {
			code, out := w.changePlan(alice, item)
			wantError(t, item, code, out, http.StatusBadRequest, "unknown_item")
		}
	})
	t.Run("the card refuses the upgrade", func(t *testing.T) {
		w := newWorld(t)
		w.saveCard(alice, cardA)
		w.subscribe(alice, starter)
		before := w.state()
		w.stripe.Decline = "card_declined"
		code, out := w.changePlan(alice, pro)
		wantError(t, "declined", code, out, http.StatusPaymentRequired, "payment_failed")
		if after := w.state(); after != before {
			t.Errorf("a refused upgrade changed the state:\nbefore %s\nafter %s", before, after)
		}
	})
	t.Run("Stripe cannot be reached", func(t *testing.T) {
		w := newWorld(t)
		w.saveCard(alice, cardA)
		w.subscribe(alice, starter)
		before := w.state()
		w.stripe.Err = errors.New("timeout")
		code, out := w.changePlan(alice, pro)
		wantError(t, "down", code, out, http.StatusBadGateway, "stripe_unavailable")
		w.stripe.Err = nil
		if after := w.state(); after != before {
			t.Errorf("the state changed:\nbefore %s\nafter %s", before, after)
		}
	})
}
