// Package stripe is the backend's dealings with Stripe: the checkout, portal
// and auto-recharge routes, the webhook, the ensure functions every path
// ends in, and the reconcile. Contract: docs/contracts/billing/stripe.md.
//
// Nothing here decides whether a session may run; that is enforcement's
// (internal/billing). This package writes what Stripe says into an
// Account's spec (the card, the subscription) and makes the credit that was
// paid for.
//
// Its stateful dependencies are the interfaces of internal/billing
// (Accounts, Ledger, Stripe, Clock); of the ledger it calls EnsureGrant and
// Revoke, and reads no balance. No test here reaches Stripe, Metronome or a
// Kubernetes API.
package stripe

import (
	"context"
	"time"
)

// PeriodFinder is a billing.Stripe that can also say which subscription
// period a payment paid for (GET /v1/invoice_payments): what a refund of a
// subscription's charge needs. A client without it revokes packs only.
type PeriodFinder interface {
	// SubscriptionPeriodOf: ok is false when the PaymentIntent paid no
	// subscription's invoice.
	SubscriptionPeriodOf(ctx context.Context, paymentIntent string) (p PaidPeriod, ok bool, err error)
}

// PaidPeriod is the subscription period an invoice was for.
type PaidPeriod struct {
	Subscription string
	Invoice      string
	Start        time.Time
}

// The outcomes of an automatic charge (autoRecharge.last.status), and why
// auto-recharge was turned off (autoRecharge.disabledReason).
const (
	RechargePending   = "pending"
	RechargeSucceeded = "succeeded"
	RechargeFailed    = "failed"

	DisabledPaymentFailed = "payment-failed"
	DisabledAuthRequired  = "authentication-required"
	DisabledNoCard        = "no-card"
	DisabledCapReached    = "cap-reached"
)

// Why a Grant is revoked (revoked.reason).
const (
	RevokedRefund     = "refund"
	RevokedDispute    = "dispute"
	RevokedSuperseded = "superseded"
)

// The codes of this package's own refusals (Error.code of
// backend-api.yaml); the rest are billing's.
const (
	CodeAlreadySubscribed = "already_subscribed"
	CodeNoCustomer        = "no_customer"
	CodeUnknownItem       = "unknown_item"
	CodePaymentsOff       = "payments_off"
	CodeTooManyCards      = "too_many_cards"
	CodeAutoRechargeOff   = "auto_recharge_off"
	CodeInvalidRequest    = "invalid_request"
)
