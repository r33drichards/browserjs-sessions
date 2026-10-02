# Billing: the backend's Stripe side

Track C of the
[metering and billing plan](plans/2026-10-02-metering-billing-tracks.md).
Contract: [`contracts/billing/stripe.md`](contracts/billing/stripe.md), and
the checkout, portal, auto-recharge and webhook routes of
[`backend-api.yaml`](contracts/billing/backend-api.yaml). Design:
[2026-10-02-metering-billing-design.md](plans/2026-10-02-metering-billing-design.md).

This side writes what Stripe says into an Account's `spec` (the card, the
subscription, auto-recharge) and makes the credit that was paid for. It
decides nothing about whether a session may run: that is enforcement's
(track D), which reads what is written here.

Credit is kept in Metronome since
[2026-10-02](plans/2026-10-02-metronome-integration.md). Nothing here knows:
it makes and revokes credit only through `Ledger.EnsureGrant` and
`Ledger.Revoke`, with the Grant keys of `stripe.md`, and reads no balance.
"Grant" below is that credit, by its key.

**Off by default.** With `STRIPE_MODE` unset none of it exists: no route, no
webhook, no call to Stripe, and the server's handler is the very one it was
(`TestStripeOffIsToday`).

## What is in the tree

| Path | What |
|---|---|
| `backend/internal/billing/stripe/types.go` | `PeriodFinder`, and this side's constants; what it depends on is `internal/billing`'s (`Accounts`, `Ledger`, `Stripe`, `Clock`, the catalogue) |
| `.../service.go` | `Service`; `EnsurePaymentMethods` with the sign-up credit decision, `EnsureSubscription`, `EnsurePurchase` |
| `.../recharge.go` | auto-recharge: `Recharge` (the attempt) and `EnsureRecharge` |
| `.../webhook.go` | `POST /stripe/webhook`: raw body, signature, mode, the event table |
| `.../handlers.go` | the checkout, checkout state, portal and auto-recharge routes |
| `.../reconcile.go` | the two reconciles and `Run` |
| `.../client.go` | `API`: `billing.Stripe` over `stripe-go`; `KeyMode` |
| `.../catalogue.go` | what this side reads of `billing.Catalogue`; `CheckCatalogue` |
| `.../stripetest/` | in-memory fakes of the same interfaces as `billingtest`'s, with what the whole event table needs (subscriptions, invoices, refunds, disputes) and calls that fail on demand; a fake of the setup calls |
| `.../checks/` | the four sandbox checks, and `webhookcheck` |
| `backend/cmd/server/stripe.go` | the wiring: `newStripe`, `withStripe` |
| `.github/workflows/backend-tests.yml` | the backend's Go tests on a pull request |
| `backend/internal/config/stripe.go` | `STRIPE_MODE`, `STRIPE_API_KEY`, `STRIPE_WEBHOOK_SECRET` (`AUTO_RECHARGE` and `SIGNUP_CREDIT` are billing's) |

## Configuration

| Env var | Default | Meaning |
|---|---|---|
| `STRIPE_MODE` | unset | `test` or `live`. Unset: nothing of this side exists. Requires `BILLING` other than `off`, and `API_URL`. |
| `STRIPE_API_KEY` | | required with `STRIPE_MODE`. The backend refuses to start if the key's prefix is of the other mode (`sk_live_`/`rk_live_` with `test`, or the reverse). |
| `STRIPE_WEBHOOK_SECRET` | | required with `STRIPE_MODE` |
| `BILLING_IDS` | `/etc/browserjs/billing-iac/ids.json` | the file of `infra/billing`'s IDs; it need not exist |
| `AUTO_RECHARGE` | `off` | `on`: accounts may turn auto-recharge on |

The key and the signing secret are held as `config.Secret`, which prints as
`[redacted]` however it is formatted, logged or encoded; no error names
either (`TestStripeRefusesToStart`, `TestStripeSecretsAreNotPrinted`).

With `BILLING` on and `STRIPE_MODE` unset the four buying routes answer
`503 payments_off`, so the UI can say that nothing can be bought; there is
still no webhook.

## Routes

On the app's host, for a signed-in user (a cookie through Pomerium). An API
token gets `403 ui_only`; a blocked or deleted account `403 account_blocked`.

| Route | Does |
|---|---|
| `POST /api/billing/checkout` `{}` | a Checkout in setup mode: saves a card, charges nothing. `409 too_many_cards` with 5 cards saved; `429 rate_limited` after 5 setup checkouts in 24 hours (counted from Stripe's list of the customer's sessions). |
| `POST /api/billing/checkout` `{"item": "<lookupKey>"}` | a Checkout that subscribes to a plan or buys a pack. `400 unknown_item` for what is not in the catalogue, not enabled, or has no price at Stripe; `409 already_subscribed` while the account's subscription is `active`, `trialing`, `past_due` or `incomplete`. |
| `GET /api/billing/checkout/{id}` | what became of a Checkout the caller started, and fulfils it (a late webhook does not keep the user waiting). `404` for another account's. |
| `POST /api/billing/portal` | a link to Stripe's Customer Portal, with the configuration `infra/billing` made: its ID from the file `BILLING_IDS` (the ConfigMap `billing-iac`, key `ids.json`, if it is mounted and of this mode), else found at Stripe by `metadata.managed_by = stripe-setup`. One made through the API is never the account's default. `409 no_customer` before the account has been to Stripe. |
| `PUT /api/billing/auto-recharge` | the caller's automatic top-up. `404 auto_recharge_off` while `AUTO_RECHARGE` is off; turning it on needs `agree: true` and a saved card (`402 payment_method_required`). |

Any call to Stripe that fails is `502 stripe_unavailable`: nothing was
charged.

On the API host, with no sign-in: `POST /stripe/webhook`. The signature is
the only credential. `400` with no body for a signature that does not
verify, a timestamp older than 5 minutes, a body over 1 MiB, or an event of
the other mode; `500` when Stripe or the cluster could not be reached, so
that Stripe sends the event again; `200` otherwise, including for an event
that is ignored.

The Stripe customer is made the first time an account starts a Checkout,
with the idempotency key `customer-<ownerHash>`, and written to
`spec.stripeCustomerId` before the Checkout is made.

### Changing plan

**A subscriber cannot change plan in place yet.** The portal's plan
switching is off (the Stripe provider cannot set its products;
`docs/billing-iac.md`), and this side has no route that changes a
subscription. What a subscriber can do: cancel in the portal (it ends at the
period's end, and the period's credit runs to its expiry), then subscribe to
another plan once it has ended; and buy credit packs at any time.
`409 already_subscribed` says so. An in-place change through our own API
would be a new call on `billing.Stripe` and a contract change.

Nothing here reads a Stripe product by its ID: prices are found by lookup
key, and a subscription's plan by its price's lookup key. The catalogue's
`productId` is checked for being present and never sent.

## How it stays correct

- **Nothing is written from an event's payload.** An event says which object
  changed; the handler reads that object from Stripe and writes what Stripe
  says now. So the order events arrive in does not matter, and neither does
  a replay.
- **A Grant's name is the hash of what caused it** (`GrantName`): making it
  twice is impossible. No list of handled events is kept.
- **The sign-up credit's Grant is the card's** (`signup/<fingerprint>`), so
  the same card on a second account finds it taken; the decision is written
  to `spec.signupCredit` once.
- **One account at a time**: the ensure functions of an account run one
  after another, so that of two reads of Stripe the older is never written
  after the newer.
- **A customer Stripe does not have has no card.** `present` is written
  false, and it is logged loudly.

Two things go beyond the letter of `stripe.md`, and are listed under
"Deviations": the guard on an old automatic charge's outcome, and which of
two subscriptions has its other Grants superseded.

## How it sits with the rest of billing

Its types and interfaces are `internal/billing`'s: `billing.Accounts`,
`billing.Ledger`, `billing.Stripe`, `billing.Clock`, the catalogue.
`cmd/server/stripe.go` wires it: `newStripe` makes the `Service` over the
account store and the ledger billing already made, and `withStripe` puts
its routes and the webhook in front of the server.

| What billing needs of this side | Where |
|---|---|
| auto-recharge in the balance pass | `Service` is the pass's `metronome.Recharger`: `Consider(ctx, account, credit)`, which is `Recharge(ctx, accountName, balanceMicros)` |
| the Stripe side of deleting an account | billing's handler does it with the same client (`Handlers.Stripe`), set by `newStripe` |
| `GET /api/billing`'s `autoRecharge` | `(*Service).AutoRechargeView(spec)` |

Of the ledger this side calls `EnsureGrant` and `Revoke` only. It uses
`GrantSelector` in three ways, always with the `Account`:

| For | Selector |
|---|---|
| a refund or a dispute of a pack | `PaymentIntent` |
| a refund of a subscription's charge | `Name: GrantName("plan/<subscription>/<period start>")` |
| an upgrade: the old period's credit goes | `Source: plan`, `ExpiresAfter` the new period's start, `Except` the new Grant's name |

A key that was used answers `created: false`; `existing.Account` other than
this account's (another's, or none) is read as "this card has had its
credit".

