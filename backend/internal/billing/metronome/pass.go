// Package metronome is the backend's dealings with Metronome, the meter
// and the credit ledger (docs/contracts/billing/metronome.md): the HTTP
// client (billing.Metronome), the webhook for the zero-balance alert, the
// balance pass. The Ledger over the client
// is billing.NewLedger; nothing here holds a balance.
package metronome

import (
	"context"
	"log/slog"
	"sort"
	"time"

	"github.com/r33drichards/computer-use/backend/internal/billing"
	"github.com/r33drichards/computer-use/backend/internal/sessions"
)

// Recharger is the Stripe component's auto-recharge (stripe.md,
// "Auto-recharge"): the balance pass offers it every active account with
// auto-recharge on, with the balance it has just read, and it decides
// whether a charge is due.
type Recharger interface {
	Consider(ctx context.Context, account billing.Account, credit billing.AccountCredit)
}

// Sessions is what the pass needs of the session store.
type Sessions interface {
	ListAll(ctx context.Context) ([]sessions.Session, error)
}

// Pass is the balance pass: Ledger.EnsureCredit for each Account that has
// an awake session, or auto-recharge on, or a credit whose end has passed.
// It repairs a missed or late alert and notices credit that ran out by
// expiring. It keeps nothing between runs.
type Pass struct {
	Accounts billing.Accounts
	Ledger   billing.Ledger
	Sessions Sessions
	Clock    billing.Clock
	// Recharge is nil where auto-recharge is off.
	Recharge Recharger
}

// Run makes one pass. An account that cannot be read from Metronome keeps
// what it had: nobody is refused or stopped for Metronome's sake.
func (p *Pass) Run(ctx context.Context) error {
	all, err := p.Sessions.ListAll(ctx)
	if err != nil {
		return err
	}
	due := map[string]bool{}
	for _, s := range all {
		if s.Owner != "" && (s.State == sessions.Running || s.State == sessions.Starting) {
			due[billing.AccountName(s.Owner)] = true
		}
	}
	// Auto-recharge and paid-for credit are of accounts that have been to
	// Stripe.
	withCustomer, err := p.Accounts.WithCustomer(ctx)
	if err != nil {
		return err
	}
	now := p.Clock.Now()
	recharging := map[string]bool{}
	for _, acc := range withCustomer {
		if ar := acc.Spec.AutoRecharge; ar != nil && ar.Enabled {
			due[acc.Name], recharging[acc.Name] = true, true
		}
		if c := acc.Spec.Credit; c != nil && c.NextExpiryAt != nil && !c.NextExpiryAt.After(now) {
			due[acc.Name] = true
		}
	}
	names := make([]string, 0, len(due))
	for name := range due {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		credit, err := p.Ledger.EnsureCredit(ctx, name)
		if err != nil {
			slog.Warn("balance pass: Metronome not read; the account keeps what it had", "account", name, "err", err)
			continue
		}
		if p.Recharge != nil && recharging[name] {
			if acc, err := p.Accounts.Get(ctx, name); err == nil {
				p.Recharge.Consider(ctx, acc, credit)
			}
		}
	}
	return nil
}

// Loop makes a pass every interval until ctx is done.
func (p *Pass) Loop(ctx context.Context, interval time.Duration) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if err := p.Run(ctx); err != nil {
				slog.Error("balance pass failed", "err", err)
			}
		}
	}
}
