package billing

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// RateCard is the alias of the rate card every contract is on.
const RateCard = "cu-standard-v1"

// The custom fields of a credit.
const (
	FieldGrantKey      = "grant_key"
	FieldSource        = "source"
	FieldPaymentIntent = "payment_intent"
)

// noExpiry stands in for "never" on a credit, which Metronome must be
// given an end for: an admin's grant with no expiresAt.
const noExpiry = 100 * 365 * 24 * time.Hour

// priority is the order credit is used in: the plan's, the sign-up
// credit, purchased credit, an admin's (lower first).
func priority(source string) int {
	switch source {
	case SourcePlan:
		return 10
	case SourceSignup:
		return 20
	case SourcePurchase:
		return 30
	}
	return 40
}

// creditName is what the user sees a credit called.
func creditName(g Grant, cat Catalogue) string {
	switch g.Source {
	case SourceSignup:
		return "Sign-up credit"
	case SourcePlan:
		for _, p := range cat.Plans {
			if p.LookupKey == g.Item {
				return p.Name + " plan credit"
			}
		}
		return "Plan credit"
	}
	return "Credit"
}

// ledger is Ledger over Metronome and the Accounts. It holds nothing:
// every answer is read at the call.
type ledger struct {
	metronome Metronome
	accounts  Accounts
	clock     Clock
	catalogue CatalogueSource
}

// NewLedger is the production Ledger: the credit as Metronome has it.
func NewLedger(m Metronome, accounts Accounts, clock Clock, catalogue CatalogueSource) Ledger {
	return &ledger{metronome: m, accounts: accounts, clock: clock, catalogue: catalogue}
}

func (l *ledger) EnsureCustomer(ctx context.Context, account string) (string, error) {
	acc, err := l.accounts.Get(ctx, account)
	if err != nil {
		return "", err
	}
	if acc.Spec.MetronomeCustomerID != "" {
		return acc.Spec.MetronomeCustomerID, nil
	}
	// The alias is the Account's name: a customer made before, whose ID
	// was not written down, is found by it.
	id, found, err := l.metronome.CustomerByAlias(ctx, account)
	if err != nil {
		return "", err
	}
	if !found {
		if id, err = l.metronome.CreateCustomer(ctx, MetronomeCustomerParams{Name: acc.Spec.OwnerHash, IngestAlias: account}); err != nil {
			return "", err
		}
	}
	err = l.metronome.CreateContract(ctx, MetronomeContractParams{
		Customer: id, RateCardAlias: RateCard, StartingAt: l.clock.Now().UTC().Truncate(time.Hour), UniquenessKey: "contract/" + account,
	})
	if err != nil {
		return "", err
	}
	_, err = l.accounts.Update(ctx, account, func(spec *AccountSpec) error {
		if spec.MetronomeCustomerID == "" {
			spec.MetronomeCustomerID = id
		}
		return nil
	})
	return id, err
}

// counts reports whether a credit counts at now: begun, not ended, not
// archived.
func counts(c MetronomeCredit, now time.Time) bool {
	return !c.Archived && !c.StartingAt.After(now) && now.Before(c.EndingBefore)
}

func (l *ledger) Balance(ctx context.Context, account string) (Balance, error) {
	id, err := l.EnsureCustomer(ctx, account)
	if err != nil {
		return Balance{}, err
	}
	credits, err := l.metronome.Credits(ctx, id)
	if err != nil {
		return Balance{}, err
	}
	now := l.clock.Now()
	var b Balance
	sort.SliceStable(credits, func(i, j int) bool {
		if credits[i].Priority != credits[j].Priority {
			return credits[i].Priority < credits[j].Priority
		}
		return credits[i].EndingBefore.Before(credits[j].EndingBefore)
	})
	for _, c := range credits {
		if !counts(c, now) {
			continue
		}
		// The net balance treats a negative segment as zero.
		left := max(c.BalanceMicros, 0)
		b.NetMicros += left
		end := c.EndingBefore
		b.Credits = append(b.Credits, CreditBalance{Key: c.CustomFields[FieldGrantKey], Source: c.CustomFields[FieldSource],
			AmountMicros: c.AmountMicros, RemainingMicros: left, ExpiresAt: &end})
	}
	return b, nil
}