**The scenarios.** `billingtest/stripestub.go` says of itself that it is
deleted when this track merges: the scenarios then make a `stripe.Service`
where they make a `StripeStub`. `TestCardGateOnBillingtestFakes` already
runs the Stripe half of scenario 1 on `billingtest`'s fake Stripe with this
package's real routes and webhook handler.

## Stripe's objects

Products, Prices and the Customer Portal configuration are made by OpenTofu
(`infra/billing`) from the catalogue; there is no setup command here. The
backend finds prices by their lookup keys, at start and every hour
(`RefreshPrices`): an enabled item Stripe has no price for is logged and is
not offered.

## The restricted key

`deploy.md` lists the permissions by what they are for; the resource names
as the Dashboard shows them are **not verified**. The calls each key makes,
from which the permissions follow:

| Key | Writes | Reads |
|---|---|---|
| run time (`STRIPE_API_KEY`) | `POST /v1/customers`, `/v1/checkout/sessions`, `/v1/billing_portal/sessions`, `/v1/payment_intents`, `/v1/payment_methods/{id}/detach`; `DELETE /v1/subscriptions/{id}` | `GET /v1/customers/{id}`, `/v1/customers/{id}/payment_methods`, `/v1/checkout/sessions` (one and list), `/v1/subscriptions` (one, with `latest_invoice` expanded, and list), `/v1/payment_intents` (one and list), `/v1/prices`, `/v1/invoice_payments` (with `data.invoice` expanded), `/v1/billing_portal/configurations` |

