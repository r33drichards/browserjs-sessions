package billing_test

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/r33drichards/browserjs-sessions/backend/internal/billing"
	"github.com/r33drichards/browserjs-sessions/backend/internal/billing/billingtest"
)

var t0 = time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC)

var rates = billing.Catalogue{
	Rates: billing.Rates{AwakeMicrosPerHour: 200000, DiskMicrosPerGBHour: 384}, SessionDiskGB: 5,
	Payg:  billing.Tier{MaxSessions: 3, MaxAwake: 2},
	Plans: []billing.Item{{Key: "starter", Name: "Starter", LookupKey: "cu_starter_monthly_v1", MaxSessions: 3, MaxAwake: 2}},
}

// books is the real Ledger over the fake Metronome and the fake Accounts.
type books struct {
	clock     *billingtest.Clock
	accounts  *billingtest.Accounts
	sessions  *billingtest.Sessions
	metronome *billingtest.Metronome
	ledger    billing.Ledger
}

func newBooks() *books {
	b := &books{clock: billingtest.NewClock(t0)}
	b.accounts = billingtest.NewAccounts(b.clock)
	b.sessions = billingtest.NewSessions(b.clock)
	b.metronome = billingtest.NewMetronome(b.clock, b.sessions, rates)
	b.ledger = billing.NewLedger(b.metronome, b.accounts, b.clock, rates)
	return b
}

func (b *books) account(t *testing.T, owner string) billing.Account {
	t.Helper()
	acc, err := b.accounts.Ensure(t.Context(), owner)
	if err != nil {
		t.Fatal(err)
	}
	return acc
}

func (b *books) credit(t *testing.T, name string) billing.AccountCredit {
	t.Helper()
	acc, err := b.accounts.Get(t.Context(), name)
	if err != nil || acc.Spec.Credit == nil {
		t.Fatalf("account %s has no credit recorded (%v)", name, err)
	}
	return *acc.Spec.Credit
}

func grant(account, source, key string, micros int64, from time.Time, days int) billing.Grant {
	until := from.AddDate(0, 0, days)
	return billing.Grant{Account: account, Source: source, AmountMicros: micros, ValidFrom: from, ExpiresAt: &until, Key: key}
}

func TestEnsureCustomerMakesItOnce(t *testing.T) {
	b := newBooks()
	acc := b.account(t, "u@example.com")
	id, err := b.ledger.EnsureCustomer(t.Context(), acc.Name)
	if err != nil || id == "" {
		t.Fatalf("%q, %v", id, err)
	}
	if got, _ := b.accounts.Get(t.Context(), acc.Name); got.Spec.MetronomeCustomerID != id {
		t.Fatalf("the Account records %q, want %q", got.Spec.MetronomeCustomerID, id)
	}
	calls := b.metronome.Calls()
	again, err := b.ledger.EnsureCustomer(t.Context(), acc.Name)
	if err != nil || again != id || b.metronome.Calls() != calls {
		t.Fatalf("a second call: %q, %v, %d more calls to Metronome", again, err, b.metronome.Calls()-calls)
	}
	// The ID lost (the Account restored from an export): the customer is
	// found by its alias, not made twice.
	if _, err := b.accounts.Update(t.Context(), acc.Name, func(s *billing.AccountSpec) error { s.MetronomeCustomerID = ""; return nil }); err != nil {
		t.Fatal(err)
	}
	if found, err := b.ledger.EnsureCustomer(t.Context(), acc.Name); err != nil || found != id {
		t.Fatalf("after the ID was lost: %q, %v, want %q", found, err, id)
	}
}

// A credit is made once for its key: a replay finds this account's, and a
// key another account used comes back with no account.
func TestEnsureGrant(t *testing.T) {
	b := newBooks()
	ctx := t.Context()
	u, v := b.account(t, "u@example.com"), b.account(t, "v@example.com")

	// An account is exhausted until its first grant.
	if c, err := b.ledger.EnsureCredit(ctx, u.Name); err != nil || !c.Exhausted || c.ExhaustedAt == nil || c.BalanceMicros != 0 {
		t.Fatalf("before any grant: %+v, %v", c, err)
	}
	b.clock.Advance(10 * time.Minute)
	g := grant(u.Name, billing.SourceSignup, "signup/fpA", 5_000_000, b.clock.Now(), 90)
	created, made, err := b.ledger.EnsureGrant(ctx, g)
	if err != nil || !created || made.Account != u.Name || made.Name != billing.GrantName("signup/fpA") {
		t.Fatalf("first: %v, %+v, %v", created, made, err)
	}
	// Credit arriving clears exhausted, in the same call.
	if c := b.credit(t, u.Name); c.Exhausted || c.ExhaustedAt != nil || c.BalanceMicros != 5_000_000 || c.NextExpiryAt == nil || c.CheckedAt == nil {
		t.Fatalf("after the grant: %+v", c)
	}
	created, existing, err := b.ledger.EnsureGrant(ctx, g)
	if err != nil || created || existing.Account != u.Name || existing.Key != "signup/fpA" || existing.AmountMicros != 5_000_000 {
		t.Fatalf("a replay: %v, %+v, %v", created, existing, err)
	}
	created, existing, err = b.ledger.EnsureGrant(ctx, grant(v.Name, billing.SourceSignup, "signup/fpA", 5_000_000, b.clock.Now(), 90))
	if err != nil || created || existing.Account != "" {
		t.Fatalf("the same card on another account: %v, %+v, %v", created, existing, err)
	}
	if got := b.metronome.AllCredits(); len(got) != 1 || got[0].Priority != 20 || got[0].Name != "Sign-up credit" ||
		got[0].CustomFields[billing.FieldSource] != billing.SourceSignup {
		t.Fatalf("credits in Metronome: %+v", got)
	}
	bal, err := b.ledger.Balance(ctx, v.Name)
	if err != nil || bal.NetMicros != 0 || len(bal.Credits) != 0 {
		t.Fatalf("the other account's balance: %+v, %v", bal, err)
	}
	for _, bad := range []billing.Grant{{Account: u.Name, Key: "x"}, {Key: "x", AmountMicros: 1}, {Account: u.Name, AmountMicros: 1}} {
		if _, _, err := b.ledger.EnsureGrant(ctx, bad); err == nil {
			t.Errorf("made %+v", bad)
		}
	}
}

