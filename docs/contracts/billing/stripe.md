# Stripe contract

> **Changed 2026-10-02 by [`metronome.md`](metronome.md).** Stripe's part
> is unchanged: every object, call, event and ensure function below
> stands. What changes is where credit is kept:
>
> - "Create the Grant" means `Ledger.EnsureGrant`, which creates a
>   Metronome credit whose `uniqueness_key` is the Grant key of the table
>   below. "Revoke" archives it. There is no Grant resource.
> - "Exists for this account" and "exists for another account" are the two
>   outcomes of Metronome's 409 (`metronome.md`, "Credit (grants)").
> - Credit is in the balance as soon as the call returns, not "at the
>   operator's next tick"; the checkout return does not wait.
> - Auto-recharge is decided in the backend's balance pass (every 5
>   minutes), from the balance it has just read from Metronome, in place
>   of `status.balanceMicros` in the sweep. The rest of it is unchanged.
> - A refund finds its credit by the `payment_intent` custom field, in
>   place of the label.
> - The hourly reconcile still re-makes every paid-for credit: the keys
>   are deterministic, and Metronome answers 409 for the ones it has.
> - Metronome is not connected to Stripe. Metronome's invoicing, payment
>   gating, recurring credits and auto-recharge join the "Not used,
>   deliberately" list.

Which Stripe objects exist, who makes them, which calls the backend makes,
which events it handles and what each does. Sources for every Stripe fact
are in section 1 of the design.

## Division of labour

| | Makes it | With |
|---|---|---|
| The Stripe account, its sandbox, the webhook endpoint, the API keys, the Radar setting | the product owner, by hand in the Dashboard | the to-do list of the design |
| Products, Prices, the Customer Portal configuration | `backend/cmd/stripe-setup`, run by the manual workflow `stripe-setup.yml` | `STRIPE_SETUP_KEY` |
| Customers, Checkout Sessions, Portal sessions, off-session PaymentIntents (auto-recharge) | the backend, at run time | `STRIPE_API_KEY` |
| SetupIntents, PaymentMethods, Subscriptions, Invoices, Refunds, Disputes | Stripe | |

Nothing in the cluster other than the backend has a Stripe key or talks to
Stripe. The operator does not.

## Object catalogue

Made from `catalogue.yaml` by the setup command. It is idempotent: run
twice, the second run changes nothing and says so.

| Object | Identity | Fields |
|---|---|---|
| Product, one per plan | `id` = `productId` (`cu_plan_starter`, `cu_plan_pro`, `cu_plan_scale`) | `name` "Computer Use <Name>", `metadata.catalogue_key`, `active` = `enabled` |
| Product for credit | `id` = `cu_credit` | `name` "Computer Use credit" |
| Price, one per plan (three recurring Prices) | `lookup_key`: `cu_starter_monthly_v1`, `cu_pro_monthly_v1`, `cu_scale_monthly_v1` | `unit_amount`, `currency: usd`, `recurring.interval: month`, licensed (not metered), `metadata.credit_micros` (for people; the backend does not read it) |
| Price, one per pack (three one-off Prices) | `lookup_key`: `cu_credit_5_v1`, `cu_credit_20_v1`, `cu_credit_50_v1` | `unit_amount`, one-off, `metadata.credit_micros` |
| Customer Portal configuration | the one with `metadata.managed_by = stripe-setup`; made if none | below |

A price's amount cannot be edited in Stripe. To change one: add an entry
with a new `lookupKey` (`..._v2`) to `catalogue.yaml`, keep the old entry
with `enabled: false` (existing subscribers stay on it and their grants
still need its credit), and run the setup again. What a plan **gives**
(credit, limits) and the two **rates** are not in Stripe at all: they change
by editing the catalogue's ConfigMap, with no Stripe change and no image
build. `transfer_lookup_key` is not used: a lookup key always means the
same amount.

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
(track C, first task). If it does not, the fallback is `proration_behavior:
none` with `billing_cycle_anchor: unchanged`.

Stripe's portal makes a customer with an active subscription keep at least
one payment method. A customer with no subscription can remove their last
one there; that is the case "Payment methods" below handles.

## Calls the backend makes

All with `Idempotency-Key` where noted, a 10 s timeout, and the SDK's two
automatic retries.