To find the smallest set: start a restricted key broad in the sandbox, run
the product through every row of the table above, then prune with the key's
request log in the Dashboard, and write the names into `deploy.md` (a
contract pull request).

## The four sandbox checks

Scripts in `backend/internal/billing/stripe/checks/`. They are run by the product
owner or a developer with **their own sandbox key**; they refuse anything
but an `sk_test_`/`rk_test_` key, print no key, and are never run in CI.
They need `curl` and `jq`; check 1 also needs the Stripe CLI.

| # | Script | Asks |
|---|---|---|
| 1 | `1-webhook-signature.sh [url]` | does an event Stripe sends reach the handler with a signature that verifies? With no argument: straight to the real handler, run on fakes (`checks/webhookcheck`). With the URL of the API host's webhook: through Pomerium, to a running backend. |
| 2 | `2-setup-checkout.sh` | a setup-mode Checkout: which events, in what order; the card attached; what `payment_method.detached` carries from the API and from the portal; whether the portal removes a last card; the fingerprint of one card saved twice, and through a wallet |
| 3 | `3-test-clock.sh` | on a test clock: subscribe, a month, upgrade, downgrade, a failed renewal, cancel |
| 4 | `4-off-session.sh` | an off-session charge on the test card that requires authentication: the error and the events |

### Results

**None has been run**: no track handles a Stripe key, and there is no
sandbox yet. Until they are, what each would confirm is an assumption the
code is built on:

| # | Assumed | If it is otherwise |
|---|---|---|
| 1 | Pomerium passes the body byte for byte and the `Stripe-Signature` header | every event is refused with 400. The fallback is a route that does not go through Pomerium. |
| 2a | `checkout.session.completed` arrives for a setup Checkout, and `setup_intent.succeeded`, `payment_method.attached` | none is relied on alone: each ends in a fresh list of the customer's payment methods, and so does the return from Checkout and the 15 minute reconcile |
| 2b | a setup Checkout with `customer` set attaches the card to that customer | no card is ever seen: the gate never opens |
| 2c | `payment_method.detached` has `customer: null` | handled either way: the account is found by the PaymentMethod's ID |
| 2d | the portal lets a customer with no subscription remove their last card | only the frequency of `active -> no_card` changes |
| 2e | a card's fingerprint is the same each time it is saved in one Stripe account, and differs through a wallet | the sign-up credit could be earned twice with one card, or refused to a card that never had it |
| 3 | the period is on the subscription item (`items.data[0].current_period_start`); a renewal's `latest_invoice` is `paid` once paid; a failed renewal is `past_due`; the portal's upgrade with `billing_cycle_anchor: now` starts a new period; `cancel_at` is set by a cancellation at the period's end | plan Grants made for the wrong period, or not made. If `billing_cycle_anchor: now` with `schedule_at_period_end` does not behave as `stripe.md` says, the fallback there is `proration_behavior: none` with `billing_cycle_anchor: unchanged`: one line in `setup.go`. |
| 3 | an upgrade's invoice has a line for the new period whose `period.start` is the latest of its lines (`SubscriptionPeriodOf`) | a refund of an upgrade's charge revokes no plan Grant, or the wrong one |
| 4 | an off-session charge that needs authentication answers an error carrying the PaymentIntent, in `requires_payment_method`, with code `authentication_required` | auto-recharge is turned off with `payment-failed` in place of `authentication-required`: the wrong sentence, the right outcome |
| 4 | the same idempotency key, sent again within 24 hours, answers the same PaymentIntent | a crash between `seq` and the call could charge twice |

The client's requests and its reading of Stripe's answers are tested against
a stand-in for the API in the same process (`client_test.go`), with answers
written from Stripe's documentation: that fixes what is sent, not that
Stripe accepts it.

## Tests

`cd backend && nix develop .. -c go test -race ./...`. None reaches Stripe or
a Kubernetes API; the webhook's events are signed with a made-up secret
(`whsec_test`) by the SDK's own test helper.

| Test | What |
|---|---|
| `TestWebhookIdempotency` | scenario 4 of `testing.md`, every case: each event of the table posted twice; a subscription's events in reverse; `detached` before `attached`; `attached` replayed after the detach; one card on two accounts; prepaid, wallet and no-fingerprint cards; a bad signature, a timestamp 6 minutes old, the other mode; a customer no Account has; the crash between `seq` and the call |
| `TestEventTable` | every row of the event table, with what it must leave |
| `TestWebhookRetriesWhatFailed` | 500 when Stripe, the account store or the ledger fails; handled when sent again |
| `TestCheckoutState` | open, complete, expired; fulfilled with no webhook; **a checkout session that belongs to another account** |
| `TestCheckoutRefusals` | a token, nobody, blocked and deleted accounts, items not sold, **subscribe while subscribed**, a sixth card, a sixth setup checkout, Stripe down |
| `TestPurchaseMustMatchItsAccount` | a paid Checkout whose reference, metadata or customer is another account's grants nothing |
| `TestReconcileMakesPaidForGrants`, `TestReconcileRepairsMissedWebhooks` | every paid-for Grant whose event never arrived is made, under its key, and a second pass changes nothing; **a card removed with no webhook**; a checkout never returned from; a renewal with no event; the 35 day window |
| `TestAutoRecharge...` | **at the threshold, at the cap, on failure**, the default card, the cap raised, the month turning, each condition that stops it, and a charge that does not say which attempt it was |
| `TestCardGateOnBillingtestFakes` | the Stripe half of scenario 1 on `billingtest`'s fakes |
| `TestStripeRefusesToStart` (`internal/config`) | **a live key with `STRIPE_MODE=test` refuses to start, and the error does not contain the key** |
| `TestStripeOffIsToday`, `TestStripeRoutes`, `TestStripePaymentsOff` (`cmd/server`) | the flag off; the routes on the right hosts and nowhere else |
| `TestAPI...` | the real client's requests and its reading of answers |

