package config

import (
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// Made up: they open nothing.
const (
	testKey = "rk_test_madeUpForTests"
	liveKey = "rk_live_madeUpForTests"
	whsec   = "whsec_madeUpForTests"
)

func stripeEnv(more map[string]string) func(string) string {
	m := map[string]string{
		"PUBLIC_URL":            "https://app.example.com",
		"SESSION_URL_TEMPLATE":  "https://sessions.example.com/{id}",
		"POMERIUM_JWKS_URL":     "http://pomerium/jwks.json",
		"API_URL":               "https://api.example.com",
		"BILLING":               "meter",
		"METRONOME_API_TOKEN":   "madeUpForTests",
		"STRIPE_MODE":           "test",
		"STRIPE_API_KEY":        testKey,
		"STRIPE_WEBHOOK_SECRET": whsec,
	}
	for k, v := range more {
		if v == "" {
			delete(m, k)
		} else {
			m[k] = v
		}
	}
	return env(m)
}

// With nothing set there is no Stripe, and nothing about it.
func TestStripeOffByDefault(t *testing.T) {
	c, err := FromEnv(stripeEnv(map[string]string{"BILLING": "", "STRIPE_MODE": "", "STRIPE_API_KEY": "", "STRIPE_WEBHOOK_SECRET": ""}))
	if err != nil {
		t.Fatal(err)
	}
	if c.StripeMode != "" || c.StripeAPIKey != "" || c.StripeWebhookSecret != "" {
		t.Errorf("Stripe is not off: %+v", c)
	}
	// A key left in the environment without STRIPE_MODE is not read.
	c, err = FromEnv(stripeEnv(map[string]string{"BILLING": "", "STRIPE_MODE": ""}))
	if err != nil || c.StripeAPIKey != "" || c.StripeWebhookSecret != "" {
		t.Errorf("err %v, config %+v", err, c)
	}
}

func TestStripeOn(t *testing.T) {
	c, err := FromEnv(stripeEnv(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.StripeMode != "test" || c.StripeAPIKey.Reveal() != testKey || c.StripeWebhookSecret.Reveal() != whsec || c.Billing.AutoRecharge || c.Billing.Payments != "test" {
		t.Errorf("config = %+v", c)
	}
	c, err = FromEnv(stripeEnv(map[string]string{"STRIPE_MODE": "live", "STRIPE_API_KEY": liveKey, "AUTO_RECHARGE": "on", "BILLING": "enforce"}))
	if err != nil || c.StripeMode != "live" || !c.Billing.AutoRecharge {
		t.Errorf("err %v, config %+v", err, c)
	}
}

// A live key with STRIPE_MODE=test refuses to start, and the reverse; no
// error says the key or the secret.
func TestStripeRefusesToStart(t *testing.T) {
	for name, c := range map[string]struct {
		env  map[string]string
		want string
	}{
		"a live key in test mode":   {map[string]string{"STRIPE_API_KEY": liveKey}, "a live key and STRIPE_MODE is test"},
		"a test key in live mode":   {map[string]string{"STRIPE_MODE": "live"}, "a test key and STRIPE_MODE is live"},
		"a live secret key in test": {map[string]string{"STRIPE_API_KEY": "sk_live_madeUpForTests"}, "a live key and STRIPE_MODE is test"},
		"a publishable key":         {map[string]string{"STRIPE_API_KEY": "pk_test_madeUpForTests"}, "not a Stripe secret key"},
		"no key":                    {map[string]string{"STRIPE_API_KEY": ""}, "are required"},
		"no webhook secret":         {map[string]string{"STRIPE_WEBHOOK_SECRET": ""}, "are required"},
		"a mode that is not one":    {map[string]string{"STRIPE_MODE": "sandbox"}, "must be test or live"},
		"billing off":               {map[string]string{"BILLING": "off"}, "requires BILLING"},
		"billing unset":             {map[string]string{"BILLING": ""}, "requires BILLING"},
		"no API host":               {map[string]string{"API_URL": ""}, "requires API_URL"},
	} {
		_, err := FromEnv(stripeEnv(c.env))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", name, err, c.want)
			continue
		}
		for _, secret := range []string{testKey, liveKey, whsec, "madeUpForTests"} {
			if strings.Contains(err.Error(), secret) {
				t.Errorf("%s: the error says a secret: %v", name, err)
			}
		}
	}
}

// However the configuration is printed, the key and the secret are not.
func TestStripeSecretsAreNotPrinted(t *testing.T) {
	c, err := FromEnv(stripeEnv(nil))
	if err != nil {
		t.Fatal(err)
	}
	var log strings.Builder
	slog.New(slog.NewTextHandler(&log, nil)).Info("config", "key", c.StripeAPIKey, "secret", c.StripeWebhookSecret, "all", fmt.Sprintf("%+v", c))
	slog.New(slog.NewJSONHandler(&log, nil)).Info("config", "key", c.StripeAPIKey, "cfg", c)
	printed := log.String() + fmt.Sprintf("%v %+v %#v %s %q", c, c, c, c.StripeAPIKey, c.StripeWebhookSecret)
	for _, secret := range []string{testKey, whsec, "madeUpForTests"} {
		if strings.Contains(printed, secret) {
			t.Errorf("%q was printed", secret)
		}
	}
	if !strings.Contains(printed, "[redacted]") {
		t.Error("nothing stands for the secrets")
	}
}
