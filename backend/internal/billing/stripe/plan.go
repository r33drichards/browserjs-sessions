package stripe

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/r33drichards/browserjs-sessions/backend/internal/billing"
)

// PlanChanger is a billing.Stripe that can also change a subscription's
// plan: what POST /api/billing/subscription needs. (Changing plan in
// Stripe's portal is off: docs/billing-iac.md.)
type PlanChanger interface {
	// UpgradeSubscription moves the subscription to price now: a new period
	// starts, charged at once less the unused time of the old one, and any
	// change scheduled for the period's end is dropped. A card that refuses
	// is ErrPaymentFailed, and the subscription is as it was.
	UpgradeSubscription(ctx context.Context, id, price string) error
	// SchedulePrice moves the subscription to price at the end of its
	// current period, in place of any change already scheduled. price ""
	// drops what is scheduled.
	SchedulePrice(ctx context.Context, id, price string) error
}

// ErrPaymentFailed is a PlanChanger's answer when the card refused the
// charge of an upgrade.
var ErrPaymentFailed = errors.New("the payment failed")

// The codes of a plan change's refusals.
const (
	CodeNotSubscribed      = "not_subscribed"
	CodePaymentFailed      = "payment_failed"
	CodeSubscriptionEnding = "subscription_ending"
)

type planChangeView struct {
	// Change is what was done: "upgraded" (now), "scheduled" (at the
	// period's end), "kept" (a scheduled change was dropped), or "none"
	// (the subscription was already on the plan).
	Change string `json:"change"`
	Item   string `json:"item"`
	// EffectiveAt is when the plan is, or will be, the subscription's.
	EffectiveAt *time.Time `json:"effectiveAt,omitempty"`
}

// changePlan changes the caller's subscription to another plan: a dearer
// one now, a cheaper one at the end of the period that is paid for. Asked
// twice, it is done once: Stripe is read first, and a subscription already
// on the plan is left alone.
func (s *Service) changePlan(w http.ResponseWriter, r *http.Request, acct billing.Account) {
	ctx := r.Context()
	var req struct {
		Item string `json:"item"`
	}
	if !body(w, r, &req) {
		return
	}
	cat := s.opt.Catalogue.Catalogue()
	target, ok := planOf(cat, req.Item)
	price, priced := s.price(req.Item)
	if !ok || !target.Enabled || !priced {
		s.fail(w, http.StatusBadRequest, CodeUnknownItem, "That plan is not available.")
		return
	}
	changer, ok := s.stripe.(PlanChanger)
	if !ok {
		s.unavailable(w, "change plan", errors.New("this Stripe client cannot change a plan"))
		return
	}
	notSubscribed := func() {
		s.fail(w, http.StatusConflict, CodeNotSubscribed, "You have no subscription to change. Subscribe to a plan instead.")
	}
	if acct.Spec.Subscription == nil {
		notSubscribed()
		return
	}
	defer s.lock(acct.Name)()
	// What Stripe says now, not what was last written.
	sub, err := s.stripe.Subscription(ctx, acct.Spec.Subscription.ID)
	if errors.Is(err, billing.ErrNotFound) {
		notSubscribed()
		return
	}
	if err != nil {
		s.unavailable(w, "read subscription", err)
		return
	}
	switch {
	case sub.Customer != acct.Spec.StripeCustomerID:
		notSubscribed()
		return
	case sub.Status == "past_due" || sub.Status == "incomplete":
		s.fail(w, http.StatusPaymentRequired, CodePaymentFailed, "Your last payment failed. Update your card in the billing portal, then change plan.")
		return
	case sub.Status != "active" && sub.Status != "trialing":
		notSubscribed()
		return
	case sub.CancelAt != nil:
		s.fail(w, http.StatusConflict, CodeSubscriptionEnding, "Your subscription is ending. Choose a plan when it has ended.")
		return
	}
	current, known := planOf(cat, sub.PriceLookupKey)
	view := planChangeView{Item: target.LookupKey}
	end := sub.CurrentPeriodEnd
	switch {
	case sub.PriceLookupKey == target.LookupKey:
		// Already on it. Choosing it again drops a change that was waiting
		// for the period's end.
		if err = changer.SchedulePrice(ctx, sub.ID, ""); err == nil {
			view.Change = "kept"
		}
	case known && target.Amount > current.Amount:
		if err = changer.UpgradeSubscription(ctx, sub.ID, price); err == nil {
			now := s.clock.Now()
			view.Change, view.EffectiveAt = "upgraded", &now
		}
	default:
		if err = changer.SchedulePrice(ctx, sub.ID, price); err == nil {
			view.Change, view.EffectiveAt = "scheduled", &end
		}
	}
	if errors.Is(err, ErrPaymentFailed) {
		s.fail(w, http.StatusPaymentRequired, CodePaymentFailed, "Your card was declined, and your plan is as it was. Update your card in the billing portal and try again.")
		return
	}
	if err != nil {
		s.unavailable(w, "change plan", err)
		return
	}
	slog.Info("stripe: plan change", "account", acct.Name, "subscription", sub.ID, "change", view.Change, "from", sub.PriceLookupKey, "to", target.LookupKey)
	if view.Change == "upgraded" {
		// The new period's credit, and the old one's remainder gone, now:
		// the events say the same when they arrive.
		if err := s.subscription(ctx, acct, sub.ID); err != nil {
			slog.Error("stripe: an upgrade was made and its credit is not granted yet; the webhook or the reconcile will", "account", acct.Name, "err", err)
		}
	}
	writeJSON(w, http.StatusOK, view)
}
