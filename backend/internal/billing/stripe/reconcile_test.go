package stripe_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/r33drichards/computer-use/backend/internal/billing"
	"github.com/r33drichards/computer-use/backend/internal/billing/stripe"
	"github.com/r33drichards/computer-use/backend/internal/billing/stripe/stripetest"
)

func (w *world) reconcile() {
	w.svc.ReconcilePaymentMethods(ctx)
	w.svc.ReconcilePurchases(ctx, w.clock.Now().Add(-35*24*time.Hour))
}

// The reconcile makes every paid-for Grant whose event never arrived, under
// the key the event would have used, and a second pass changes nothing:
// Metronome answers that the key is taken.
func TestReconcileMakesPaidForGrants(t *testing.T) {
	w := newWorld(t)
	w.saveCard(alice, cardA)
	// A plan, a pack and an automatic charge, none of whose events arrive.
	plan := w.startCheckout(alice, starter)
	w.stripe.CompleteCheckout(plan, cardA)
	cs, _ := w.stripe.Checkout(ctx, plan)
	pack := w.startCheckout(alice, pack20)
	w.stripe.CompleteCheckout(pack, cardA)
	recharge := w.lostRecharge(alice)
	equal(t, "grants before", w.live(), []string{"signup/fpA"})

	w.clock.Advance(time.Hour)
	w.reconcile()
	paid := []string{fmt.Sprintf("plan/%s/%d", cs.Subscription, w.clock.Now().Add(-time.Hour).Unix()), "purchase/" + pack, "recharge/" + recharge, "signup/fpA"}
	equal(t, "grants", w.live(), paid)
	if got := w.account(alice).Spec.AutoRecharge; got.ChargedCents != 2000 || got.Last.Status != "succeeded" {
		t.Errorf("autoRecharge = %+v, want the charge counted once", got)
	}
	before := w.state()
	w.reconcile()
	if after := w.state(); after != before {
		t.Errorf("a second reconcile changed the state:\nbefore %s\nafter %s", before, after)
	}
}

// A webhook was missed: the reconcile does what it would have.
func TestReconcileRepairsMissedWebhooks(t *testing.T) {
	t.Run("a card removed with no webhook", func(t *testing.T) {
		w := newWorld(t)
		acct := w.saveCard(alice, cardA)
		w.stripe.DetachCard(acct.Spec.PaymentMethod.IDs[0]) // no event
		if !w.account(alice).Spec.PaymentMethod.Present {
			t.Fatal("the Account knew without being told")
		}
		w.clock.Advance(15 * time.Minute)
		w.svc.ReconcilePaymentMethods(ctx)
		pm := w.account(alice).Spec.PaymentMethod
		if pm.Present || pm.RemovedAt == nil || !pm.RemovedAt.Equal(w.clock.Now()) || len(pm.IDs) != 0 {
			t.Errorf("paymentMethod = %+v, want absent, removed now", pm)
		}
		// removedAt is when it was noticed, and stays that.
		w.clock.Advance(15 * time.Minute)
		w.svc.ReconcilePaymentMethods(ctx)
		if got := w.account(alice).Spec.PaymentMethod; !got.RemovedAt.Equal(*pm.RemovedAt) || !got.ReadAt.Equal(w.clock.Now()) {
			t.Errorf("paymentMethod = %+v, want removedAt kept and readAt now", got)
		}
	})
	t.Run("a checkout completed with no webhook, and never returned from", func(t *testing.T) {
		w := newWorld(t)
		w.stripe.CompleteCheckout(w.startCheckout(alice, ""), cardA)
		w.stripe.CompleteCheckout(w.startCheckout(alice, pack20), cardA)
		id := w.startCheckout(alice, starter)
		w.stripe.CompleteCheckout(id, cardA)
		cs, _ := w.stripe.Checkout(ctx, id)
		w.reconcile()
		got := w.account(alice).Spec
		if !got.PaymentMethod.Present || got.SignupCredit == nil || got.Subscription == nil || got.Subscription.ID != cs.Subscription {
			t.Errorf("spec = %+v", got)
		}
		if n := len(w.live()); n != 3 {
			t.Errorf("grants = %v, want the sign-up credit, the pack and the plan", w.live())
		}
	})
	t.Run("a renewal paid with no webhook; a subscription ended with none", func(t *testing.T) {
		w := newWorld(t)
		w.saveCard(alice, cardA)
		sub := w.subscribe(alice, starter)
		w.renew(sub, func(*billing.Subscription) {})
		w.reconcile()
		if n := len(w.live()); n != 3 {
			t.Errorf("grants = %v, want the sign-up credit and two periods", w.live())
		}
		w.stripe.ChangeSubscription(sub, func(s *billing.Subscription) { s.Status = "canceled" })
		w.reconcile()
		if got := w.account(alice).Spec.Subscription.Status; got != "canceled" {
			t.Errorf("status = %q, want canceled", got)
		}
	})
	t.Run("only what is in the window", func(t *testing.T) {
		w := newWorld(t)
		w.saveCard(alice, cardA)
		// Two packs whose events never arrive, 36 days apart.
		old := w.startCheckout(alice, pack20)
		w.stripe.CompleteCheckout(old, cardA)
		w.clock.Advance(36 * 24 * time.Hour)
		recent := w.startCheckout(alice, pack20)
		w.stripe.CompleteCheckout(recent, cardA)
		w.reconcile()
		equal(t, "grants", w.live(), []string{"purchase/" + recent, "signup/fpA"})
		// By hand, with an earlier date: the older one too.
		w.svc.ReconcilePurchases(ctx, w.clock.Now().Add(-90*24*time.Hour))
		equal(t, "grants", w.live(), []string{"purchase/" + old, "purchase/" + recent, "signup/fpA"})
	})
	t.Run("a customer deleted at Stripe with no webhook", func(t *testing.T) {
		w := newWorld(t)
		a := w.saveCard(alice, cardA)
		b := w.saveCard(bob, stripetest.Card{Fingerprint: "fpB", Funding: "credit"})
		w.stripe.DeleteCustomer(a.Spec.StripeCustomerID)
		w.svc.ReconcilePaymentMethods(ctx)
		if w.account(alice).Spec.PaymentMethod.Present {
			t.Error("an account whose customer is gone still has a card")
		}
		if got, _ := w.accounts.Get(ctx, b.Name); !got.Spec.PaymentMethod.Present {
			t.Error("the other account lost its card")
		}
	})
	t.Run("Stripe down: nothing is changed", func(t *testing.T) {
		w := newWorld(t)
		w.saveCard(alice, cardA)
		w.subscribe(alice, starter)
		before := w.state()
		w.stripe.Err = errors.New("timeout")
		w.reconcile()
		if after := w.state(); after != before {
			t.Errorf("a reconcile that reached nothing changed the state:\nbefore %s\nafter %s", before, after)
		}
	})
}

