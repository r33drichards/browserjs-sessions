package config

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/r33drichards/browserjs-sessions/backend/internal/billing"
	"github.com/r33drichards/browserjs-sessions/backend/internal/billing/stripe"
)

// Secret is a value that is never printed: formatted or logged, it is
// "[redacted]". Reveal is the value.
type Secret string

func (s Secret) Reveal() string { return string(s) }

func (Secret) String() string       { return "[redacted]" }
func (Secret) GoString() string     { return "[redacted]" }
func (Secret) LogValue() slog.Value { return slog.StringValue("[redacted]") }

// MarshalText keeps it out of JSON, and of anything else that encodes.
func (Secret) MarshalText() ([]byte, error) { return []byte("[redacted]"), nil }

// stripeFromEnv reads STRIPE_MODE, STRIPE_API_KEY and
// STRIPE_WEBHOOK_SECRET, after billingFromEnv. No error says a key or a
// secret.
func (c *Config) stripeFromEnv(get func(string) string) error {
	switch c.StripeMode = get("STRIPE_MODE"); c.StripeMode {
	case "":
		return nil
	case "test", "live":
	default:
		return fmt.Errorf("STRIPE_MODE must be test or live, got %q", c.StripeMode)
	}
	if c.Billing.Mode == billing.Off {
		return errors.New("STRIPE_MODE requires BILLING to be meter or enforce")
	}
	if c.APIURL == "" {
		return errors.New("STRIPE_MODE requires API_URL: Stripe's webhook is on the API host")
	}
	c.StripeAPIKey, c.StripeWebhookSecret = Secret(get("STRIPE_API_KEY")), Secret(get("STRIPE_WEBHOOK_SECRET"))
	if c.StripeAPIKey == "" || c.StripeWebhookSecret == "" {
		return errors.New("STRIPE_API_KEY and STRIPE_WEBHOOK_SECRET are required with STRIPE_MODE")
	}
	// A live key in test mode, or the reverse, would charge real cards for
	// a rehearsal, or take a real payment for nothing.
	mode, err := stripe.KeyMode(c.StripeAPIKey.Reveal())
	if err != nil {
		return fmt.Errorf("STRIPE_API_KEY is %w", err)
	}
	if mode != c.StripeMode {
		return fmt.Errorf("STRIPE_API_KEY is a %s key and STRIPE_MODE is %s", mode, c.StripeMode)
	}
	if c.BillingIDs = get("BILLING_IDS"); c.BillingIDs == "" {
		c.BillingIDs = "/etc/browserjs/billing-iac/ids.json"
	}
	return nil
}
