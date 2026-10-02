package stripe_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/r33drichards/computer-use/backend/internal/billing"
	"github.com/r33drichards/computer-use/backend/internal/billing/stripe"
	"github.com/r33drichards/computer-use/backend/internal/billing/stripe/stripetest"
)

var ctx = context.Background()

// subscribe is a subscription Checkout completed and its event handled; it
// answers the subscription's ID.
func (w *world) subscribe(owner, item string) string {
	w.t.Helper()
	id := w.startCheckout(owner, item)
	w.ok(w.stripe.CompleteCheckout(id, cardA))
	cs, _ := w.stripe.Checkout(ctx, id)
	return cs.Subscription
}

// buy is a pack bought at a Checkout and its event handled; it answers the
// Checkout Session's ID and the PaymentIntent's.
func (w *world) buy(owner, item string) (checkout, paymentIntent string) {
	w.t.Helper()
	id := w.startCheckout(owner, item)
	w.ok(w.stripe.CompleteCheckout(id, cardA))
	cs, _ := w.stripe.Checkout(ctx, id)
	return id, cs.PaymentIntent
}

// rechargeOn turns the owner's auto-recharge on with the catalogue's
// defaults ($20 when under $2, at most $50 a month) and puts their balance
// under the threshold.
func (w *world) rechargeOn(owner string) billing.Account {
	w.t.Helper()
	if code, out := w.call(owner, "PUT", "/api/billing/auto-recharge", `{"enabled":true,"agree":true}`); code != http.StatusOK {
		w.t.Fatalf("auto-recharge: status %d: %v", code, out)
	}
	acct := w.account(owner)
	w.balance(owner, 1_000_000)
	return acct
}

// lostRecharge is an automatic charge made at Stripe whose answer never
// came back: the Account still says pending. It answers the PaymentIntent.
func (w *world) lostRecharge(owner string) string {
	w.t.Helper()
	acct := w.rechargeOn(owner)
	w.stripe.LoseRechargeResponse = true
	if err := w.svc.Recharge(ctx, acct.Name, 1_000_000); err == nil {
		w.t.Fatal("the lost answer was not an error")
	}
	w.stripe.LoseRechargeResponse = false
	for _, pi := range w.stripe.PaymentIntentsMade() {
		if pi.Metadata["kind"] == billing.KindRecharge {
			return pi.ID
		}
	}
	w.t.Fatal("no automatic charge was made")
	return ""
}

func (w *world) renew(sub string, change func(*billing.Subscription)) {
	w.clock.Advance(31 * 24 * time.Hour)
	now := w.clock.Now()
	w.stripe.ChangeSubscription(sub, func(s *billing.Subscription) {
		s.CurrentPeriodStart, s.CurrentPeriodEnd = s.CurrentPeriodEnd, s.CurrentPeriodEnd.AddDate(0, 1, 0)
		s.LatestInvoice = fmt.Sprintf("in_%d", now.Unix())
		change(s)
	})
}

// eventCase is one row of the event table of stripe.md: the state an event
// is about, the event, and what handling it must leave.
type eventCase struct {
	name    string
	arrange func(w *world) stripetest.Event
	check   func(t *testing.T, w *world)
}

