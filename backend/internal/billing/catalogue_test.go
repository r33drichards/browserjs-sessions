package billing

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const contractCatalogue = "../../../docs/contracts/billing/catalogue.yaml"

func TestTheContractsCatalogueParses(t *testing.T) {
	raw, err := os.ReadFile(contractCatalogue)
	if err != nil {
		t.Fatal(err)
	}
	c, err := ParseCatalogue(raw)
	if err != nil {
		t.Fatal(err)
	}
	if c.Rates != (Rates{AwakeMicrosPerHour: 200000, DiskMicrosPerGBHour: 384}) || c.SessionDiskGB != 5 {
		t.Errorf("rates %+v, disk %d", c.Rates, c.SessionDiskGB)
	}
	if c.SignupCredit.AmountMicros != 5000000 || c.SignupCredit.ValidDays != 90 || !c.SignupCredit.RefuseWallets || len(c.SignupCredit.RefuseFunding) != 1 {
		t.Errorf("signupCredit %+v", c.SignupCredit)
	}
	if got := c.Tier(PlanPayg); got.MaxSessions != 3 || got.MaxAwake != 2 {
		t.Errorf("payg %+v", got)
	}
	if got := c.Tier("pro"); got.MaxSessions != 10 || got.MaxAwake != 4 {
		t.Errorf("pro %+v", got)
	}
	// A subscriber stays on a plan that is no longer sold.
	if got := c.Tier("scale"); got.MaxSessions != 25 || got.MaxAwake != 8 {
		t.Errorf("scale %+v", got)
	}
	// A plan the catalogue does not have, like none, is pay as you go.
	if got := c.Tier("gone"); !reflect.DeepEqual(got, c.Payg) {
		t.Errorf("unknown plan %+v", got)
	}
	if p, ok := c.Pack("credit-20"); !ok || p.CreditMicros != 20000000 {
		t.Errorf("pack %+v %v", p, ok)
	}
	if p, ok := c.Pack("cu_credit_5_v1"); !ok || p.Key != "credit-5" {
		t.Errorf("pack by lookup key %+v %v", p, ok)
	}

	// What a caller may see: enabled items only, no product IDs.
	public, err := json.Marshal(c.Public())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(public), "scale") || strings.Contains(string(public), "productId") || strings.Contains(string(public), "cu_plan_") {
		t.Errorf("the public catalogue shows what it should not: %s", public)
	}
	for _, want := range []string{`"key":"starter"`, `"key":"pro"`, `"key":"credit-50"`, `"awakeMicrosPerHour":200000`, `"sessionDiskGB":5`, `"currency":"usd"`} {
		if !strings.Contains(string(public), want) {
			t.Errorf("the public catalogue lacks %s: %s", want, public)
		}
	}
}

func TestParseCatalogueRefusesWhatCannotBeEnforced(t *testing.T) {
	for name, yaml := range map[string]string{
		"not YAML":       "{",
		"an unknown key": "rates: {awakeMicrosPerHour: 1}\nsessionDiskGB: 5\npayg: {maxSessions: 1, maxAwake: 1}\nsurprise: true\n",
		"no rates":       "sessionDiskGB: 5\npayg: {maxSessions: 1, maxAwake: 1}\n",
		"no disk size":   "rates: {awakeMicrosPerHour: 1}\npayg: {maxSessions: 1, maxAwake: 1}\n",
		"no limits":      "rates: {awakeMicrosPerHour: 1}\nsessionDiskGB: 5\n",
		"a plan called payg": "rates: {awakeMicrosPerHour: 1}\nsessionDiskGB: 5\npayg: {maxSessions: 1, maxAwake: 1}\n" +
			"plans: [{key: payg, maxSessions: 1, maxAwake: 1}]\n",
	} {
		if _, err := ParseCatalogue([]byte(yaml)); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
}

// The file is re-read when it changes; one that does not parse keeps the
// last good one.
func TestCatalogueFileIsReRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalogue.yaml")
	write := func(yaml string, at time.Time) {
		t.Helper()
		if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
	}
	const good = "rates: {awakeMicrosPerHour: %RATE%}\nsessionDiskGB: 5\npayg: {maxSessions: 3, maxAwake: 2}\n"
	t0 := time.Now().Add(-time.Hour)
	write(strings.ReplaceAll(good, "%RATE%", "200000"), t0)
	f, err := LoadCatalogue(path)
	if err != nil {
		t.Fatal(err)
	}
	again := func() Catalogue {
		f.mu.Lock()
		f.checked = time.Time{} // as if catalogueRecheck had passed
		f.mu.Unlock()
		return f.Catalogue()
	}
	if got := again().Rates.AwakeMicrosPerHour; got != 200000 {
		t.Fatalf("rate %d", got)
	}
	write(strings.ReplaceAll(good, "%RATE%", "250000"), t0.Add(time.Minute))
	if got := again().Rates.AwakeMicrosPerHour; got != 250000 {
		t.Fatalf("after a change: rate %d, want 250000", got)
	}
	write("{ this is not a catalogue", t0.Add(2*time.Minute))
	if got := again().Rates.AwakeMicrosPerHour; got != 250000 {
		t.Fatalf("after a bad file: rate %d, want the last good one", got)
	}
	write(strings.ReplaceAll(good, "%RATE%", "300000"), t0.Add(3*time.Minute))
	if got := again().Rates.AwakeMicrosPerHour; got != 300000 {
		t.Fatalf("after a good file again: rate %d", got)
	}
	if _, err := LoadCatalogue(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Error("a missing file loaded")
	}
}

func TestGrantNameAndSelector(t *testing.T) {
	name := GrantName("signup/fpA")
	if len(name) != 42 || !strings.HasPrefix(name, "g-") || name != GrantName("signup/fpA") || name == GrantName("signup/fpB") {
		t.Errorf("GrantName = %q", name)
	}
	if got := AccountName("u@example.com"); len(got) != 37 || !strings.HasPrefix(got, "acct-") {
		t.Errorf("AccountName = %q", got)
	}
	now := time.Now()
	later, earlier := now.Add(time.Hour), now.Add(-time.Hour)
	g := Grant{Name: "g-1", Account: "acct-a", Source: SourcePlan, ExpiresAt: &later, Ref: &GrantRef{PaymentIntent: "pi_1"}}
	for _, c := range []struct {
		name string
		sel  GrantSelector
		want bool
	}{
		{"everything of the account", GrantSelector{Account: "acct-a"}, true},
		{"another account", GrantSelector{Account: "acct-b"}, false},
		{"by name", GrantSelector{Name: "g-1"}, true},
		{"by source", GrantSelector{Account: "acct-a", Source: SourcePurchase}, false},
		{"by payment intent", GrantSelector{PaymentIntent: "pi_1"}, true},
		{"another payment intent", GrantSelector{PaymentIntent: "pi_2"}, false},
		{"expiring after", GrantSelector{Account: "acct-a", Source: SourcePlan, ExpiresAfter: &now}, true},
		{"already expired then", GrantSelector{Account: "acct-a", ExpiresAfter: &later}, false},
		{"except itself", GrantSelector{Account: "acct-a", Except: "g-1"}, false},
		{"expiring after, long ago", GrantSelector{ExpiresAfter: &earlier}, true},
	} {
		if got := c.sel.Matches(g); got != c.want {
			t.Errorf("%s: %v", c.name, got)
		}
	}
	g.Revoked = &Revoked{Reason: "refund", At: now}
	if (GrantSelector{Account: "acct-a"}).Matches(g) {
		t.Error("a revoked grant is not revoked again")
	}
}
