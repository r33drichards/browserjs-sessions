#!/usr/bin/env bash
# Sandbox check 3 (tracks, "Track C"): a subscription's life on a test clock.
#
#   subscribe; a month passes (a new period, paid: a new plan Grant, the old
#   credit gone); upgrade (a new period from now, charged at once: the old
#   Grant superseded); downgrade (waits for the period's end); a renewal that
#   fails (past_due, no Grant); cancel (pay as you go at the period's end).
#
# Apply infra/billing to the sandbox first: it makes the prices this looks up. The
# upgrade and the downgrade here are made through the API with the settings
# the portal configuration asks for; doing them in the portal itself, on the
# link printed, is the part that confirms the configuration (stripe.md).
. "$(dirname "$0")/lib.sh"

price() { api GET /v1/prices -G -d "lookup_keys[]=$1" -d active=true | jq -r '.data[0].id // empty'; }
STARTER=$(price cu_starter_monthly_v1)
PRO=$(price cu_pro_monthly_v1)
[ -n "$STARTER" ] && [ -n "$PRO" ] || { echo "no prices: apply infra/billing to the sandbox first" >&2; exit 1; }

say "a test clock, and a customer on it with a card that works"
NOW=$(date +%s)
CLOCK=$(api POST /v1/test_helpers/test_clocks -d "frozen_time=$NOW" -d name=check3 | jq -r .id)
CUS=$(api POST /v1/customers -d "test_clock=$CLOCK" -d "email=check3@example.com" | jq -r .id)
PM=$(api POST /v1/payment_methods/pm_card_visa/attach -d "customer=$CUS" | jq -r .id)
api POST "/v1/customers/$CUS" -d "invoice_settings[default_payment_method]=$PM" >/dev/null
echo "$CLOCK $CUS $PM"

# What ensureSubscription reads: the status, the period (on the item), the
# price, the latest invoice.
show() {
  api GET "/v1/subscriptions/$SUB" -G -d "expand[]=latest_invoice" |
    jq '{status, cancel_at, cancel_at_period_end, schedule,
         item: (.items.data[0] | {lookup_key: .price.lookup_key, current_period_start, current_period_end}),
         latest_invoice: (.latest_invoice | {id, status, billing_reason, amount_due, amount_paid})}'
}

# advance SECONDS: move the clock on and wait for Stripe to catch up.
advance() {
  NOW=$((NOW + $1))
  api POST "/v1/test_helpers/test_clocks/$CLOCK/advance" -d "frozen_time=$NOW" >/dev/null
  until [ "$(api GET "/v1/test_helpers/test_clocks/$CLOCK" | jq -r .status)" = ready ]; do sleep 3; done
}

say "subscribe to Starter"
SUB=$(api POST /v1/subscriptions -d "customer=$CUS" -d "items[0][price]=$STARTER" -d "metadata[kind]=plan" | jq -r .id)
show
events

say "a month passes: expect a new period, and latest_invoice paid (billing_reason subscription_cycle)"
STARTED=$(date +%s)
advance $((32 * 86400))
show
events

say "upgrade to Pro (proration_behavior=always_invoice, billing_cycle_anchor=now): expect a period starting now, an invoice paid at once"
STARTED=$(date +%s)
ITEM=$(api GET "/v1/subscriptions/$SUB" | jq -r '.items.data[0].id')
api POST "/v1/subscriptions/$SUB" -d "items[0][id]=$ITEM" -d "items[0][price]=$PRO" \
  -d proration_behavior=always_invoice -d billing_cycle_anchor=now >/dev/null
sleep 5
show
echo "the upgrade invoice's lines (SubscriptionPeriodOf takes the latest period start):"
INV=$(api GET "/v1/subscriptions/$SUB" | jq -r .latest_invoice)
api GET "/v1/invoices/$INV" | jq '[.lines.data[] | {amount, period, description}]'
events

say "the same in the portal, by hand: a downgrade should WAIT for the period's end (schedule_at_period_end)"
api POST /v1/billing_portal/sessions -d "customer=$CUS" -d "return_url=https://example.com/billing" | jq -r .url
pause "downgrade to Starter in the portal, then"
show

say "a renewal that fails: the card is replaced by one that declines, and a month passes"
STARTED=$(date +%s)
BAD=$(api POST /v1/payment_methods/pm_card_chargeCustomerFail/attach -d "customer=$CUS" | jq -r .id)
api POST "/v1/customers/$CUS" -d "invoice_settings[default_payment_method]=$BAD" >/dev/null
api POST "/v1/subscriptions/$SUB" -d "default_payment_method=$BAD" >/dev/null
advance $((32 * 86400))
echo "expect status past_due and latest_invoice open: no Grant"
show
events

say "cancel at the period's end, with a card that works again; then the period ends"
STARTED=$(date +%s)
api POST "/v1/customers/$CUS" -d "invoice_settings[default_payment_method]=$PM" >/dev/null
api POST "/v1/subscriptions/$SUB" -d "default_payment_method=$PM" -d cancel_at_period_end=true >/dev/null
echo "expect cancel_at set (cancelAt in the Account):"
show
advance $((32 * 86400))
echo "expect status canceled:"
show
events

say "done. The clock and its customer can be deleted:"
echo "curl -X DELETE $API/v1/test_helpers/test_clocks/$CLOCK -u \"\$STRIPE_API_KEY:\""