func eventCases() []eventCase {
	unchanged := func(arrange func(w *world) stripetest.Event) (func(w *world) stripetest.Event, func(*testing.T, *world)) {
		var before string
		return func(w *world) stripetest.Event {
				ev := arrange(w)
				before = w.state()
				return ev
			}, func(t *testing.T, w *world) {
				if after := w.state(); after != before {
					t.Errorf("the event changed something:\nbefore %s\nafter %s", before, after)
				}
			}
	}
	hasCard := func(t *testing.T, w *world) {
		t.Helper()
		pm := w.account(alice).Spec.PaymentMethod
		if pm == nil || !pm.Present || pm.RemovedAt != nil || len(pm.IDs) == 0 {
			t.Errorf("paymentMethod = %+v, want present", pm)
		}
	}
	// An account with a customer and a card Stripe has and the Account has
	// not heard of.
	unseenCard := func(w *world) (customer, pm string) {
		w.startCheckout(alice, "")
		customer = w.account(alice).Spec.StripeCustomerID
		pm, _ = w.stripe.AttachCard(customer, cardA)
		return customer, pm
	}
	var cases []eventCase
	add := func(name string, arrange func(w *world) stripetest.Event, check func(*testing.T, *world)) {
		cases = append(cases, eventCase{name, arrange, check})
	}

	add("checkout.session.completed, setup",
		func(w *world) stripetest.Event { return w.stripe.CompleteCheckout(w.startCheckout(alice, ""), cardA) },
		func(t *testing.T, w *world) {
			hasCard(t, w)
			acct := w.account(alice)
			if sc := acct.Spec.SignupCredit; sc == nil || sc.State != billing.SignupGranted {
				t.Errorf("signupCredit = %+v, want granted", sc)
			}
			equal(t, "grants", w.live(), []string{"signup/fpA"})
			g := w.grant("signup/fpA")
			equal(t, "the sign-up Grant", []any{g.Account, g.Source, g.AmountMicros, g.ValidFrom, *g.ExpiresAt},
				[]any{acct.Name, "signup", 5_000_000, w.clock.Now(), w.clock.Now().Add(90 * 24 * time.Hour)})
		})

	{
		var id, pi string
		check := func(t *testing.T, w *world) {
			hasCard(t, w)
			equal(t, "grants", w.live(), []string{"purchase/" + id, "signup/fpA"})
			g := w.grant("purchase/" + id)
			equal(t, "the pack's Grant",
				[]any{g.Account, g.Source, g.AmountMicros, g.Item, *g.ExpiresAt, g.Ref},
				[]any{w.account(alice).Name, "purchase", 20_000_000, pack20, w.clock.Now().Add(365 * 24 * time.Hour),
					billing.GrantRef{CheckoutSession: id, PaymentIntent: pi}})
		}
		paid := func(w *world) stripetest.Event {
			w.saveCard(alice, cardA)
			id = w.startCheckout(alice, pack20)
			ev := w.stripe.CompleteCheckout(id, cardA)
			cs, _ := w.stripe.Checkout(ctx, id)
			pi = cs.PaymentIntent
			return ev
		}
		add("checkout.session.completed, payment", paid, check)
		add("checkout.session.async_payment_succeeded", func(w *world) stripetest.Event {
			paid(w)
			return w.stripe.Event("checkout.session.async_payment_succeeded", map[string]any{"id": id, "object": "checkout.session"})
		}, check)
	}

	{
		arrange, check := unchanged(func(w *world) stripetest.Event {
			w.saveCard(alice, cardA)
			id := w.startCheckout(alice, pack20)
			return w.stripe.Event("checkout.session.async_payment_failed", map[string]any{"id": id, "object": "checkout.session"})
		})
		add("checkout.session.async_payment_failed", arrange, check)
	}

	add("setup_intent.succeeded", func(w *world) stripetest.Event {
		customer, _ := unseenCard(w)
		return w.stripe.Event("setup_intent.succeeded", map[string]any{"id": "seti_1", "object": "setup_intent", "customer": customer})
	}, hasCard)
	for _, typ := range []string{"payment_method.attached", "payment_method.updated", "payment_method.automatically_updated"} {
		add(typ, func(w *world) stripetest.Event {
			customer, pm := unseenCard(w)
			return w.stripe.Event(typ, map[string]any{"id": pm, "object": "payment_method", "customer": customer})
		}, hasCard)
	}

	add("payment_method.detached", func(w *world) stripetest.Event {
		return w.stripe.DetachCard(w.saveCard(alice, cardA).Spec.PaymentMethod.IDs[0])
	}, func(t *testing.T, w *world) {
		pm := w.account(alice).Spec.PaymentMethod
		if pm.Present || pm.RemovedAt == nil || len(pm.IDs) != 0 {
			t.Errorf("paymentMethod = %+v, want absent with removedAt", pm)
		}
		equal(t, "grants: the credit is kept", w.live(), []string{"signup/fpA"})
	})

	{
		var second string
		add("customer.updated", func(w *world) stripetest.Event {
			customer := w.saveCard(alice, cardA).Spec.StripeCustomerID
			pm, ev := w.stripe.AttachCard(customer, stripetest.Card{Fingerprint: "fpB", Funding: "debit"})
			w.ok(ev)
			second = pm
			return w.stripe.SetDefault(customer, pm)
		}, func(t *testing.T, w *world) {
			pm := w.account(alice).Spec.PaymentMethod
			if pm.Default != second || len(pm.IDs) != 2 {
				t.Errorf("paymentMethod = %+v, want default %s of two", pm, second)
			}
			equal(t, "grants", w.live(), []string{"signup/fpA"})
		})
	}

	add("customer.deleted", func(w *world) stripetest.Event {
		return w.stripe.DeleteCustomer(w.saveCard(alice, cardA).Spec.StripeCustomerID)
	}, func(t *testing.T, w *world) {
		if pm := w.account(alice).Spec.PaymentMethod; pm.Present || pm.RemovedAt == nil {
			t.Errorf("paymentMethod = %+v, want absent with removedAt", pm)
		}
	})

	{
		var sub string
		var start time.Time
		add("customer.subscription.created", func(w *world) stripetest.Event {
			w.saveCard(alice, cardA)
			id := w.startCheckout(alice, starter)
			w.stripe.CompleteCheckout(id, cardA) // its event is not delivered
			cs, _ := w.stripe.Checkout(ctx, id)
			sub, start = cs.Subscription, w.clock.Now()
			return w.stripe.SubscriptionEvent("customer.subscription.created", sub)
		}, func(t *testing.T, w *world) {
			got := w.account(alice).Spec.Subscription
			if got == nil || got.ID != sub || got.Status != "active" || got.PriceLookupKey != starter ||
				!got.CurrentPeriodStart.Equal(start) || !got.CurrentPeriodEnd.Equal(start.AddDate(0, 1, 0)) {
				t.Errorf("subscription = %+v", got)
			}
			key := fmt.Sprintf("plan/%s/%d", sub, start.Unix())
			equal(t, "grants", w.live(), []string{key, "signup/fpA"})
			g := w.grant(key)
			equal(t, "the plan's Grant", []any{g.Source, g.AmountMicros, g.Item, g.ValidFrom, *g.ExpiresAt, g.Ref.Subscription},
				[]any{"plan", 10_000_000, starter, start, start.AddDate(0, 1, 0), sub})
		})
	}

	{
		var old, upgraded string
		add("customer.subscription.updated, an upgrade", func(w *world) stripetest.Event {
			w.saveCard(alice, cardA)
			sub := w.subscribe(alice, starter)
			old = fmt.Sprintf("plan/%s/%d", sub, w.clock.Now().Unix())
			w.clock.Advance(10 * 24 * time.Hour)
			now := w.clock.Now()
			upgraded = fmt.Sprintf("plan/%s/%d", sub, now.Unix())
			// The portal's upgrade: a new period from now, paid at once.
			return w.stripe.ChangeSubscription(sub, func(s *billing.Subscription) {
				s.PriceLookupKey, s.CurrentPeriodStart, s.CurrentPeriodEnd = pro, now, now.AddDate(0, 1, 0)
			})
		}, func(t *testing.T, w *world) {
			equal(t, "grants", w.live(), []string{upgraded, "signup/fpA"})
			if g := w.grant(old); g.Revoked == nil || g.Revoked.Reason != stripe.RevokedSuperseded {
				t.Errorf("the old period's Grant: revoked = %+v, want superseded", g.Revoked)
			}
			if g := w.grant(upgraded); g.AmountMicros != 44_000_000 {
				t.Errorf("the upgrade's Grant is %d", g.AmountMicros)
			}
			if got := w.account(alice).Spec.Subscription.PriceLookupKey; got != pro {
				t.Errorf("priceLookupKey = %q", got)
			}
		})
	}

	for _, c := range []struct{ typ, status string }{
		{"customer.subscription.deleted", "canceled"},
		{"customer.subscription.paused", "paused"},
		{"customer.subscription.resumed", "active"},
	} {
		var key string
		add(c.typ, func(w *world) stripetest.Event {
			w.saveCard(alice, cardA)
			sub := w.subscribe(alice, starter)
			key = fmt.Sprintf("plan/%s/%d", sub, w.clock.Now().Unix())
			w.stripe.ChangeSubscription(sub, func(s *billing.Subscription) { s.Status = c.status })
			return w.stripe.SubscriptionEvent(c.typ, sub)
		}, func(t *testing.T, w *world) {
			if got := w.account(alice).Spec.Subscription.Status; got != c.status {
				t.Errorf("status = %q, want %q", got, c.status)
			}
			// The period already paid for runs to its expiry.
			equal(t, "grants", w.live(), []string{key, "signup/fpA"})
		})
	}

	{
		var first, second string
		add("invoice.paid, a renewal", func(w *world) stripetest.Event {
			w.saveCard(alice, cardA)
			sub := w.subscribe(alice, starter)
			first = fmt.Sprintf("plan/%s/%d", sub, w.clock.Now().Unix())
			w.renew(sub, func(*billing.Subscription) {})
			s, _ := w.stripe.Subscription(ctx, sub)
			second = fmt.Sprintf("plan/%s/%d", sub, s.CurrentPeriodStart.Unix())
			return w.stripe.InvoiceEvent("invoice.paid", sub)
		}, func(t *testing.T, w *world) {
			// The old period's Grant has expired; it is not superseded.
			equal(t, "grants", w.live(), []string{first, second, "signup/fpA"})
			if g := w.grant(second); !g.ValidFrom.Equal(*w.grant(first).ExpiresAt) {
				t.Errorf("the new period starts %v, the old ended %v", g.ValidFrom, w.grant(first).ExpiresAt)
			}
		})
	}

	{
		var first string
		add("invoice.payment_failed, a renewal", func(w *world) stripetest.Event {
			w.saveCard(alice, cardA)
			sub := w.subscribe(alice, starter)
			first = fmt.Sprintf("plan/%s/%d", sub, w.clock.Now().Unix())
			w.renew(sub, func(s *billing.Subscription) { s.Status, s.LatestInvoiceStatus = "past_due", "open" })
			return w.stripe.InvoiceEvent("invoice.payment_failed", sub)
		}, func(t *testing.T, w *world) {
			if got := w.account(alice).Spec.Subscription.Status; got != "past_due" {
				t.Errorf("status = %q, want past_due", got)
			}
			equal(t, "grants: none for a period that is not paid", w.live(), []string{first, "signup/fpA"})
		})
	}

	{
		arrange, check := unchanged(func(w *world) stripetest.Event {
			customer := w.saveCard(alice, cardA).Spec.StripeCustomerID
			return w.stripe.Event("invoice.paid", map[string]any{"id": "in_1", "object": "invoice", "customer": customer, "parent": nil})
		})
		add("invoice.paid, not a subscription's", arrange, check)
	}

	{
		var pi string
		add("payment_intent.succeeded, an automatic charge", func(w *world) stripetest.Event {
			w.saveCard(alice, cardA)
			pi = w.lostRecharge(alice)
			return w.stripe.PaymentIntentEvent("payment_intent.succeeded", pi)
		}, func(t *testing.T, w *world) {
			equal(t, "grants", w.live(), []string{"recharge/" + pi, "signup/fpA"})
			g := w.grant("recharge/" + pi)
			equal(t, "the Grant", []any{g.Source, g.AmountMicros, g.Ref.PaymentIntent}, []any{"purchase", 20_000_000, pi})
			a := w.account(alice).Spec.AutoRecharge
			equal(t, "autoRecharge", []any{a.Enabled, a.Seq, a.ChargedCents, a.Last.Status, a.Last.PaymentIntent},
				[]any{true, 1, 2000, "succeeded", pi})
		})
	}
	{
		add("payment_intent.payment_failed, an automatic charge", func(w *world) stripetest.Event {
			w.saveCard(alice, cardA)
			w.stripe.Decline = "card_declined"
			return w.stripe.PaymentIntentEvent("payment_intent.payment_failed", w.lostRecharge(alice))
		}, func(t *testing.T, w *world) {
			equal(t, "grants", w.live(), []string{"signup/fpA"})
			a := w.account(alice).Spec.AutoRecharge
			equal(t, "autoRecharge", []any{a.Enabled, a.DisabledReason, a.ChargedCents, a.Last.Status},
				[]any{false, "payment-failed", 0, "failed"})
		})
	}
	{
		arrange, check := unchanged(func(w *world) stripetest.Event {
			w.saveCard(alice, cardA)
			_, pi := w.buy(alice, pack20)
			return w.stripe.PaymentIntentEvent("payment_intent.succeeded", pi)
		})
		add("payment_intent.succeeded, a checkout's", arrange, check)
	}

	{
		var id string
		revoked := func(reason string) func(t *testing.T, w *world) {
			return func(t *testing.T, w *world) {
				equal(t, "grants", w.live(), []string{"signup/fpA"})
				if g := w.grant("purchase/" + id); g.Revoked == nil || g.Revoked.Reason != reason {
					t.Errorf("the pack's Grant: revoked = %+v, want %s", g.Revoked, reason)
				}
			}
		}
		bought := func(w *world) (pi string) {
			w.saveCard(alice, cardA)
			id, pi = w.buy(alice, pack20)
			return pi
		}
		add("refund.created, a pack", func(w *world) stripetest.Event { return w.stripe.RefundEvent(bought(w)) }, revoked("refund"))
		add("charge.refunded, a pack", func(w *world) stripetest.Event { return w.stripe.ChargeRefundedEvent(bought(w)) }, revoked("refund"))

		blocked := func(want bool) func(t *testing.T, w *world) {
			return func(t *testing.T, w *world) {
				revoked("dispute")(t, w)
				b := w.account(alice).Spec.Blocked
				if want != (b != nil && b.Reason == "dispute") {
					t.Errorf("blocked = %+v, want blocked for the dispute: %v", b, want)
				}
			}
		}
		add("charge.dispute.created", func(w *world) stripetest.Event {
			return w.stripe.DisputeEvent("charge.dispute.created", bought(w), "needs_response")
		}, blocked(true))
		for status, still := range map[string]bool{"won": false, "lost": true} {
			add("charge.dispute.closed, "+status, func(w *world) stripetest.Event {
				pi := bought(w)
				w.ok(w.stripe.DisputeEvent("charge.dispute.created", pi, "needs_response"))
				return w.stripe.DisputeEvent("charge.dispute.closed", pi, status)
			}, blocked(still))
		}
	}

	{
		var key string
		add("charge.refunded, a subscription's", func(w *world) stripetest.Event {
			w.saveCard(alice, cardA)
			sub := w.subscribe(alice, starter)
			key = fmt.Sprintf("plan/%s/%d", sub, w.clock.Now().Unix())
			return w.stripe.ChargeRefundedEvent(w.stripe.SubscriptionPayment(sub))
		}, func(t *testing.T, w *world) {
			equal(t, "grants", w.live(), []string{"signup/fpA"})
			if g := w.grant(key); g.Revoked == nil || g.Revoked.Reason != "refund" {
				t.Errorf("the plan's Grant: revoked = %+v, want refund", g.Revoked)
			}
		})
	}

	{
		arrange, check := unchanged(func(w *world) stripetest.Event {
			customer := w.saveCard(alice, cardA).Spec.StripeCustomerID
			return w.stripe.Event("customer.created", map[string]any{"id": customer, "object": "customer"})
		})
		add("an event that is not in the table", arrange, check)
	}
	return cases
}