func (l *ledger) Usage(ctx context.Context, account string, from, to time.Time) (Usage, error) {
	id, err := l.EnsureCustomer(ctx, account)
	if err != nil {
		return Usage{}, err
	}
	used, err := l.metronome.Usage(ctx, id, from, to)
	if err != nil {
		return Usage{}, err
	}
	// Money is quantity at the catalogue's rate.
	rates := l.catalogue.Catalogue().Rates
	awake := func(seconds int64) int64 { return seconds * rates.AwakeMicrosPerHour / 3600 }
	disk := func(gbSeconds int64) int64 { return gbSeconds * rates.DiskMicrosPerGBHour / 3600 }
	u := Usage{Start: from, End: to}
	type sums struct{ awake, disk int64 }
	days, sessions := map[string]*sums{}, map[string]*sums{}
	add := func(m map[string]*sums, key string, r MetronomeUsageRow) {
		if m[key] == nil {
			m[key] = &sums{}
		}
		m[key].awake += r.AwakeSeconds
		m[key].disk += r.DiskGBSeconds
	}
	for _, r := range used.Rows {
		u.AwakeSeconds += r.AwakeSeconds
		u.DiskGBSeconds += r.DiskGBSeconds
		add(days, r.Day, r)
		add(sessions, r.SessionID, r)
	}
	u.AwakeMicros, u.DiskMicros = awake(u.AwakeSeconds), disk(u.DiskGBSeconds)
	for day, s := range days {
		u.Days = append(u.Days, DayUsage{Date: day, AwakeSeconds: s.awake, AwakeMicros: awake(s.awake), DiskMicros: disk(s.disk)})
	}
	for id, s := range sessions {
		u.Sessions = append(u.Sessions, SessionUsage{ID: id, AwakeSeconds: s.awake, AwakeMicros: awake(s.awake), DiskMicros: disk(s.disk)})
	}
	sort.Slice(u.Days, func(i, j int) bool { return u.Days[i].Date < u.Days[j].Date })
	sort.Slice(u.Sessions, func(i, j int) bool { return u.Sessions[i].ID < u.Sessions[j].ID })
	return u, nil
}

// grantOf is a credit as the Grant it was made for.
func grantOf(account string, c MetronomeCredit) Grant {
	key := c.CustomFields[FieldGrantKey]
	end := c.EndingBefore
	g := Grant{Name: GrantName(key), Account: account, Source: c.CustomFields[FieldSource], AmountMicros: c.AmountMicros,
		ValidFrom: c.StartingAt, ExpiresAt: &end, Key: key}
	if pi := c.CustomFields[FieldPaymentIntent]; pi != "" {
		g.Ref = &GrantRef{PaymentIntent: pi}
	}
	if c.Archived {
		g.Revoked = &Revoked{}
	}
	return g
}

// hourUp is t rounded up to the hour, as a credit's end is given.
func hourUp(t time.Time) time.Time {
	if up := t.UTC().Truncate(time.Hour); !up.Equal(t) {
		return up.Add(time.Hour)
	}
	return t.UTC()
}

