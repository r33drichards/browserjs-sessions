package billing_test

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/r33drichards/computer-use/backend/internal/billing"
	"github.com/r33drichards/computer-use/backend/internal/billing/billingtest"
)

// The contract's catalogue: a rate for each size, twice and four times a
// small session's, and the sizes each plan includes.
func TestTheCatalogueOfSizes(t *testing.T) {
	data, err := os.ReadFile("../../../docs/contracts/billing/catalogue.yaml")
	if err != nil {
		t.Fatal(err)
	}
	c, err := billing.ParseCatalogue(data)
	if err != nil {
		t.Fatal(err)
	}
	for size, want := range map[string]int64{"": 200000, "small": 200000, "medium": 400000, "large": 800000, "unheard-of": 200000} {
		if got := c.AwakeRate(size); got != want {
			t.Errorf("%q: %d micro-dollars an hour, want %d", size, got, want)
		}
	}
	for plan, want := range map[string]string{"payg": "small medium", "starter": "small medium", "pro": "small medium large", "scale": "small medium large"} {
		var got []string
		for _, size := range []string{"small", "medium", "large"} {
			if c.Tier(plan).Allows(size) {
				got = append(got, size)
			}
		}
		if strings.Join(got, " ") != want {
			t.Errorf("%s includes %v, want %s", plan, got, want)
		}
	}
	public := c.Public()
	if !reflect.DeepEqual(public.Sizes, []billing.PublicSize{{"small", 200000}, {"medium", 400000}, {"large", 800000}}) {
		t.Errorf("public sizes %+v", public.Sizes)
	}
	if !reflect.DeepEqual(public.Payg.Sizes, []string{"small", "medium"}) {
		t.Errorf("public payg sizes %v", public.Payg.Sizes)
	}
	raw, _ := json.Marshal(public)
	for _, want := range []string{`"sizes":[{"key":"small","awakeMicrosPerHour":200000},{"key":"medium","awakeMicrosPerHour":400000},{"key":"large","awakeMicrosPerHour":800000}]`,
		`"key":"pro"`, `"sizes":["small","medium","large"]`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("the public catalogue lacks %s:\n%s", want, raw)
		}
	}
	// A pack has no sizes; a tier that names none has small.
	for _, p := range public.Packs {
		if p.Sizes != nil {
			t.Errorf("pack %s has sizes %v", p.Key, p.Sizes)
		}
	}
	if (billing.Tier{}).Allows("medium") || !(billing.Tier{}).Allows("small") || !(billing.Tier{}).Allows("") {
		t.Error("a tier that names no sizes must include small and only small")
	}

	for name, bad := range map[string]string{
		"small priced twice": "rates: {awakeMicrosPerHour: 1}\nsessionDiskGB: 5\npayg: {maxSessions: 1, maxAwake: 1}\nsizes: {small: {awakeMicrosPerHour: 2}}\n",
		"a size for nothing": "rates: {awakeMicrosPerHour: 1}\nsessionDiskGB: 5\npayg: {maxSessions: 1, maxAwake: 1}\nsizes: {medium: {awakeMicrosPerHour: 0}}\n",
	} {
		if _, err := billing.ParseCatalogue([]byte(bad)); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
}

// An hour awake costs the rate of the session's size; the disk is the same.
func TestUsageBySize(t *testing.T) {
	sized := rates
	sized.Sizes = map[string]billing.SizeRate{"medium": {AwakeMicrosPerHour: 400000}, "large": {AwakeMicrosPerHour: 800000}}
	b := &books{clock: billingtest.NewClock(t0)}
	b.accounts = billingtest.NewAccounts(b.clock)
	b.sessions = billingtest.NewSessions(b.clock)
	b.metronome = billingtest.NewMetronome(b.clock, b.sessions, sized)
	b.ledger = billing.NewLedger(b.metronome, b.accounts, b.clock, sized)
	ctx := t.Context()
	u := b.account(t, "u@example.com")
	if _, _, err := b.ledger.EnsureGrant(ctx, grant(u.Name, billing.SourceSignup, "signup/fpA", 5_000_000, b.clock.Now(), 90)); err != nil {
		t.Fatal(err)
	}
	small, _ := b.sessions.CreateSized(ctx, "s", "u@example.com", "", nil)
	large, _ := b.sessions.CreateSized(ctx, "l", "u@example.com", "large", nil)
	b.metronome.Tick(b.clock.Now())
	for range 60 {
		b.metronome.Tick(b.clock.Advance(time.Minute))
	}
	used, err := b.ledger.Usage(ctx, u.Name, t0.Truncate(24*time.Hour), b.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	// Two sessions for an hour: $0.20 and $0.80, and two 5 GB disks.
	if used.AwakeSeconds != 7200 || used.AwakeMicros != 1_000_000 || used.DiskMicros != 3840 {
		t.Fatalf("%+v", used)
	}
	by := map[string]int64{}
	for _, s := range used.Sessions {
		by[s.ID] = s.AwakeMicros
		if s.AwakeSeconds != 3600 {
			t.Errorf("%s: %d awake seconds", s.ID, s.AwakeSeconds)
		}
	}
	if by[small.ID] != 200000 || by[large.ID] != 800000 {
		t.Errorf("by session %v", by)
	}
	if len(used.Days) != 1 || used.Days[0].AwakeMicros != 1_000_000 {
		t.Errorf("by day %+v", used.Days)
	}
	// What Metronome drew is what the usage says.
	if bal, _ := b.ledger.Balance(ctx, u.Name); bal.NetMicros != 5_000_000-1_000_000-3840 {
		t.Errorf("balance %d", bal.NetMicros)
	}
}