// Every row of the event table of stripe.md.
func TestEventTable(t *testing.T) {
	for _, c := range eventCases() {
		t.Run(c.name, func(t *testing.T) {
			w := newWorld(t)
			w.ok(c.arrange(w))
			c.check(t, w)
		})
	}
}

// Scenario 4 of testing.md: replay and disorder.
func TestWebhookIdempotency(t *testing.T) {
	t.Run("every event posted twice", func(t *testing.T) {
		for _, c := range eventCases() {
			t.Run(c.name, func(t *testing.T) {
				w := newWorld(t)
				ev := c.arrange(w)
				w.ok(ev)
				first := w.state()
				w.ok(ev)
				if second := w.state(); second != first {
					t.Errorf("the second delivery changed the state:\nfirst %s\nsecond %s", first, second)
				}
				c.check(t, w)
			})
		}
	})

	t.Run("a subscription's events in reverse order", func(t *testing.T) {
		w := newWorld(t)
		w.saveCard(alice, cardA)
		id := w.startCheckout(alice, starter)
		completed := w.stripe.CompleteCheckout(id, cardA)
		cs, _ := w.stripe.Checkout(ctx, id)
		sub := cs.Subscription
		w.ok(w.stripe.InvoiceEvent("invoice.paid", sub))
		w.ok(w.stripe.SubscriptionEvent("customer.subscription.updated", sub))
		w.ok(w.stripe.SubscriptionEvent("customer.subscription.created", sub))
		w.ok(completed)
		equal(t, "grants: one plan Grant for the period", w.live(),
			[]string{fmt.Sprintf("plan/%s/%d", sub, w.clock.Now().Unix()), "signup/fpA"})
		if got := w.account(alice).Spec.Subscription; got == nil || got.ID != sub || got.Status != "active" {
			t.Errorf("subscription = %+v", got)
		}
	})

	t.Run("payment_method.detached before the payment_method.attached of the same card", func(t *testing.T) {
		w := newWorld(t)
		w.startCheckout(alice, "")
		customer := w.account(alice).Spec.StripeCustomerID
		pm, attached := w.stripe.AttachCard(customer, cardA)
		detached := w.stripe.DetachCard(pm)
		// Stripe's state already has it detached when either arrives.
		w.ok(detached)
		w.ok(attached)
		if got := w.account(alice).Spec.PaymentMethod; got == nil || got.Present {
			t.Errorf("paymentMethod = %+v, want present: false", got)
		}
		equal(t, "grants: no card was seen, no credit", w.live(), []string(nil))
	})

	t.Run("payment_method.attached replayed after the card was detached", func(t *testing.T) {
		w := newWorld(t)
		acct := w.saveCard(alice, cardA)
		pm := acct.Spec.PaymentMethod.IDs[0]
		w.ok(w.stripe.DetachCard(pm))
		w.ok(w.stripe.AttachedEvent(pm, acct.Spec.StripeCustomerID))
		if got := w.account(alice).Spec.PaymentMethod; got.Present || got.RemovedAt == nil {
			t.Errorf("paymentMethod = %+v, want present: false: the handler reads Stripe again", got)
		}
	})

	t.Run("the same card saved on a second account", func(t *testing.T) {
		w := newWorld(t)
		w.saveCard(alice, cardA)
		second := w.saveCard(bob, cardA)
		if pm := second.Spec.PaymentMethod; pm == nil || !pm.Present {
			t.Errorf("the second account's paymentMethod = %+v, want present: it is active", pm)
		}
		if sc := second.Spec.SignupCredit; sc == nil || sc.State != billing.SignupRefused || sc.Reason != billing.RefusedCardUsed {
			t.Errorf("the second account's signupCredit = %+v, want refused, card-used", sc)
		}
		equal(t, "grants: no second Grant", w.live(), []string{"signup/fpA"})
		if g := w.grant("signup/fpA"); g.Account != w.account(alice).Name {
			t.Errorf("the Grant is %s's", g.Account)
		}
	})

	for _, c := range []struct {
		name, reason string
		card         stripetest.Card
	}{
		{"a prepaid card", billing.RefusedPrepaid, stripetest.Card{Fingerprint: "fpP", Funding: "prepaid"}},
		{"a wallet card", billing.RefusedWallet, stripetest.Card{Fingerprint: "fpW", Funding: "credit", Wallet: "apple_pay"}},
		{"a card with no fingerprint", billing.RefusedNoFingerprint, stripetest.Card{Funding: "credit"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newWorld(t)
			acct := w.saveCard(alice, c.card)
			if pm := acct.Spec.PaymentMethod; pm == nil || !pm.Present {
				t.Errorf("paymentMethod = %+v, want present: the card is saved all the same", pm)
			}
			if sc := acct.Spec.SignupCredit; sc == nil || sc.State != billing.SignupRefused || sc.Reason != c.reason {
				t.Errorf("signupCredit = %+v, want refused, %s", sc, c.reason)
			}
			equal(t, "grants", w.live(), []string(nil))
		})
	}

	t.Run("an event that does not verify", func(t *testing.T) {
		w := newWorld(t)
		w.startCheckout(alice, "")
		customer := w.account(alice).Spec.StripeCustomerID
		_, attached := w.stripe.AttachCard(customer, cardA)
		before := w.state()
		calls := w.stripe.Calls["PaymentMethods"]

		w.stripe.Livemode = true
		_, live := w.stripe.AttachCard(customer, stripetest.Card{Fingerprint: "fpB"})
		w.stripe.Livemode = false
		now := time.Now()
		for name, signature := range map[string]string{
			"a bad signature":            stripetest.Sign(attached.Payload, "whsec_other", now),
			"a timestamp 6 minutes old":  stripetest.Sign(attached.Payload, whsec, now.Add(-6*time.Minute)),
			"no signature":               "",
			"a signature of other bytes": stripetest.Sign(append([]byte(" "), attached.Payload...), whsec, now),
		} {
			if code := w.postSigned(attached.Payload, signature); code != http.StatusBadRequest {
				t.Errorf("%s: status %d, want 400", name, code)
			}
		}
		if code := w.post(live); code != http.StatusBadRequest {
			t.Errorf("livemode of the other mode: status %d, want 400", code)
		}
		if after := w.state(); after != before {
			t.Errorf("a refused event changed the state:\nbefore %s\nafter %s", before, after)
		}
		if got := w.stripe.Calls["PaymentMethods"]; got != calls {
			t.Errorf("a refused event made %d calls to Stripe", got-calls)
		}
		// The same event, signed, 4 minutes old: handled.
		if code := w.postSigned(attached.Payload, stripetest.Sign(attached.Payload, whsec, now.Add(-4*time.Minute))); code != http.StatusOK {
			t.Errorf("a signed event 4 minutes old: status %d, want 200", code)
		}
	})

	t.Run("a live-mode backend refuses test events", func(t *testing.T) {
		w := newWorld(t, func(o *stripe.Options) { o.Mode = "live" })
		if code := w.post(w.stripe.Event("customer.updated", map[string]any{"id": "cus_x"})); code != http.StatusBadRequest {
			t.Errorf("status %d, want 400", code)
		}
		w.stripe.Livemode = true
		if code := w.post(w.stripe.Event("customer.updated", map[string]any{"id": "cus_x"})); code != http.StatusOK {
			t.Errorf("a live event: status %d, want 200", code)
		}
	})

	t.Run("an event for a customer no Account has", func(t *testing.T) {
		w := newWorld(t)
		w.saveCard(alice, cardA)
		stranger := w.stripe.AddCustomer()
		pm, attached := w.stripe.AttachCard(stranger, stripetest.Card{Fingerprint: "fpS", Funding: "credit"})
		before := w.state()
		w.ok(attached)
		w.ok(w.stripe.Event("customer.updated", map[string]any{"id": stranger}))
		w.ok(w.stripe.Event("setup_intent.succeeded", map[string]any{"id": "seti_9", "customer": stranger}))
		w.stripe.PutSubscription(billing.Subscription{ID: "sub_s", Customer: stranger, Status: "active", PriceLookupKey: starter,
			CurrentPeriodStart: w.clock.Now(), CurrentPeriodEnd: w.clock.Now().AddDate(0, 1, 0), LatestInvoiceStatus: "paid"})
		w.ok(w.stripe.SubscriptionEvent("customer.subscription.created", "sub_s"))
		w.ok(w.stripe.DetachCard(pm))
		w.ok(w.stripe.DeleteCustomer(stranger))
		if after := w.state(); after != before {
			t.Errorf("the events changed something:\nbefore %s\nafter %s", before, after)
		}
	})

	t.Run("auto-recharge: the sweep crashes after writing seq and before the call, then runs again", func(t *testing.T) {
		w := newWorld(t)
		w.saveCard(alice, cardA)
		acct := w.rechargeOn(alice)
		// The crash: seq is written, the call is not made.
		w.stripe.FailOn("CreateRecharge", errors.New("the process died"))
		if err := w.svc.Recharge(ctx, acct.Name, 1_000_000); err == nil {
			t.Fatal("no error")
		}
		w.stripe.FailOn("CreateRecharge", nil)
		a := w.account(alice).Spec.AutoRecharge
		equal(t, "after the crash", []any{a.Seq, a.Last.Status, len(w.stripe.PaymentIntentsMade())}, []any{1, "pending", 0})

		// At once, nothing: an attempt is pending. Later, the same attempt.
		if err := w.svc.Recharge(ctx, acct.Name, 1_000_000); err != nil || w.stripe.Calls["CreateRecharge"] != 1 {
			t.Fatalf("an attempt was made again at once: err %v, %d calls", err, w.stripe.Calls["CreateRecharge"])
		}
		w.clock.Advance(11 * time.Minute)
		if err := w.svc.Recharge(ctx, acct.Name, 1_000_000); err != nil {
			t.Fatal(err)
		}
		made := w.stripe.PaymentIntentsMade()
		if len(made) != 1 {
			t.Fatalf("%d PaymentIntents, want 1", len(made))
		}
		a = w.account(alice).Spec.AutoRecharge
		equal(t, "after the second run", []any{a.Seq, a.Last.Status, a.ChargedCents}, []any{1, "succeeded", 2000})
		equal(t, "grants", w.live(), []string{"recharge/" + made[0].ID, "signup/fpA"})
	})

	t.Run("auto-recharge: the charge is made and its answer lost, then the sweep runs again", func(t *testing.T) {
		w := newWorld(t)
		w.saveCard(alice, cardA)
		pi := w.lostRecharge(alice)
		acct := w.account(alice)
		w.clock.Advance(11 * time.Minute)
		if err := w.svc.Recharge(ctx, acct.Name, 1_000_000); err != nil {
			t.Fatal(err)
		}
		// The same idempotency key: the same PaymentIntent.
		if made := w.stripe.PaymentIntentsMade(); len(made) != 1 || made[0].ID != pi {
			t.Fatalf("PaymentIntents = %+v, want only %s", made, pi)
		}
		equal(t, "grants", w.live(), []string{"recharge/" + pi, "signup/fpA"})
		// Its event, late, and again: nothing more.
		before := w.state()
		w.ok(w.stripe.PaymentIntentEvent("payment_intent.succeeded", pi))
		w.ok(w.stripe.PaymentIntentEvent("payment_intent.succeeded", pi))
		if after := w.state(); after != before {
			t.Errorf("the late event changed the state:\nbefore %s\nafter %s", before, after)
		}
	})
}

