#!/usr/bin/env bash
# Sandbox check 1 (tracks, "Track C"): an event sent by Stripe reaches the
# webhook handler with a signature that verifies, through whatever is in
# front of it: the body and the Stripe-Signature header are untouched.
#
#   1-webhook-signature.sh                      # straight to the handler, on this machine
#   1-webhook-signature.sh https://api.<local domain>/stripe/webhook
#                                               # through Pomerium, to a backend running with STRIPE_MODE=test
#
# With no argument it runs the real handler on in-memory fakes
# (checks/webhookcheck), which is enough to see a signature verify. Through
# Pomerium, the backend must be the one answering: read its log.
. "$(dirname "$0")/lib.sh"
command -v stripe >/dev/null || { echo "the Stripe CLI is needed: https://docs.stripe.com/stripe-cli" >&2; exit 2; }

TARGET=${1:-}
# The CLI takes the key from STRIPE_API_KEY. Its signing secret is stable
# for a key; it is passed to the handler
# in its environment, not printed.
SECRET=$(stripe listen --print-secret)
cleanup() { kill "${LISTEN:-}" "${HANDLER:-}" 2>/dev/null || true; }
trap cleanup EXIT

if [ -z "$TARGET" ]; then
  TARGET=http://127.0.0.1:8099/stripe/webhook
  (cd "$(dirname "$0")/../../../.." && STRIPE_WEBHOOK_SECRET=$SECRET go run ./internal/billing/stripe/checks/webhookcheck --listen 127.0.0.1:8099) &
  HANDLER=$!
  sleep 8
else
  echo "the backend behind $TARGET must have STRIPE_WEBHOOK_SECRET set to the CLI's secret:"
  echo "  stripe listen --print-secret   (put it in the untracked .env; docs/billing-development.md)"
fi

say "forwarding Stripe's events to $TARGET"
stripe listen --forward-to "$TARGET" --skip-verify &
LISTEN=$!
sleep 5

say "stripe trigger checkout.session.completed"
stripe trigger checkout.session.completed
sleep 5
say "Above, each forwarded event should show [200]. A [400] is a signature that did not verify:"
echo "the body or the Stripe-Signature header was changed on the way, or the secret is not the CLI's."