func TestPrices(t *testing.T) {
	cat := contractCatalogue(t)
	// More than ten things on sale: more than one call.
	for i := 0; i < 9; i++ {
		cat.Packs = append(cat.Packs, billing.Item{Key: fmt.Sprint("extra-", i), ProductID: "cu_credit",
			LookupKey: fmt.Sprint("cu_extra_", i), Amount: 100, CreditMicros: 1_000_000, ValidDays: 365, Enabled: true})
	}
	w := newWorld(t, func(o *stripe.Options) { o.Catalogue = cat })
	w.stripe.SetPrices("cu_extra_8")
	calls := w.stripe.Calls["Prices"]
	if err := w.svc.RefreshPrices(ctx); err != nil {
		t.Fatal(err)
	}
	if got := w.stripe.Calls["Prices"] - calls; got != 2 {
		t.Errorf("%d calls for %d lookup keys, want 2", got, len(stripe.LookupKeys(cat)))
	}
	w.saveCard(alice, cardA)
	w.startCheckout(alice, "cu_extra_8")
	// On sale in the catalogue, and no price at Stripe: not offered.
	code, out := w.call(alice, "POST", "/api/billing/checkout", `{"item":"cu_extra_0"}`)
	wantError(t, "no price", code, out, 400, "unknown_item")

	// Stripe down: the prices known stay.
	w.stripe.Err = errors.New("timeout")
	if err := w.svc.RefreshPrices(ctx); err == nil {
		t.Error("no error")
	}
	w.stripe.Err = nil
	w.startCheckout(alice, pack20)
}

func TestGrantName(t *testing.T) {
	// "g-" and the first 40 hex characters of the SHA-256 of the key.
	if got := billing.GrantName("signup/fpA"); len(got) != 42 || got[:2] != "g-" {
		t.Errorf("GrantName = %q", got)
	}
	if billing.GrantName("purchase/cs_1") == billing.GrantName("purchase/cs_2") {
		t.Error("two keys, one name")
	}
}

func TestCatalogue(t *testing.T) {
	cat := contractCatalogue(t)
	if err := stripe.CheckCatalogue(cat); err != nil {
		t.Fatal(err)
	}
	equal(t, "what is on sale", stripe.LookupKeys(cat),
		[]string{"cu_starter_monthly_v1", "cu_pro_monthly_v1", "cu_credit_5_v1", "cu_credit_20_v1", "cu_credit_50_v1"})
	item := billing.Item{Key: "a", ProductID: "p", LookupKey: "k", Amount: 1, CreditMicros: 1, ValidDays: 1}
	with := func(change func(*billing.Item)) billing.Item {
		it := item
		change(&it)
		return it
	}
	for name, c := range map[string]billing.Catalogue{
		"no currency":            {Packs: []billing.Item{item}},
		"a lookup key twice":     {Currency: "usd", Plans: []billing.Item{item}, Packs: []billing.Item{item}},
		"a pack with no days":    {Currency: "usd", Packs: []billing.Item{with(func(i *billing.Item) { i.ValidDays = 0 })}},
		"a free plan":            {Currency: "usd", Plans: []billing.Item{with(func(i *billing.Item) { i.Amount = 0 })}},
		"a pack with no product": {Currency: "usd", Packs: []billing.Item{with(func(i *billing.Item) { i.ProductID = "" })}},
	} {
		if err := stripe.CheckCatalogue(c); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	// And a Service is not made from one.
	w := newWorld(t)
	if _, err := stripe.New(w.accounts, w.ledger, w.stripe, w.clock,
		stripe.Options{Mode: "test", WebhookSecret: whsec, PublicURL: publicURL, Catalogue: billing.Catalogue{}}); err == nil {
		t.Error("a Service was made from an empty catalogue")
	}
}