// What cannot be handled now is answered 500, so that Stripe sends it again;
// then it is handled.
func TestWebhookRetriesWhatFailed(t *testing.T) {
	for name, breakIt := range map[string]func(w *world, err error){
		"Stripe cannot be reached":      func(w *world, err error) { w.stripe.Err = err },
		"the cluster cannot be written": func(w *world, err error) { w.accountsErr = err },
		"a Grant cannot be made":        func(w *world, err error) { w.metronome.Unreachable(err != nil) },
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			ev := w.stripe.CompleteCheckout(w.startCheckout(alice, ""), cardA)
			breakIt(w, errors.New("unavailable"))
			if code := w.post(ev); code != http.StatusInternalServerError {
				t.Fatalf("status %d, want 500", code)
			}
			breakIt(w, nil)
			w.ok(ev)
			if sc := w.account(alice).Spec.SignupCredit; sc == nil || sc.State != billing.SignupGranted {
				t.Errorf("signupCredit = %+v, want granted", sc)
			}
			equal(t, "grants", w.live(), []string{"signup/fpA"})
		})
	}
}

func TestWebhookRefusesWhatIsNotAnEvent(t *testing.T) {
	w := newWorld(t)
	long := []byte(`{"id":"evt_1","type":"customer.updated","data":{"object":{"id":"` + strings.Repeat("a", 1<<20) + `"}}}`)
	if code := w.postSigned(long, stripetest.Sign(long, whsec, time.Now())); code != http.StatusBadRequest {
		t.Errorf("an event over 1 MiB: status %d, want 400", code)
	}
	r, _ := http.NewRequest(http.MethodGet, stripe.WebhookPath, nil)
	rec := httptest.NewRecorder()
	w.webhook.ServeHTTP(rec, r)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: status %d, want 405", rec.Code)
	}
}

