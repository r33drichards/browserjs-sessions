package stripe

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/r33drichards/browserjs-sessions/backend/internal/billing"
)

// How often the two reconciles run.
const (
	paymentMethodsEvery = 15 * time.Minute
	purchasesEvery      = time.Hour
)

// Run reads the prices and runs both reconciles now, then keeps them going
// until ctx is done: payment methods every 15 minutes (a missed
// payment_method.detached must not leave sessions running for long without
// a card), prices and purchases every hour.
func (s *Service) Run(ctx context.Context) {
	hourly := func() {
		if err := s.RefreshPrices(ctx); err != nil {
			slog.Error("stripe: prices not read", "err", err)
		}
		s.ReconcilePurchases(ctx, s.clock.Now().Add(-reconcileWindow))
	}
	s.ReconcilePaymentMethods(ctx)
	hourly()
	cards := time.NewTicker(paymentMethodsEvery)
	defer cards.Stop()
	purchases := time.NewTicker(purchasesEvery)
	defer purchases.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-cards.C:
			s.ReconcilePaymentMethods(ctx)
		case <-purchases.C:
			hourly()
		}
	}
}

// each calls do for every Account that has a customer, pausing between them
// to stay far below Stripe's rate limits. One account's failure is logged
// and does not stop the pass.
func (s *Service) each(ctx context.Context, what string, do func(billing.Account) error) {
	accounts, err := s.accounts.WithCustomer(ctx)
	if err != nil {
		slog.Error("stripe: reconcile: accounts not listed", "pass", what, "err", err)
		return
	}
	failed := 0
	for _, acct := range accounts {
		if ctx.Err() != nil {
			return
		}
		if err := do(acct); err != nil {
			failed++
			slog.Error("stripe: reconcile", "pass", what, "account", acct.Name, "err", err)
		}
		if s.opt.Pace > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(s.opt.Pace):
			}
		}
	}
	slog.Info("stripe: reconciled", "pass", what, "accounts", len(accounts), "failed", failed)
}

// ReconcilePaymentMethods runs EnsurePaymentMethods for every customer.
func (s *Service) ReconcilePaymentMethods(ctx context.Context) {
	s.each(ctx, "payment-methods", func(acct billing.Account) error {
		return s.EnsurePaymentMethods(ctx, acct.Spec.StripeCustomerID)
	})
}

// ReconcilePurchases runs, for every customer, EnsureSubscription for each
// subscription that has not ended, and EnsurePurchase and EnsureRecharge for
// each Checkout Session and automatic charge made since since. It repairs a
// missed webhook, and makes every paid-for Grant again after the cluster's
// objects are lost: their names are deterministic.
func (s *Service) ReconcilePurchases(ctx context.Context, since time.Time) {
	s.each(ctx, "purchases", func(acct billing.Account) error {
		customer := acct.Spec.StripeCustomerID
		var errs []error
		subs, err := s.stripe.Subscriptions(ctx, customer)
		errs = append(errs, err)
		listed := map[string]bool{}
		for _, sub := range subs {
			listed[sub.ID] = true
			if sub.Status != "canceled" {
				errs = append(errs, s.EnsureSubscription(ctx, sub.ID))
			}
		}
		// A subscription the Account names and Stripe does not list: it
		// ended, and no event said so.
		if cur := acct.Spec.Subscription; cur != nil && subscribed(cur.Status) && !listed[cur.ID] {
			errs = append(errs, s.EnsureSubscription(ctx, cur.ID))
		}
		checkouts, err := s.stripe.Checkouts(ctx, customer, since)
		errs = append(errs, err)
		for _, cs := range checkouts {
			_, err := s.EnsurePurchase(ctx, cs.ID)
			errs = append(errs, err)
		}
		intents, err := s.stripe.PaymentIntents(ctx, customer, since)
		errs = append(errs, err)
		for _, pi := range intents {
			if pi.Metadata["kind"] == billing.KindRecharge {
				errs = append(errs, s.EnsureRecharge(ctx, pi.ID))
			}
		}
		return errors.Join(errs...)
	})
}
