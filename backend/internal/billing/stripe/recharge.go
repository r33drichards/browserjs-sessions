package stripe

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/r33drichards/browserjs-sessions/backend/internal/billing"
)

// An account's automatic charges are at least this far apart, and an attempt
// that was written down and never heard of again is made again, with the
// same idempotency key, after it.
const rechargeInterval = 10 * time.Minute

func month(t time.Time) string { return t.UTC().Format("2006-01") }

// rechargeKey is the idempotency key of an account's seq-th automatic
// charge of a month: recharge-<ownerHash>-<yyyy-mm>-<seq>.
func rechargeKey(ownerHash, month string, seq int) string {
	return fmt.Sprintf("recharge-%s-%s-%d", ownerHash, month, seq)
}

// attemptOf is the month and the seq a rechargeKey was made from.
func attemptOf(key string) (month string, seq int, ok bool) {
	parts := strings.Split(key, "-")
	// recharge, the hash, yyyy, mm, seq
	if len(parts) != 5 || parts[0] != "recharge" {
		return "", 0, false
	}
	seq, err := strconv.Atoi(parts[4])
	return parts[2] + "-" + parts[3], seq, err == nil
}

// Recharge makes one automatic top-up for the account if it is due: the
// account's owner turned auto-recharge on, the balance is under their
// threshold, and the month's cap allows it. The balance pass calls it for
// an account whose balance it has just read; it is a no-op, with no call to
// Stripe, for an account it does not apply to.
//
// The attempt is written to the Account (seq, pending) before Stripe is
// called, and the idempotency key is made from that seq: a crash between the
// two repeats the same key and so the same PaymentIntent.
func (s *Service) Recharge(ctx context.Context, account string, balanceMicros int64) error {
	if !s.opt.AutoRecharge {
		return nil
	}
	acct, err := s.accounts.Get(ctx, account)
	if errors.Is(err, billing.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	ar := acct.Spec.AutoRecharge
	if ar == nil || acct.Spec.Blocked != nil || acct.Spec.DeletedAt != nil || acct.Spec.StripeCustomerID == "" {
		return nil
	}
	now := s.clock.Now()
	// The cap is by calendar month: one that stopped it last month no
	// longer does.
	reopened := !ar.Enabled && ar.DisabledReason == DisabledCapReached && ar.Month != month(now)
	if (!ar.Enabled && !reopened) || balanceMicros >= ar.ThresholdMicros {
		return nil
	}
	defer s.lock(acct.Name)()

	cat := s.opt.Catalogue.Catalogue()
	pack, ok := packByKey(cat, ar.Pack)
	if !ok || !pack.Enabled {
		slog.Warn("stripe: auto-recharge names a pack that is not sold", "account", acct.Name, "pack", ar.Pack)
		return nil
	}
	disable := func(reason string) error {
		slog.Info("stripe: auto-recharge turned off", "account", acct.Name, "reason", reason)
		_, err := s.accounts.Update(ctx, acct.Name, func(spec *billing.AccountSpec) error {
			if spec.AutoRecharge != nil {
				spec.AutoRecharge.Enabled = false
				spec.AutoRecharge.DisabledReason = reason
			}
			return nil
		})
		return err
	}
	if pm := acct.Spec.PaymentMethod; pm == nil || !pm.Present {
		return disable(DisabledNoCard)
	}

	// An attempt already written down: wait for it, then make it again.
	retry := false
	if last := ar.Last; last != nil && last.At != nil {
		if now.Sub(*last.At) < rechargeInterval {
			return nil
		}
		// The key has the month in it: last month's is not made again.
		retry = last.Status == RechargePending && ar.Month == month(now)
	}
	charged := ar.ChargedCents
	if ar.Month != month(now) {
		charged = 0
	}
	if !retry && charged+pack.Amount > ar.MonthlyCapCents {
		return disable(DisabledCapReached)
	}

	// The card: the customer's default, else the newest.
	methods, def, err := s.stripe.PaymentMethods(ctx, acct.Spec.StripeCustomerID)
	if err != nil {
		return err
	}
	if len(methods) == 0 {
		return disable(DisabledNoCard)
	}
	sortOldestFirst(methods)
	card := methods[len(methods)-1].ID
	for _, m := range methods {
		if m.ID == def {
			card = def
		}
	}

	var seq int
	acct, err = s.accounts.Update(ctx, acct.Name, func(spec *billing.AccountSpec) error {
		a := spec.AutoRecharge
		if a == nil {
			return errors.New("auto-recharge was removed")
		}
		if a.Month != month(now) {
			a.Month, a.Seq, a.ChargedCents = month(now), 0, 0
		}
		if !retry {
			a.Seq++
		}
		a.Enabled, a.DisabledReason = true, ""
		a.Last = &billing.LastRecharge{Status: RechargePending, At: &now}
		seq = a.Seq
		return nil
	})
	if err != nil {
		return err
	}
	pi, err := s.stripe.CreateRecharge(ctx, billing.RechargeParams{
		Customer: acct.Spec.StripeCustomerID, PaymentMethod: card,
		AmountCents: pack.Amount, Currency: cat.Currency,
		Account: acct.Name, Item: pack.LookupKey,
		IdempotencyKey: rechargeKey(acct.Spec.OwnerHash, month(now), seq),
	})
	if err != nil {
		// Still pending: made again, with the same key, after the interval.
		return err
	}
	return s.recharge(ctx, acct, pi, true)
}

// Consider is Recharge as the balance pass asks for it (metronome.Recharger):
// with the account and the credit it has just read.
func (s *Service) Consider(ctx context.Context, account billing.Account, credit billing.AccountCredit) {
	if err := s.Recharge(ctx, account.Name, credit.BalanceMicros); err != nil {
		slog.Error("stripe: auto-recharge", "account", account.Name, "err", err)
	}
}

// EnsureRecharge writes down what became of an automatic charge, and makes
// its Grant if it succeeded.
func (s *Service) EnsureRecharge(ctx context.Context, id string) error {
	pi, err := s.stripe.PaymentIntent(ctx, id)
	if errors.Is(err, billing.ErrNotFound) {
		slog.Warn("stripe: no such payment intent", "paymentIntent", id)
		return nil
	}
	if err != nil {
		return err
	}
	if pi.Metadata["kind"] != billing.KindRecharge {
		return nil
	}
	acct, err := s.accountOf(ctx, pi.Metadata["account"], pi.Customer)
	if errors.Is(err, billing.ErrNotFound) {
		slog.Warn("stripe: no account for this automatic charge", "paymentIntent", id, "customer", pi.Customer)
		return nil
	}
	if err != nil {
		return err
	}
	if deleted(acct, "an automatic charge") {
		return nil
	}
	defer s.lock(acct.Name)()
	return s.recharge(ctx, acct, pi, false)
}

// recharge is EnsureRecharge for an account whose lock is held. answer says
// that pi is Stripe's answer to the attempt the Account has pending.
func (s *Service) recharge(ctx context.Context, acct billing.Account, pi billing.PaymentIntent, answer bool) error {
	now := s.clock.Now()
	// Whether the Account's "last" is this attempt and has no outcome yet.
	// An older attempt's outcome, read again by the reconcile for 35 days,
	// must not be written over a newer one's. The charge says which attempt
	// it was: metadata.month and metadata.seq, from its idempotency key.
	current := func(a *billing.AutoRecharge) bool {
		if a == nil || a.Last == nil || a.Last.Status != RechargePending {
			return false
		}
		if answer {
			return true
		}
		if seq, err := strconv.Atoi(pi.Metadata["seq"]); err == nil && pi.Metadata["month"] != "" {
			return a.Month == pi.Metadata["month"] && a.Seq == seq
		}
		// A charge that does not say: it is the pending one if that one
		// was written down no later than the charge was made.
		return a.Last.At != nil && !pi.Created.Before(a.Last.At.Add(-time.Minute))
	}
	switch pi.Status {
	case "succeeded":
		pack, ok := packOf(s.opt.Catalogue.Catalogue(), pi.Metadata["item"])
		if !ok {
			slog.Error("stripe: an automatic charge's item is not a pack of the catalogue; NO CREDIT WAS GRANTED",
				"paymentIntent", pi.ID, "item", pi.Metadata["item"])
			return nil
		}
		expires := now.Add(time.Duration(pack.ValidDays) * 24 * time.Hour)
		// ref.paymentIntent is how a refund finds it.
		created, _, err := s.ledger.EnsureGrant(ctx, billing.Grant{
			Account: acct.Name, Source: billing.SourcePurchase, AmountMicros: pack.CreditMicros,
			ValidFrom: now, ExpiresAt: &expires, Key: "recharge/" + pi.ID, Item: pack.LookupKey,
			Ref: &billing.GrantRef{PaymentIntent: pi.ID},
		})
		if err != nil {
			return err
		}
		_, err = s.accounts.Update(ctx, acct.Name, func(spec *billing.AccountSpec) error {
			a := spec.AutoRecharge
			if a == nil {
				return nil
			}
			cur := current(a)
			// Counted once: when the attempt gets its outcome, or, if that
			// was lost, when its Grant is made.
			if (cur || created) && a.Month == month(pi.Created) {
				a.ChargedCents += pi.AmountCents
			}
			if cur {
				a.Last = &billing.LastRecharge{PaymentIntent: pi.ID, Status: RechargeSucceeded, At: &now}
			}
			return nil
		})
		return err
	case "requires_payment_method", "canceled":
		reason := DisabledPaymentFailed
		if pi.DeclineCode == "authentication_required" {
			reason = DisabledAuthRequired
		}
		_, err := s.accounts.Update(ctx, acct.Name, func(spec *billing.AccountSpec) error {
			a := spec.AutoRecharge
			if !current(a) {
				return nil
			}
			// Not retried: the user is told, and offered a checkout.
			a.Enabled, a.DisabledReason = false, reason
			a.Last = &billing.LastRecharge{PaymentIntent: pi.ID, Status: RechargeFailed, At: &now}
			return nil
		})
		return err
	}
	// Still on its way (processing, requires_action): its event follows.
	return nil
}