// The order credit is used in: the plan's, the sign-up credit, purchased,
// an admin's; and each name the user sees.
func TestBalanceIsInTheOrderItIsUsed(t *testing.T) {
	b := newBooks()
	ctx := t.Context()
	u := b.account(t, "u@example.com")
	now := b.clock.Now()
	pack := grant(u.Name, billing.SourcePurchase, "purchase/cs_1", 20_000_000, now, 365)
	pack.Ref = &billing.GrantRef{PaymentIntent: "pi_1"}
	plan := grant(u.Name, billing.SourcePlan, "plan/sub_1/1", 10_000_000, now, 30)
	plan.Item = "cu_starter_monthly_v1"
	admin := billing.Grant{Account: u.Name, Source: billing.SourceAdmin, AmountMicros: 1_000_000, ValidFrom: now, Key: "admin/goodwill"}
	for _, g := range []billing.Grant{pack, admin, grant(u.Name, billing.SourceSignup, "signup/fpA", 5_000_000, now, 90), plan} {
		if created, _, err := b.ledger.EnsureGrant(ctx, g); err != nil || !created {
			t.Fatal(created, err)
		}
	}
	bal, err := b.ledger.Balance(ctx, u.Name)
	if err != nil || bal.NetMicros != 36_000_000 {
		t.Fatalf("%+v, %v", bal, err)
	}
	var order []string
	for _, c := range bal.Credits {
		order = append(order, c.Source)
	}
	if want := []string{"plan", "signup", "purchase", "admin"}; !reflect.DeepEqual(order, want) {
		t.Fatalf("order %v, want %v", order, want)
	}
	names := map[string]string{}
	for _, c := range b.metronome.AllCredits() {
		names[c.CustomFields[billing.FieldSource]] = c.Name
	}
	if names["plan"] != "Starter plan credit" || names["purchase"] != "Credit" || names["signup"] != "Sign-up credit" {
		t.Errorf("names %v", names)
	}
	// The earliest end of a credit with a balance is the plan's.
	if c := b.credit(t, u.Name); c.NextExpiryAt == nil || c.NextExpiryAt.After(now.AddDate(0, 0, 30).Add(time.Hour)) {
		t.Errorf("nextExpiryAt %v", c.NextExpiryAt)
	}

	// Revoke: a refund archives the pack found by its payment intent.
	if err := b.ledger.Revoke(ctx, billing.GrantSelector{Account: u.Name, PaymentIntent: "pi_1"}, "refund"); err != nil {
		t.Fatal(err)
	}
	if c := b.credit(t, u.Name); c.BalanceMicros != 16_000_000 || c.Exhausted {
		t.Fatalf("after the refund: %+v", c)
	}
	// Everything of the account: it is exhausted from now.
	b.clock.Advance(time.Hour)
	if err := b.ledger.Revoke(ctx, billing.GrantSelector{Account: u.Name}, "account-deleted"); err != nil {
		t.Fatal(err)
	}
	if c := b.credit(t, u.Name); !c.Exhausted || c.ExhaustedAt == nil || !c.ExhaustedAt.Equal(b.clock.Now()) || c.BalanceMicros != 0 || c.NextExpiryAt != nil {
		t.Fatalf("after everything was revoked: %+v", c)
	}
	// It keeps the time it became true.
	b.clock.Advance(time.Hour)
	if c, _ := b.ledger.EnsureCredit(ctx, u.Name); !c.ExhaustedAt.Equal(b.clock.Now().Add(-time.Hour)) {
		t.Fatalf("exhaustedAt moved: %v", c.ExhaustedAt)
	}
	if err := b.ledger.Revoke(ctx, billing.GrantSelector{PaymentIntent: "pi_1"}, "refund"); err == nil {
		t.Error("revoked with no account to look in")
	}
}

