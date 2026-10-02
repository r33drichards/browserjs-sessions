#!/usr/bin/env bash
# Sandbox check 4 (tracks, "Track C"), second half: an off-session charge on
# a test card that requires authentication. Answers, for
# docs/billing-stripe.md: the error and the events, as ensureRecharge
# expects them (the PaymentIntent in the error, status
# requires_payment_method, code authentication_required).
#
# The first half (the smallest set of permissions for the restricted key) is
# by hand: docs/billing-stripe.md, "The restricted key".
. "$(dirname "$0")/lib.sh"

charge() { # NAME TOKEN
  say "$1"
  local cus pm
  cus=$(api POST /v1/customers -d "email=check4@example.com" | jq -r .id)
  pm=$(api POST "/v1/payment_methods/$2/attach" -d "customer=$cus" | jq -r .id)
  api POST /v1/payment_intents -d amount=2000 -d currency=usd -d "customer=$cus" -d "payment_method=$pm" \
    -d off_session=true -d confirm=true -d "metadata[kind]=recharge" -d "metadata[account]=acct-check" \
    -H "Idempotency-Key: check4-$cus" |
    jq '{id, status, error: (.error // null | if . then {type, code, decline_code, payment_intent: (.payment_intent | {id, status, last_payment_error: (.last_payment_error | {code, decline_code})})} else null end)}'
}

charge "a card that works: expect status succeeded, error null" pm_card_visa
charge "a card that requires authentication (4000 0025 0000 3155): expect error.code authentication_required, payment_intent.status requires_payment_method" pm_card_authenticationRequired
charge "a card that saves and then declines (4000 0000 0000 0341): expect error.code card_declined" pm_card_chargeCustomerFail

sleep 5
say "the events (expect payment_intent.succeeded for the first, payment_intent.payment_failed for the others)"
events payment_intent.
