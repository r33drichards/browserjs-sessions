#!/usr/bin/env bash
# Sandbox check 2 (tracks, "Track C"): a setup-mode Checkout.
#
# Answers, for docs/billing-stripe.md:
#   a. which events arrive, and in what order;
#   b. that the card is attached to the customer;
#   c. what payment_method.detached carries (is customer null?) when the card
#      is removed through the API, and in the portal;
#   d. whether the portal lets a customer with no subscription remove their
#      last card;
#   e. the fingerprint of the same test card saved twice, and through a
#      wallet.
#
# It needs a person at a browser for the two Checkouts and the portal.
. "$(dirname "$0")/lib.sh"

say "a customer"
CUS=$(api POST /v1/customers -d "email=check2@example.com" -d "metadata[check]=2" | jq -r .id)
echo "$CUS"

checkout() {
  api POST /v1/checkout/sessions -d mode=setup -d currency=usd -d "customer=$CUS" \
    -d "client_reference_id=acct-check" -d "metadata[kind]=setup" -d "setup_intent_data[metadata][kind]=setup" \
    -d "success_url=https://example.com/billing?checkout={CHECKOUT_SESSION_ID}" -d "cancel_url=https://example.com/billing" |
    jq -r '"\(.id) \(.url)"'
}

cards() {
  api GET "/v1/customers/$CUS/payment_methods" |
    jq -r '.data[] | "\(.id) type=\(.type) fingerprint=\(.card.fingerprint) funding=\(.card.funding) wallet=\(.card.wallet.type // "none") created=\(.created)"'
}

say "a setup Checkout: open it and save 4242 4242 4242 4242 (any future date, any CVC)"
read -r CS URL < <(checkout)
echo "$URL"
pause "when Checkout says it is done"

say "(a) the events, in order"
events
say "(b) the session, and the customer's payment methods"
api GET "/v1/checkout/sessions/$CS" | jq '{status, payment_status, mode, customer, setup_intent, client_reference_id}'
cards
say "the customer's default (invoice_settings.default_payment_method): is a card saved by a setup Checkout made the default?"
api GET "/v1/customers/$CUS" | jq '.invoice_settings.default_payment_method'

say "(e) the same card saved a second time: open it and save 4242 4242 4242 4242 again"
read -r _ URL < <(checkout)
echo "$URL"
pause "when Checkout says it is done"
cards
echo "The two fingerprints above should be the same. For a wallet: repeat by hand with Apple Pay or Google Pay"
echo "in a browser that has one, and note wallet= and the fingerprint (expected: a different one, the device's)."

say "(c) one card detached through the API"
PM=$(api GET "/v1/customers/$CUS/payment_methods" | jq -r '.data[0].id')
STARTED=$(date +%s)
api POST "/v1/payment_methods/$PM/detach" | jq '{id, customer}'
sleep 5
echo "the event's object (the contract expects customer: null, and previous_attributes to name the customer):"
api GET /v1/events -G -d type=payment_method.detached -d limit=1 | jq '.data[0] | {type, object: (.data.object | {id, customer}), previous_attributes: .data.previous_attributes}'

say "(c, d) the last card removed in the portal: open it and try to remove the card"
api POST /v1/billing_portal/sessions -d "customer=$CUS" -d "return_url=https://example.com/billing" | jq -r .url
pause "when you have removed it, or found that you cannot"
STARTED=$((STARTED - 1))
events payment_method.
cards
echo "No card listed above: the portal lets a customer with no subscription remove their last card (d: yes)."