## Deviations from the contracts, and what they do not say

Raised here for a contract pull request; none is worked around silently.

1. **One customer ID for both modes.** Decided: the Account will hold
   Stripe's state per mode (`spec.stripe.test`, `spec.stripe.live`), so
   that after the switch to live an account has no card there and goes
   through the gate again. That change follows this pull request; until it
   lands, see 2.
2. **`stripeCustomerId` cannot be changed once set** (the CRD's rule), and
   stage 4 switches from test mode to live, where the test-mode customers do
   not exist. As written, every account that saved a test card would have a
   customer Stripe's live mode does not know: no card, and no way to make a
   Checkout. Either the Accounts are deleted at the switch, or the rule
   allows the ID to be cleared. This side treats a customer Stripe does not
   have as "no card".
3. **`PUT /api/billing/auto-recharge` answers 400 with the code
   `invalid_request`** for a cap above the maximum, a cap below one pack, or
   enabling without `agree`. `backend-api.yaml` lists those as 400s and its
   `Error.code` has no value for them.
4. **`too_many_cards` is 409 and the sixth setup checkout of a day is
   `429 rate_limited`** with `Retry-After: 3600`; `backend-api.yaml` names
   the codes and no statuses.
5. **An automatic charge's PaymentIntent carries `metadata.month` and
   `metadata.seq`** as well as `account`, `kind` and `item` (the client
   takes them from the idempotency key). Without them an old failed charge,
   read again by the hourly reconcile for 35 days, would turn auto-recharge
   off again after the user had turned it back on. A charge without them is
   matched to the pending attempt by when it was made. `chargedCents` is
   added to once per charge for the same reason.
6. **An attempt left `pending` is made again after 10 minutes**, with the
   same `seq` and so the same idempotency key. `stripe.md` says both that a
   pending attempt blocks the next and that a crash repeats the same key;
   this is how both hold. Stripe keeps an idempotency key for 24 hours: a
   backend down for longer than that after such a crash could charge twice.
7. **With two live subscriptions on one account** only the later one
   supersedes other plan Grants; the earlier one's period is still granted
   if it is paid, and then superseded by the later's. Applied literally,
   step 4 of `ensureSubscription` would have each revoke the other's.
8. **`SIGNUP_CREDIT=off` leaves `spec.signupCredit` unset** (nothing is
   decided), so turning it on again decides at the next read of the card.
   The CRD's reasons have no value for "the bonus was off".
9. **The webhook accepts an event made with another API version** than the
   SDK's (`IgnoreAPIVersionMismatch`): it reads only the type and a few IDs
   from the event. `stripe.md` has the endpoint made with the pinned
   version; that is still right, and no longer fatal if it is not.
10. **`stripe-go` is v86.4.2** (API `2026-08-26.dahlia`), later than the
    `2025-03-31` the contract names as the minimum.
11. **`payments_off`**: `deploy.md` says that with `STRIPE_MODE` unset there
    are no checkout routes; `backend-api.yaml` gives them a 503. With
    billing off there are none; with billing on they answer 503.
12. **The rebuild of purchases older than 35 days** ("by running the command
    with `--since`") is `ReconcilePurchases(ctx, since)`; no command calls
    it with an older date yet, because one needs the account store.
13. **The limit on webhook failures per source address is a limit on what
    is logged**, not a refusal: an address that keeps sending requests that
    do not verify is answered 400 each time and logged only at first. An
    event that verifies is never refused for where it came from, so Stripe
    cannot be locked out by someone else's noise behind the same proxy.
14. **A setup Checkout's `currency` is the catalogue's**, given to the
    client when it is made: `billing.CheckoutParams` has no field for it.
15. **A refund of a subscription's charge** needs
    `GET /v1/invoice_payments`, which `billing.Stripe` does not have. It is
    an interface of its own here (`PeriodFinder`), which the real client
    is; with a client that is not one, only packs are revoked by a refund.

## Not built here

Stripe Tax, trials, coupons, metered prices: `stripe.md`, "Not used,
deliberately".
