# Stripe contract

Which Stripe objects exist, who makes them, which calls the backend makes,
which events it handles and what each does. Sources for every Stripe fact
are in section 1 of the design.

## Division of labour

| | Makes it | With |
|---|---|---|
| The Stripe account, its sandbox, the webhook endpoint, the API keys | the product owner, by hand in the Dashboard | the to-do list of the design |
| Products, Prices, the Customer Portal configuration | `backend/cmd/stripe-setup`, run by the manual workflow `stripe-setup.yml` | `STRIPE_SETUP_KEY` |
| Customers, Checkout Sessions, Portal sessions | the backend, at run time | `STRIPE_API_KEY` |
| Subscriptions, Invoices, PaymentIntents, Refunds, Disputes | Stripe | |

Nothing in the cluster other than the backend has a Stripe key or talks to
Stripe. The operator does not.

## Object catalogue

Made from `catalogue.yaml` by the setup command. It is idempotent: run
twice, the second run changes nothing and says so.

| Object | Identity | Fields |
|---|---|---|
| Product, one per plan | `id` = `productId` (`cu_plan_starter`, `cu_plan_pro`, `cu_plan_scale`) | `name` "Computer Use <Name>", `metadata.catalogue_key`, `active` = `enabled` |
| Product for packs | `id` = `cu_pack_hours` | `name` "Computer Use hours" |
| Price, one per plan | `lookup_key` (`cu_starter_monthly_v1`, `cu_pro_monthly_v1`, `cu_scale_monthly_v1`) | `unit_amount`, `currency: usd`, `recurring.interval: month`, licensed (not metered), `metadata.hours` (for people; the backend does not read it) |
| Price, one per pack | `lookup_key` (`cu_hours_20_v1`, `cu_hours_100_v1`) | `unit_amount`, one-off, `metadata.hours` |
| Customer Portal configuration | the one with `metadata.managed_by = stripe-setup`; made if none | below |

A price's amount cannot be edited in Stripe. To change one: add an entry
with a new `lookupKey` (`..._v2`) to `catalogue.yaml`, keep the old entry
with `enabled: false` (existing subscribers stay on it and their grants
still need its hours), and run the setup again. `transfer_lookup_key` is
not used: a lookup key always means the same amount and hours.

Portal configuration:

| Feature | Setting |
|---|---|
| `invoice_history`, `payment_method_update`, `customer_update` (email, address, tax ID) | enabled |
| `subscription_cancel` | enabled, `mode: at_period_end` |
| `subscription_update` | enabled, `default_allowed_updates: [price]`, `products`: every enabled plan with its one price |
| `subscription_update.proration_behavior` | `always_invoice` |
| `subscription_update.billing_cycle_anchor` | `now` (an upgrade starts a new period, paid in full, less the unused time of the old) |
| `subscription_update.schedule_at_period_end.conditions` | `decreasing_item_amount` (a downgrade waits for the period's end) |

That `billing_cycle_anchor: now` together with `schedule_at_period_end`
behaves as described is to be confirmed in the sandbox with a test clock
(track A, first task). If it does not, the fallback is `proration_behavior:
none` with `billing_cycle_anchor: unchanged`: the plan changes at once, the
price from the next period, and the "Plan grants" rule below still holds.

## Calls the backend makes

All with `Idempotency-Key` where noted, a 10 s timeout, and the SDK's two
automatic retries.

| When | Call | Notes |
|---|---|---|
| Start-up, and hourly | `GET /v1/prices?lookup_keys[]=...` (at most 10 per call) | builds the map lookup key to price ID; a missing enabled key is logged and that item is not offered |
| First checkout of an account | `POST /v1/customers` | `email`, `metadata.account`, `metadata.owner_hash`; `Idempotency-Key: customer-<ownerHash>`; the ID is written to `spec.stripeCustomerId` before the Checkout Session is made |
| Subscribe | `POST /v1/checkout/sessions` | `mode: subscription`, `customer`, `line_items[0].price`, `client_reference_id` = the Account's name, `metadata` and `subscription_data.metadata` = `{account, item}`, `success_url: <PUBLIC_URL>/billing?checkout={CHECKOUT_SESSION_ID}`, `cancel_url: <PUBLIC_URL>/billing`, `automatic_tax` off, `consent_collection.terms_of_service: required` when the terms URL is set in the Dashboard |
| Buy a pack | `POST /v1/checkout/sessions` | `mode: payment`, `customer`, one line, quantity 1, `metadata` and `payment_intent_data.metadata` = `{account, item}`, the same URLs |
| Manage billing | `POST /v1/billing_portal/sessions` | `customer`, `return_url: <PUBLIC_URL>/billing` |
| Reading, on events and in the reconcile | `GET /v1/subscriptions/{id}`, `GET /v1/checkout/sessions/{id}`, `GET /v1/subscriptions?customer=`, `GET /v1/checkout/sessions?customer=`, `GET /v1/invoice_payments?payment[payment_intent]=` | |
| Deleting an account | `DELETE /v1/subscriptions/{id}` (cancel now, no proration refund) | the Customer is kept: Stripe holds the invoices |

Refused before any call: a subscribe while `spec.subscription.status` is
`active`, `trialing`, `past_due` or `incomplete` (answer `409
already_subscribed`: the UI sends the user to the portal); an item that is
not `enabled`; any purchase by a blocked account.

## Grant keys

A Grant's name is `g-` followed by the first 40 hex characters of the
SHA-256 of its `spec.key`. Creating a Grant that exists is success. This is
the whole of the idempotency: no list of processed event IDs is kept.

| Cause | `spec.key` | `seconds`, `validFrom`, `expiresAt` |
|---|---|---|
| The month's free allowance (operator) | `free/<ownerHash>/<yyyy-mm>` | the tier's hours; the first of the month to the first of the next, UTC |
| A subscription's paid period (backend) | `plan/<subscription id>/<current_period_start, unix>` | the plan's hours; the period's start to its end |
| A pack (backend) | `purchase/<checkout session id>` | the pack's hours; from the time of the call, for `validDays` |
| An admin's adjustment (by hand) | `admin/<free text>` | as written |

## Ensure functions

Every path below ends in one of two functions. Both read Stripe's current
state, so the order events arrive in does not matter, and both are safe to
run any number of times.

**`ensureSubscription(subscription ID)`**

1. `GET` the Subscription, expanding `items.data.price` and `latest_invoice`.
2. Find the Account by `metadata.account`; failing that, by
   `spec.stripeCustomerId` = its `customer`. None: log and stop (200 to
   Stripe; the reconcile will not find it either).
3. Write `spec.subscription`: `id`, `status`, the price's `lookup_key`,
   `items.data[0].current_period_start` and `_end` (the period is on the
   item since API version 2025-03-31, not on the subscription), `cancelAt`
   from `cancel_at`, `readAt`. If the Account already names another
   subscription that is still `active`, keep the one with the later
   `created` and log loudly.
4. If `status` is `active` or `trialing`, and `latest_invoice.status` is
   `paid`: create the plan Grant for the current period; then set
   `revoked: {reason: superseded}` on every other unrevoked `plan` Grant of
   the same account whose `expiresAt` is after the new one's `validFrom`.
5. If `status` is `canceled`, `unpaid` or `incomplete_expired`: nothing to
   grant. The current period's Grant, already paid for, runs to its expiry.

**`ensurePurchase(checkout session ID)`**

1. `GET` the Checkout Session.
2. Stop unless `mode` is `payment`, `status` is `complete` and
   `payment_status` is `paid`.
3. Account from `client_reference_id`; it must match `metadata.account` and
   the session's `customer` must be the Account's. Mismatch: log, stop.
4. Create the pack Grant, with `ref.checkoutSession` and
   `ref.paymentIntent` and the label `browserjs.dev/payment-intent`.

## Webhook

`POST https://api.computeruse.site/stripe/webhook`. Public at Pomerium,
no identity; the only authentication is the signature.

1. Read the raw body (at most 1 MiB; Stripe's events are far smaller).
2. Verify `Stripe-Signature` against `STRIPE_WEBHOOK_SECRET` with the SDK's
   `webhook.ConstructEvent`, tolerance 5 minutes. Invalid: `400`, nothing
   logged but the fact and the source address; a rate limit on failures per
   source address, as the API host has for bad tokens.
3. If the event's `livemode` does not match `STRIPE_MODE`: `400`.
4. Handle by type (below), synchronously. Success, or an event that is
   ignored: `200`. A failure to reach Stripe or to write to the cluster:
   `500`, so that Stripe sends it again.

| Event | Action |
|---|---|
| `checkout.session.completed`, `checkout.session.async_payment_succeeded` | `mode: payment`: `ensurePurchase(id)`. `mode: subscription`: `ensureSubscription(session.subscription)`. |
| `checkout.session.async_payment_failed` | nothing (logged) |
| `customer.subscription.created`, `.updated`, `.deleted`, `.paused`, `.resumed` | `ensureSubscription(id)` |
| `invoice.paid` | if `parent.type` is `subscription_details`: `ensureSubscription(parent.subscription_details.subscription)` |
| `invoice.payment_failed` | the same (the status becomes `past_due`; no Grant is made) |
| `refund.created`, `charge.refunded` | find the Grant by the label `browserjs.dev/payment-intent`; a pack: set `revoked: {reason: refund}`. A subscription's charge (found through `invoice_payments`): revoke the plan Grant of that invoice's period. A partial refund revokes the whole Grant (refunds are made by the product owner by hand, who can add an `admin` Grant for the remainder). |
| `charge.dispute.created` | set `spec.blocked: {reason: dispute}` on the Account and revoke the disputed Grant with `reason: dispute` |
| `charge.dispute.closed` | status `won`: remove `spec.blocked` if its reason is `dispute` (the Grant stays revoked; an admin restores hours by hand). Otherwise nothing. |
| anything else | `200`, ignored |

The endpoint in the Dashboard subscribes to exactly the events named in
this table, and is created with the API version of the pinned `stripe-go`
release (2025-03-31 "basil" or later: `invoice.parent` and the period on
the item are assumed).

## Returning from Checkout

Webhooks can be late, so the page the user lands on also fulfils: the UI
calls `GET /api/billing/checkout/{id}`, which checks that the session's
`client_reference_id` is the caller's Account and runs `ensurePurchase` or
`ensureSubscription`. The Grant then exists; its hours are in the balance
at the operator's next tick (at most 60 s), which the UI waits for
(`ui-states.md`).

## Reconcile

Hourly, and at start-up, the backend goes through every Account with a
`stripeCustomerId`: `ensureSubscription` for each of its subscriptions
that is not `canceled`, and `ensurePurchase` for each of its Checkout
Sessions of the last 35 days (the page size and a pause between customers
keep it far below Stripe's rate limits; with more than a few thousand
customers this becomes a rolling pass). This repairs a missed webhook (a
sandbox retries only three times, over a few hours) and rebuilds every
paid-for Grant after the cluster's objects are lost: the names are
deterministic, so the Grants come back as they were. Packs older than 35
days are rebuilt by running the command with `--since`, by hand.

## Not used, deliberately

Billing Meters and meter events, billing credits (credit grants), metered
prices, Stripe Tax (`automatic_tax`), trials, coupons, quantities above 1,
Stripe Entitlements, saved-card off-session charges (auto top-up). The
reasons are section 3.3 and section 9 of the design.