// With SIGNUP_CREDIT off a card is saved and nothing is decided.
func TestNoSignupCredit(t *testing.T) {
	w := newWorld(t, func(o *stripe.Options) { o.NoSignupCredit = true })
	acct := w.saveCard(alice, cardA)
	if !acct.Spec.PaymentMethod.Present || acct.Spec.SignupCredit != nil {
		t.Errorf("spec = %+v, want a card and no decision", acct.Spec)
	}
	equal(t, "grants", w.live(), []string(nil))
}

// The decision is made once: a second card, after the first is gone, earns
// nothing.
func TestSignupCreditDecidedOnce(t *testing.T) {
	w := newWorld(t)
	acct := w.saveCard(alice, cardA)
	w.ok(w.stripe.DetachCard(acct.Spec.PaymentMethod.IDs[0]))
	_, attached := w.stripe.AttachCard(acct.Spec.StripeCustomerID, stripetest.Card{Fingerprint: "fpB", Funding: "credit"})
	w.ok(attached)
	got := w.account(alice).Spec
	if !got.PaymentMethod.Present || got.PaymentMethod.RemovedAt != nil {
		t.Errorf("paymentMethod = %+v, want present, removedAt cleared", got.PaymentMethod)
	}
	equal(t, "grants: still exactly one", w.live(), []string{"signup/fpA"})
}