func (l *ledger) EnsureGrant(ctx context.Context, g Grant) (bool, Grant, error) {
	if g.Account == "" || g.Key == "" || g.AmountMicros <= 0 {
		return false, Grant{}, fmt.Errorf("billing: a grant needs an account, a key and an amount: %+v", g)
	}
	id, err := l.EnsureCustomer(ctx, g.Account)
	if err != nil {
		return false, Grant{}, err
	}
	end := g.ValidFrom.Add(noExpiry)
	if g.ExpiresAt != nil {
		end = *g.ExpiresAt
	}
	fields := map[string]string{FieldGrantKey: g.Key, FieldSource: g.Source}
	if g.Ref != nil && g.Ref.PaymentIntent != "" {
		fields[FieldPaymentIntent] = g.Ref.PaymentIntent
	}
	_, conflict, err := l.metronome.CreateCredit(ctx, MetronomeCreditParams{
		Customer: id, Name: creditName(g, l.catalogue.Catalogue()), UniquenessKey: g.Key, Priority: priority(g.Source),
		AmountMicros: g.AmountMicros, StartingAt: g.ValidFrom.UTC().Truncate(time.Hour), EndingBefore: hourUp(end), CustomFields: fields,
	})
	if err != nil {
		return false, Grant{}, err
	}
	g.Name = GrantName(g.Key)
	if !conflict {
		// Credit arriving clears exhausted, in the same call.
		_, err := l.EnsureCredit(ctx, g.Account)
		return true, g, err
	}
	// The key was used: by this account (a replay), or by another.
	credits, err := l.metronome.Credits(ctx, id)
	if err != nil {
		return false, Grant{}, err
	}
	for _, c := range credits {
		if c.CustomFields[FieldGrantKey] == g.Key {
			_, err := l.EnsureCredit(ctx, g.Account)
			return false, grantOf(g.Account, c), err
		}
	}
	return false, Grant{Name: g.Name, Key: g.Key}, nil
}

func (l *ledger) Revoke(ctx context.Context, sel GrantSelector, reason string) error {
	if sel.Account != "" {
		return l.revoke(ctx, sel.Account, sel)
	}
	// A refund names a payment or a Grant, not an account, and a credit is
	// one Metronome customer's: look in every account that has paid.
	accounts, err := l.accounts.WithCustomer(ctx)
	if err != nil {
		return err
	}
	for _, acc := range accounts {
		if acc.Spec.MetronomeCustomerID == "" {
			continue
		}
		if err := l.revoke(ctx, acc.Name, sel); err != nil {
			return err
		}
	}
	return nil
}

// revoke archives the credits of one account that sel finds. An account
// that had none is not written to.
func (l *ledger) revoke(ctx context.Context, account string, sel GrantSelector) error {
	id, err := l.EnsureCustomer(ctx, account)
	if err != nil {
		return err
	}
	credits, err := l.metronome.Credits(ctx, id)
	if err != nil {
		return err
	}
	found := false
	for _, c := range credits {
		if !sel.Matches(grantOf(account, c)) {
			continue
		}
		if err := l.metronome.ArchiveCredit(ctx, id, c.ID); err != nil {
			return err
		}
		found = true
	}
	if !found && sel.Account == "" {
		return nil
	}
	_, err = l.EnsureCredit(ctx, account)
	return err
}

func (l *ledger) EnsureCredit(ctx context.Context, account string) (AccountCredit, error) {
	b, err := l.Balance(ctx, account)
	if err != nil {
		return AccountCredit{}, err
	}
	now := l.clock.Now()
	next := AccountCredit{Exhausted: b.NetMicros == 0, BalanceMicros: b.NetMicros, CheckedAt: &now}
	for _, c := range b.Credits {
		if c.RemainingMicros > 0 && c.ExpiresAt != nil && (next.NextExpiryAt == nil || c.ExpiresAt.Before(*next.NextExpiryAt)) {
			next.NextExpiryAt = c.ExpiresAt
		}
	}
	acc, err := l.accounts.Update(ctx, account, func(spec *AccountSpec) error {
		c := next
		if c.Exhausted {
			// It keeps the time it became true: the start of "N days at zero".
			c.ExhaustedAt = &now
			if spec.Credit != nil && spec.Credit.Exhausted && spec.Credit.ExhaustedAt != nil {
				c.ExhaustedAt = spec.Credit.ExhaustedAt
			}
		}
		spec.Credit = &c
		return nil
	})
	if err != nil {
		return AccountCredit{}, err
	}
	return *acc.Spec.Credit, nil
}