| When | Call | Notes |
|---|---|---|
| Start-up, and hourly | `GET /v1/prices?lookup_keys[]=...` (at most 10 per call) | the map lookup key to price ID; a missing enabled key is logged and that item is not offered |
| The first time an account is sent to Stripe | `POST /v1/customers` | `email`, `metadata.account`, `metadata.owner_hash`; `Idempotency-Key: customer-<ownerHash>`; the ID is written to `spec.stripeCustomerId` before anything else is made |
| **Save a card** (the gate) | `POST /v1/checkout/sessions` | `mode: setup`, `currency: usd`, `customer`, `client_reference_id` = the Account's name, `metadata` and `setup_intent_data.metadata` = `{account, kind: setup}`, `success_url: <PUBLIC_URL>/billing?checkout={CHECKOUT_SESSION_ID}`, `cancel_url: <PUBLIC_URL>/billing`. Nothing is charged; Stripe may place a temporary $0 or $1 authorisation to check the card. |
| Subscribe | `POST /v1/checkout/sessions` | `mode: subscription`, `customer`, `line_items[0].price`, `client_reference_id`, `metadata` and `subscription_data.metadata` = `{account, kind: plan, item}`, the same URLs, `automatic_tax` off. Subscription mode saves the payment method. |
| Buy credit | `POST /v1/checkout/sessions` | `mode: payment`, `customer`, one line, quantity 1, `payment_intent_data.setup_future_usage: off_session` (the card is saved to the customer), `metadata` and `payment_intent_data.metadata` = `{account, kind: purchase, item}`, the same URLs |
| Manage billing | `POST /v1/billing_portal/sessions` | `customer`, `return_url: <PUBLIC_URL>/billing` |
| Auto-recharge | `POST /v1/payment_intents` | `amount`, `currency`, `customer`, `payment_method` (the customer's default, else the newest card), `off_session: true`, `confirm: true`, `metadata` = `{account, kind: recharge, item}`, `Idempotency-Key: recharge-<ownerHash>-<yyyy-mm>-<seq>` |
| Reading, on events and in the reconcile | `GET /v1/customers/{id}/payment_methods`, `GET /v1/customers/{id}`, `GET /v1/setup_intents/{id}`, `GET /v1/payment_methods/{id}`, `GET /v1/subscriptions/{id}`, `GET /v1/checkout/sessions/{id}`, `GET /v1/payment_intents/{id}`, `GET /v1/subscriptions?customer=`, `GET /v1/checkout/sessions?customer=`, `GET /v1/payment_intents?customer=`, `GET /v1/invoice_payments?payment[payment_intent]=` | |
| Deleting an account | `DELETE /v1/subscriptions/{id}` (cancel now), `POST /v1/payment_methods/{id}/detach` for each card | the Customer is kept: Stripe holds the invoices |

Refused before any call: a subscribe while `spec.subscription.status` is
`active`, `trialing`, `past_due` or `incomplete` (`409 already_subscribed`:
the UI sends the user to the portal); an item that is not `enabled`; any
checkout by a blocked account; a setup checkout beyond 5 a day or with 5
cards already saved.

## Grant keys

A Grant's name is `g-` followed by the first 40 hex characters of the
SHA-256 of its `spec.key`. Creating a Grant that exists is success. This is
the whole of the idempotency: no list of processed event IDs is kept.

| Cause | `spec.key` | `amountMicros`, `validFrom`, `expiresAt` |
|---|---|---|
| The sign-up credit | `signup/<card fingerprint>` | `signupCredit.amountMicros`; from the time of the call, for `signupCredit.validDays` |
| A subscription's paid period | `plan/<subscription id>/<current_period_start, unix>` | the plan's `creditMicros`; the period's start to its end |
| A pack bought at a checkout | `purchase/<checkout session id>` | the pack's `creditMicros`; from the time of the call, for `validDays` |
| A pack bought by auto-recharge | `recharge/<payment intent id>` | the same |
| An admin's adjustment (by hand) | `admin/<free text>` | as written |

The sign-up Grant's key is the **card's**, not the account's, so the same
card on a second account finds the Grant already there. Whether an account
has had its decision is `spec.signupCredit`, written once.

## Ensure functions

Every path below ends in one of these. Each reads Stripe's current state,
so the order events arrive in does not matter, and each is safe to run any
number of times.

**`ensurePaymentMethods(customer ID)`**

1. Find the Account by `spec.stripeCustomerId`. None: log and stop.
2. `GET /v1/customers/{id}/payment_methods` (every type, all pages) and the
   customer's `invoice_settings.default_payment_method`.
3. Write `spec.paymentMethod`: `present` = the list is not empty, `ids`,
   `default`, `readAt`; `removedAt` = now if `present` went from true to
   false, cleared if it is true.
4. If `present` and `spec.signupCredit` is absent: **`decideSignupCredit`**,
   on the oldest card of the list:
   - no `card.fingerprint`: `refused`, `no-fingerprint`;
   - `card.funding` in `signupCredit.refuseFunding` (prepaid): `refused`,
     `prepaid`;
   - `card.wallet` set and `refuseWallets`: `refused`, `wallet` (a card
     saved through Apple Pay or Google Pay carries the device's number, so
     its fingerprint is not the card's);
   - otherwise create the Grant `signup/<fingerprint>` for this account. If
     it was created: `granted`. If it exists **for this account** (a replay):
     `granted`. If it exists for another account: `refused`, `card-used`.
   Then write `spec.signupCredit` once. A refusal does not stop the card
   from being saved or the account from being active; it only gives no
   credit.

   The fingerprint is unique within one Stripe account and differs between
   accounts; whether it is the same in a sandbox and in live mode is not
   documented and is not relied on.

Nothing here stops sessions: `present` turning false is seen by the
backend's sweep (`enforcement.md`, row 5), which puts them to sleep.

**`ensureSubscription(subscription ID)`**

1. `GET` the Subscription, expanding `items.data.price` and `latest_invoice`.
2. Find the Account by `metadata.account`; failing that, by
   `spec.stripeCustomerId` = its `customer`. None: log and stop.
3. Write `spec.subscription`: `id`, `status`, the price's `lookup_key`,
   `items.data[0].current_period_start` and `_end` (the period is on the
   item since API version 2025-03-31), `cancelAt` from `cancel_at`,
   `readAt`. If the Account already names another subscription that is
   still `active`, keep the one with the later `created` and log loudly.
4. If `status` is `active` or `trialing`, and `latest_invoice.status` is
   `paid`: create the plan Grant for the current period; then set
   `revoked: {reason: superseded}` on every other unrevoked `plan` Grant of
   the same account whose `expiresAt` is after the new one's `validFrom`.
5. If `status` is `canceled`, `unpaid` or `incomplete_expired`: nothing to
   grant. The current period's Grant, already paid for, runs to its expiry.
6. `ensurePaymentMethods(customer)`.

**`ensurePurchase(checkout session ID)`**

1. `GET` the Checkout Session.
2. `mode: setup` and `status: complete`: `ensurePaymentMethods(customer)`,
   done.
3. `mode: subscription`: `ensureSubscription(session.subscription)`, done.
4. `mode: payment`: stop unless `status` is `complete` and `payment_status`
   is `paid`. The Account from `client_reference_id` must match
   `metadata.account` and the session's `customer` must be the Account's;
   mismatch: log, stop. Create the pack Grant, with `ref.checkoutSession`,
   `ref.paymentIntent` and the label `browserjs.dev/payment-intent`. Then
   `ensurePaymentMethods(customer)`.

**`ensureRecharge(payment intent ID)`**

1. `GET` the PaymentIntent. Stop unless `metadata.kind` is `recharge`.
2. `succeeded`: create the Grant `recharge/<id>` with the pack's credit;
   write `autoRecharge.last.status: succeeded` and add the amount to
   `chargedCents`.
3. `requires_payment_method` or `canceled` (it failed): write
   `autoRecharge.enabled: false`, `last.status: failed`, `disabledReason`
   (`authentication-required` when the decline code is
   `authentication_required`, else `payment-failed`). The UI tells the user
   and offers a checkout, where they can authenticate.

## Auto-recharge

Off for every account until its owner turns it on in the app, choosing a
pack, a threshold and a monthly cap, and agreeing to the text of
`ui-states.md` (recorded as `agreedAt`). It is a prepaid top-up made for
the user, not post-paid billing: nothing is ever owed.

The backend's sweep, for an `active` account with `autoRecharge.enabled`,
when all of these hold:

- `status.balanceMicros < thresholdMicros`, and the ledger is not stale;
- `last.status` is not `pending`, and the last attempt was more than 10
  minutes ago;
- `chargedCents + the pack's amount <= monthlyCapCents` for the current
  calendar month (else it sets `enabled: false`, `disabledReason:
  cap-reached` and stops until the user raises the cap or the month turns);

writes `seq + 1`, `last: {status: pending, at}` to the Account **first**,
then creates the PaymentIntent with the idempotency key built from that
`seq`. A crash between the two repeats the same key and so the same
PaymentIntent. The response, the webhook (`payment_intent.succeeded`,
`payment_intent.payment_failed`) and the reconcile all end in
`ensureRecharge`.

A failed attempt turns auto-recharge off; it is not retried. With no card
(`no_card`) nothing is attempted and `disabledReason` is `no-card`.

## Webhook

`POST https://api.computeruse.site/stripe/webhook`. Public at Pomerium,
no identity; the only authentication is the signature.

1. Read the raw body (at most 1 MiB).
2. Verify `Stripe-Signature` against `STRIPE_WEBHOOK_SECRET` with the SDK's
   `webhook.ConstructEvent`, tolerance 5 minutes. Invalid: `400`, nothing
   logged but the fact and the source address; a rate limit on failures per
   source address.
3. If the event's `livemode` does not match `STRIPE_MODE`: `400`.
4. Handle by type (below), synchronously. Success, or an event that is
   ignored: `200`. A failure to reach Stripe or to write to the cluster:
   `500`, so that Stripe sends it again.

| Event | Action |
|---|---|
| `checkout.session.completed`, `checkout.session.async_payment_succeeded` | `ensurePurchase(id)` |
| `checkout.session.async_payment_failed` | nothing (logged) |
| `setup_intent.succeeded` | `ensurePaymentMethods(customer)` |
| `payment_method.attached`, `payment_method.updated`, `payment_method.automatically_updated` | `ensurePaymentMethods(data.object.customer)` |
| `payment_method.detached` | the event's object has `customer: null`: find the Account whose `spec.paymentMethod.ids` contains the PaymentMethod's ID, then `ensurePaymentMethods` of its customer. None found: `200` (the reconcile covers it). |
| `customer.updated` | `ensurePaymentMethods(id)` (the default may have changed) |
| `customer.deleted` | write `paymentMethod.present: false`; log loudly (customers are not deleted by this product) |
| `customer.subscription.created`, `.updated`, `.deleted`, `.paused`, `.resumed` | `ensureSubscription(id)` |
| `invoice.paid`, `invoice.payment_failed` | if `parent.type` is `subscription_details`: `ensureSubscription(parent.subscription_details.subscription)` |
| `payment_intent.succeeded`, `payment_intent.payment_failed` | `metadata.kind` is `recharge`: `ensureRecharge(id)`. Otherwise ignored (Checkout's own are handled by its session events). |
| `refund.created`, `charge.refunded` | find the Grant by the label `browserjs.dev/payment-intent`; a pack: set `revoked: {reason: refund}`. A subscription's charge (found through `invoice_payments`): revoke the plan Grant of that invoice's period. A partial refund revokes the whole Grant (refunds are made by the product owner by hand, who can add an `admin` Grant for the remainder). |
| `charge.dispute.created` | set `spec.blocked: {reason: dispute}` and revoke the disputed Grant with `reason: dispute` |
| `charge.dispute.closed` | status `won`: remove `spec.blocked` if its reason is `dispute`. Otherwise nothing. |
| anything else | `200`, ignored |

The endpoint in the Dashboard subscribes to exactly the events named in
this table, and is created with the API version of the pinned `stripe-go`
release (2025-03-31 "basil" or later).

## Returning from Checkout

Webhooks can be late, so the page the user lands on also fulfils: the UI
calls `GET /api/billing/checkout/{id}`, which checks that the session's
`client_reference_id` is the caller's Account and runs `ensurePurchase`.
A saved card makes the account active at once (the backend's own write);
credit is in the balance at the operator's next tick (at most 60 s), which
the UI waits for (`ui-states.md`).

## Reconcile

At start-up and on a timer, the backend goes through every Account with a
`stripeCustomerId`:

| Every | Does |
|---|---|
| 15 minutes | `ensurePaymentMethods` (a missed `payment_method.detached` must not leave sessions running for long without a card) |
| hour | `ensureSubscription` for each subscription that is not `canceled`; `ensurePurchase` for each Checkout Session and `ensureRecharge` for each `recharge` PaymentIntent of the last 35 days |

Paced to stay far below Stripe's rate limits; with more than a few thousand
customers it becomes a rolling pass. This repairs a missed webhook (a
sandbox retries only three times, over a few hours) and rebuilds every
paid-for Grant after the cluster's objects are lost: the names are
deterministic, so the Grants come back as they were. Older purchases are
rebuilt by running the command with `--since`, by hand.

## Not used, deliberately

Billing Meters and meter events, billing credits (credit grants), metered
prices, Stripe Tax (`automatic_tax`), trials, coupons, quantities above 1,
Stripe Entitlements. The reasons are sections 1.6, 4.2 and 10 of the
design.