// Two subscriptions on one account: the later is the account's, and only
// its credit counts.
func TestTwoSubscriptionsKeepTheLater(t *testing.T) {
	w := newWorld(t)
	acct := w.saveCard(alice, cardA)
	first := w.subscribe(alice, starter)
	w.clock.Advance(time.Hour)
	now := w.clock.Now()
	second := billing.Subscription{ID: "sub_second", Customer: acct.Spec.StripeCustomerID, Status: "active", PriceLookupKey: pro,
		CurrentPeriodStart: now, CurrentPeriodEnd: now.AddDate(0, 1, 0), Created: now, LatestInvoice: "in_2", LatestInvoiceStatus: "paid"}
	w.stripe.PutSubscription(second)
	w.ok(w.stripe.SubscriptionEvent("customer.subscription.created", second.ID))
	// The older one's event, late.
	w.ok(w.stripe.SubscriptionEvent("customer.subscription.updated", first))
	if got := w.account(alice).Spec.Subscription.ID; got != second.ID {
		t.Errorf("the account's subscription is %s, want the later one", got)
	}
	equal(t, "grants", w.live(), []string{fmt.Sprintf("plan/%s/%d", second.ID, now.Unix()), "signup/fpA"})

	// An ended subscription's event does not displace the one going.
	w.stripe.PutSubscription(billing.Subscription{ID: "sub_old", Customer: acct.Spec.StripeCustomerID, Status: "canceled", PriceLookupKey: starter})
	w.ok(w.stripe.SubscriptionEvent("customer.subscription.deleted", "sub_old"))
	if got := w.account(alice).Spec.Subscription; got.ID != second.ID || got.Status != "active" {
		t.Errorf("subscription = %+v, want %s still", got, second.ID)
	}
}