// Usage is Metronome's seconds, with money at the catalogue's rates.
func TestUsage(t *testing.T) {
	b := newBooks()
	ctx := t.Context()
	u := b.account(t, "u@example.com")
	if _, _, err := b.ledger.EnsureGrant(ctx, grant(u.Name, billing.SourceSignup, "signup/fpA", 5_000_000, b.clock.Now(), 90)); err != nil {
		t.Fatal(err)
	}
	s, _ := b.sessions.Create(ctx, "one", "u@example.com", nil)
	b.metronome.Tick(b.clock.Now())
	for range 60 {
		b.metronome.Tick(b.clock.Advance(time.Minute))
	}
	used, err := b.ledger.Usage(ctx, u.Name, t0.Truncate(24*time.Hour), b.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	if used.AwakeSeconds != 3600 || used.AwakeMicros != 200000 || used.DiskGBSeconds != 18000 || used.DiskMicros != 1920 {
		t.Fatalf("an hour awake: %+v", used)
	}
	if len(used.Sessions) != 1 || used.Sessions[0].ID != s.ID || used.Sessions[0].AwakeMicros != 200000 || len(used.Days) != 1 || used.Days[0].Date != "2026-10-02" {
		t.Fatalf("by session %+v, by day %+v", used.Sessions, used.Days)
	}
	if bal, _ := b.ledger.Balance(ctx, u.Name); bal.NetMicros != 5_000_000-201920 {
		t.Fatalf("balance %d", bal.NetMicros)
	}
}

// reads counts the calls made to an Accounts.
type reads struct {
	billing.Accounts
	gets int
}

func (r *reads) Get(ctx context.Context, name string) (billing.Account, error) {
	r.gets++
	return r.Accounts.Get(ctx, name)
}

// There is no copy of the ledger in the backend: the production Ledger is
// made of its clients and nothing else, two calls make two reads of
// Metronome and of the Account, and no billing package has an informer, a
// cache type or a package-level map of accounts or balances.
func TestNoLedgerCopy(t *testing.T) {
	b := newBooks()
	ctx := t.Context()
	u := b.account(t, "u@example.com")
	counted := &reads{Accounts: b.accounts}
	ledger := billing.NewLedger(b.metronome, counted, b.clock, rates)
	if _, _, err := ledger.EnsureGrant(ctx, grant(u.Name, billing.SourceSignup, "signup/fpA", 5_000_000, b.clock.Now(), 90)); err != nil {
		t.Fatal(err)
	}
	for _, call := range []struct {
		name string
		do   func() error
	}{
		{"Balance", func() error { _, err := ledger.Balance(ctx, u.Name); return err }},
		{"Usage", func() error { _, err := ledger.Usage(ctx, u.Name, t0, t0.Add(time.Hour)); return err }},
		{"EnsureCredit", func() error { _, err := ledger.EnsureCredit(ctx, u.Name); return err }},
	} {
		var metronome, account [2]int
		for i := range 2 {
			m, a := b.metronome.Calls(), counted.gets
			if err := call.do(); err != nil {
				t.Fatal(err)
			}
			metronome[i], account[i] = b.metronome.Calls()-m, counted.gets-a
		}
		if metronome[0] == 0 || metronome[1] != metronome[0] || account[0] == 0 || account[1] != account[0] {
			t.Errorf("%s: calls to Metronome %v, reads of the Account %v: the second call did not read again", call.name, metronome, account)
		}
	}
	// What Metronome says now is what is answered: nothing is remembered.
	b.sessions.Create(ctx, "one", "u@example.com", nil)
	b.metronome.Tick(b.clock.Now())
	b.metronome.Tick(b.clock.Advance(time.Minute))
	if bal, _ := ledger.Balance(ctx, u.Name); bal.NetMicros >= 5_000_000 {
		t.Errorf("the balance did not follow Metronome: %d", bal.NetMicros)
	}

	// The source of the billing packages that run in the backend.
	for _, dir := range []string{".", "kube", "metronome"} {
		entries, err := os.ReadDir(dir)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			raw, err := os.ReadFile(dir + "/" + name)
			if err != nil {
				t.Fatal(err)
			}
			src := string(raw)
			for _, banned := range []string{"informer", "Informer", "tools/cache", "LedgerView"} {
				if strings.Contains(src, banned) {
					// The words may appear in a comment that says there is none.
					for _, line := range strings.Split(src, "\n") {
						if strings.Contains(line, banned) && !strings.HasPrefix(strings.TrimSpace(line), "//") {
							t.Errorf("%s/%s: %q in code: %s", dir, name, banned, strings.TrimSpace(line))
						}
					}
				}
			}
			for _, line := range strings.Split(src, "\n") {
				if strings.HasPrefix(line, "var ") && strings.Contains(line, "map[") {
					t.Errorf("%s/%s: a package-level map: %s", dir, name, line)
				}
			}
		}
	}
}
