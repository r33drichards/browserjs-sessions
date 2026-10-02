package config

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/r33drichards/computer-use/backend/internal/billing"
)

func withRequired(m map[string]string) func(string) string {
	all := map[string]string{
		"PUBLIC_URL":           "https://app.example.com",
		"SESSION_URL_TEMPLATE": "https://sessions.example.com/{id}",
		"POMERIUM_JWKS_URL":    "http://pomerium/jwks.json",
	}
	for k, v := range m {
		all[k] = v
	}
	return env(all)
}

// Billing is off by default, and everything else has deploy.md's default.
func TestBillingDefaults(t *testing.T) {
	c, err := FromEnv(withRequired(map[string]string{"ADMIN_EMAILS": "Root@Example.com"}))
	if err != nil {
		t.Fatal(err)
	}
	b := c.Billing
	if b.Mode != billing.Off {
		t.Fatalf("BILLING unset: mode %q, want off", b.Mode)
	}
	if b.Grace != 5*time.Minute || b.DrainTimeout != 10*time.Minute || b.BalancePass != 5*time.Minute || b.ZeroBalanceDeleteAfter != 336*time.Hour {
		t.Errorf("durations %+v", b)
	}
	if b.MaxAwakeSessions != 10 || b.WakesPerHour != 30 || b.ZeroBalanceDelete || !b.SignupCredit || b.Payments != "off" {
		t.Errorf("defaults %+v", b)
	}
	if !slices.Equal(b.ExemptEmails, []string{"root@example.com"}) {
		t.Errorf("exempt %q, want the admins", b.ExemptEmails)
	}
	if c.BillingCatalogue != "/etc/browserjs/catalogue.yaml" || b.PublicURL != "https://app.example.com" || b.BillingURL() != "https://app.example.com/billing" {
		t.Errorf("catalogue %q, public URL %q", c.BillingCatalogue, b.PublicURL)
	}
}

func TestBillingFromEnv(t *testing.T) {
	c, err := FromEnv(withRequired(map[string]string{
		"BILLING": "enforce", "STRIPE_MODE": "test", "ADMIN_EMAILS": "root@example.com",
		"API_URL": "https://api.example.com", "STRIPE_API_KEY": "rk_test_madeUp", "STRIPE_WEBHOOK_SECRET": "whsec_madeUp",
		"METRONOME_API_TOKEN": "made-up", "METRONOME_WEBHOOK_SECRET": "made-up-too", "METRONOME_URL": "http://metronome.test/",
		"BILLING_GRACE": "1m", "BILLING_DRAIN_TIMEOUT": "2m", "BILLING_BALANCE_PASS": "3m", "ZERO_BALANCE_DELETE_AFTER": "24h",
		"BILLING_EXEMPT_EMAILS": " Guest@Example.com , ", "MAX_AWAKE_SESSIONS": "4", "WAKES_PER_HOUR": "7",
		"ZERO_BALANCE_DELETE": "on", "SIGNUP_CREDIT": "off", "BILLING_CATALOGUE": "/tmp/catalogue.yaml",
	}))
	if err != nil {
		t.Fatal(err)
	}
	b := c.Billing
	if b.Mode != billing.Enforce || b.Payments != "test" || b.Grace != time.Minute || b.DrainTimeout != 2*time.Minute ||
		b.BalancePass != 3*time.Minute || b.ZeroBalanceDeleteAfter != 24*time.Hour || b.MaxAwakeSessions != 4 || b.WakesPerHour != 7 ||
		!b.ZeroBalanceDelete || b.SignupCredit || c.BillingCatalogue != "/tmp/catalogue.yaml" {
		t.Errorf("%+v", b)
	}
	if c.MetronomeToken != "made-up" || c.MetronomeWebhookSecret != "made-up-too" || c.MetronomeURL != "http://metronome.test" {
		t.Errorf("metronome: %q", c.MetronomeURL)
	}
	// The list replaces the admins: an admin not on it is not exempt.
	if !slices.Equal(b.ExemptEmails, []string{"guest@example.com"}) {
		t.Errorf("exempt %q", b.ExemptEmails)
	}
	if c, err := FromEnv(withRequired(map[string]string{"BILLING": "meter", "METRONOME_API_TOKEN": "made-up"})); err != nil || c.Billing.Mode != billing.Meter {
		t.Errorf("BILLING=meter without Stripe: %v, %v", c.Billing.Mode, err)
	}
	if c, err := FromEnv(withRequired(map[string]string{"BILLING": "off"})); err != nil || c.Billing.Mode != billing.Off {
		t.Errorf("BILLING=off: %v, %v", c.Billing.Mode, err)
	}
}

func TestBillingRefusesWhatItCannotRun(t *testing.T) {
	for name, vars := range map[string]map[string]string{
		"an unknown mode":              {"BILLING": "on"},
		"enforce without Stripe":       {"BILLING": "enforce", "METRONOME_API_TOKEN": "made-up"},
		"metering without Metronome":   {"BILLING": "meter"},
		"a grace that is no duration":  {"BILLING_GRACE": "soon"},
		"a drain timeout of zero":      {"BILLING_DRAIN_TIMEOUT": "0s"},
		"no places":                    {"MAX_AWAKE_SESSIONS": "0"},
		"wakes that are not a number":  {"WAKES_PER_HOUR": "many"},
		"a switch that is neither":     {"ZERO_BALANCE_DELETE": "maybe"},
		"a sign-up credit that is not": {"SIGNUP_CREDIT": "5"},
	} {
		if _, err := FromEnv(withRequired(vars)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	_, err := FromEnv(withRequired(map[string]string{"BILLING": "enforce", "METRONOME_API_TOKEN": "made-up"}))
	if err == nil || !strings.Contains(err.Error(), "STRIPE_MODE") {
		t.Errorf("enforce without Stripe: %v, want it to name STRIPE_MODE", err)
	}
}

// The token is named when it is missing, and never shown when it is there.
func TestMetronomeToken(t *testing.T) {
	_, err := FromEnv(withRequired(map[string]string{"BILLING": "meter"}))
	if err == nil || !strings.Contains(err.Error(), "METRONOME_API_TOKEN") {
		t.Fatalf("err = %v", err)
	}
	_, err = FromEnv(withRequired(map[string]string{"BILLING": "enforce", "METRONOME_API_TOKEN": "s3cret-value"}))
	if err == nil || strings.Contains(err.Error(), "s3cret-value") {
		t.Fatalf("err = %v", err)
	}
	// With billing off Metronome is not needed.
	if _, err := FromEnv(withRequired(nil)); err != nil {
		t.Fatal(err)
	}
}