// A deleted account keeps its owner, its sign-up credit's outcome and its
// deletion: nothing Stripe says is written to it, and nothing is granted.
func TestDeletedAccountIsLeftAlone(t *testing.T) {
	w := newWorld(t)
	acct := w.saveCard(alice, cardA)
	sub := w.subscribe(alice, starter)
	now := w.clock.Now()
	if _, err := w.accounts.Update(ctx, acct.Name, func(s *billing.AccountSpec) error {
		*s = billing.AccountSpec{Owner: s.Owner, OwnerHash: s.OwnerHash, StripeCustomerID: s.StripeCustomerID,
			MetronomeCustomerID: s.MetronomeCustomerID, Credit: s.Credit, SignupCredit: s.SignupCredit, DeletedAt: &now}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before := w.state()
	_, attached := w.stripe.AttachCard(acct.Spec.StripeCustomerID, stripetest.Card{Fingerprint: "fpB", Funding: "credit"})
	w.ok(attached)
	w.ok(w.stripe.SubscriptionEvent("customer.subscription.updated", sub))
	w.ok(w.stripe.InvoiceEvent("invoice.paid", sub))
	w.ok(w.stripe.Event("customer.updated", map[string]any{"id": acct.Spec.StripeCustomerID}))
	w.reconcile()
	if after := w.state(); after != before {
		t.Errorf("a deleted account was written to:\nbefore %s\nafter %s", before, after)
	}
}
