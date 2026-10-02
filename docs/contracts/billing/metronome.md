# Metronome contract

Metronome (a Stripe product) is the meter and the credit ledger: it takes
usage events, holds each account's credit, burns it down, and tells us
when it is gone. Decided by the product owner on 2026-10-02 ("let's just
use Metronome"). Design and reasons:
[../../plans/2026-10-02-metronome-integration.md](../../plans/2026-10-02-metronome-integration.md).

**Where this file and another contract disagree, this file wins.** The
files it replaces in part or in whole carry a note at their top.

Facts about Metronome below were read from its documentation on
2026-10-02; the ones marked **UNVERIFIED** are settled by the sandbox
checks at the end, before code depends on them.

## Division of labour

| | Makes it | With |
|---|---|---|
| The Metronome account, its sandbox and production environments, the API tokens, the webhook destination and its secret | the product owner, by hand in Metronome's dashboard | the to-do list of the design |
| Billable metrics, products, the rate card and its rates, the zero-balance alert, custom field keys | `backend/cmd/metronome-setup`, run by the manual workflow `metronome-setup.yml` | `METRONOME_API_TOKEN` |
| Customers, contracts, credits (grants), balance and usage reads | the backend, at run time | `METRONOME_API_TOKEN` |
| Usage events | the observer (the billing operator), at run time | `METRONOME_API_TOKEN` |
| Burn-down, balances, the alert | Metronome | |

Metronome is **not connected to Stripe**. It sends no invoice and charges
nobody. Every payment stays as `stripe.md` has it (Checkout, the portal,
our webhook); a payment becomes credit when the backend creates a
Metronome credit for it. Metronome's own Stripe integration, recurring
credits, payment-gated commits and auto-recharge are not used (design,
"Decisions").

Two processes hold a Metronome token: the backend and the observer.
Nothing else does.

## Objects made by the setup command

`backend/cmd/metronome-setup --catalogue <catalogue.yaml>` finds each
object by name (or alias) and creates what is missing; `--apply=false`
prints what it would do; a second run changes nothing. Billable metrics
are immutable in Metronome: a changed definition is a new metric with a
new name suffix (`_v2`), never an edit.

| Object | Name | Definition |
|---|---|---|
| Billable metric | `cu_awake_seconds_v1` | `aggregation_type: sum`, `aggregation_key: seconds`, `event_type_filter: session.awake`, `group_keys: [["session_id"]]` |
| Billable metric | `cu_disk_gb_seconds_v1` | sum of `gb_seconds`, event type `session.kept`, `group_keys: [["session_id"]]` |
| Product (usage) | `Awake time` | on `cu_awake_seconds_v1`; `quantity_conversion`: divide by 3600 (hours) |
| Product (usage) | `Disk` | on `cu_disk_gb_seconds_v1`; `quantity_conversion`: divide by 2628000 (GB-months of 730 hours) |
| Product (fixed) | `Credit` | what every credit is attached to |
| Rate card | alias `cu-standard-v1` | below |
| Alert | `cu-zero-balance`, `uniqueness_key: cu-zero-balance` | `alert_type: low_remaining_contract_credit_and_commit_balance_reached`, `threshold: 0`, no `customer_id` (every customer), `evaluate_on_create: false` |
| Custom field keys | `grant_key`, `source`, `payment_intent` on credits | |

Rates on `cu-standard-v1`, both `rate_type: FLAT`, in US cents, from
`catalogue.yaml` (`rates`):

| Product | List rate | Commit rate | From the catalogue |
|---|---|---|---|
| `Awake time` | **0** | 20 per hour | `awakeMicrosPerHour` / 10000 |
| `Disk` | **0** | 28 per GB-month | `diskMicrosPerGBHour` x 730 / 10000, rounded to the cent |

The list rate is zero and the real price is the commit rate: usage draws
credit down at the real price while there is credit and costs nothing when
there is none (Metronome's "guarantee zero overages" pattern). This is
"the balance never goes below zero and nothing is ever owed". Every credit
is created with `rate_type: COMMIT_RATE`.

A change of rate is a new rate on the card with a `starting_at`; nothing is
recomputed.

## Customers and contracts

`Accounts.Ensure`, when it makes an Account, and the reconcile for any
Account without `spec.metronomeCustomerId`:

1. `POST /v1/customers` with `name` = the owner hash (not the email
   address: no personal data is sent to Metronome), `ingest_aliases` =
   `[<Account name>]`. If the alias is taken, find the customer by alias
   and use it.
2. `POST /v1/contracts/create` with `customer_id`, `rate_card_alias:
   cu-standard-v1`, `starting_at` = now truncated to the hour,
   `usage_statement_schedule: {frequency: MONTHLY}`, `uniqueness_key:
   contract/<Account name>`. A 409 is success.
3. Write `spec.metronomeCustomerId` and the label
   `browserjs.dev/metronome-customer` on the Account.

No session can exist before its owner's Account, so no usage event names a
customer Metronome does not know.

## Usage events

Sent by the observer to `POST /v1/ingest`, at most 100 events a request.

```json
{
  "transaction_id": "awake/<session id>/<tick, unix seconds>",
  "customer_id": "<Account name>",
  "event_type": "session.awake",
  "timestamp": "2026-10-02T10:01:00Z",
  "properties": { "session_id": "<session id>", "seconds": "60" }
}
```

```json
{
  "transaction_id": "kept/<session id>/<hour start, unix seconds>",
  "customer_id": "<Account name>",
  "event_type": "session.kept",
  "timestamp": "2026-10-02T11:00:00Z",
  "properties": { "session_id": "<session id>", "gb_seconds": "18000" }
}
```

- **What is counted is `metering.md`'s "What is observed" and "Seconds",
  unchanged**: the gap rule, `MAX_GAP`, `readySince`. The observer computes
  the awake seconds and GB-seconds of each tick exactly as the step does
  and as `metering-vectors.json` expects (`awakeSeconds`, `diskGBSeconds`).
  It does no arithmetic in money.
- `session.awake`: one event per awake session per tick (60 s) that
  charged more than zero seconds.
- `session.kept`: the GB-seconds of each session are added up in the
  observer and sent once an hour, at the hour, with the hour's start in the
  key. A session deleted mid-hour is sent at its last sight.
- `transaction_id` is the idempotency key; Metronome ignores a repeat for
  34 days. A retry sends the same key.
- The observer's state (each session's `lastSeen` and `awake`, the hour's
  GB-seconds) is in its memory. Losing it (a restart) can only lose
  charges: a session met again with no `lastSeen` is charged from
  `readySince` if that is within `MAX_GAP`, otherwise nothing, and the
  hour's unsent GB-seconds are gone.
- **Delivery.** A 429 or 5xx is retried with backoff, the batch kept in
  memory for at most one hour (events may be backdated 34 days); after
  that, or on a restart, it is dropped and that time is free. A 4xx other
  than 429 is logged with the batch's keys and dropped.
- After each tick that was delivered, the observer renews the Lease
  `billing-observer`. A Lease older than 10 minutes is logged by the
  backend every sweep and shown as `ledger: stale` in `GET /api/billing`.
  It refuses nobody.

Every error still falls in the user's favour (`metering.md`, "Which way
errors fall"), with "operator" read as "observer" and two additions:
Metronome unreachable for more than an hour, and a lost hour of disk on a
restart, are free.

## Credit (grants)

`Ledger.EnsureGrant` creates a customer-level credit,
`POST /v1/contracts/customerCredits/create`:

| Field | Value |
|---|---|
| `customer_id` | the Account's `metronomeCustomerId` |
| `product_id` | the `Credit` product |
| `name` | what the user sees: "Sign-up credit", "Starter plan credit", "Credit" |
| `uniqueness_key` | the Grant key of `stripe.md`, unchanged: `signup/<card fingerprint>`, `plan/<subscription id>/<period start>`, `purchase/<checkout session id>`, `recharge/<payment intent id>`, `admin/<text>` |
| `priority` | `plan` 10, `signup` 20, `purchase` and `recharge` 30, `admin` 40 (lower is used first) |
| `rate_type` | `COMMIT_RATE` |
| `access_schedule.schedule_items[0]` | `amount` = `amountMicros` / 10000 (cents); `starting_at` = `validFrom` truncated to the hour; `ending_before` = `expiresAt` rounded up to the hour |
| `custom_fields` | `grant_key`, `source`, and `payment_intent` for a pack |

- Created: `created` is true, and the Account's `spec.credit` is updated
  (below): credit arriving clears `exhausted`.
- **409** (the key was used): list this customer's credits; if one has the
  `grant_key`, it is a replay: `created` false, `existing.Account` is this
  account. If none has, the key belongs to another account: `created`
  false, `existing.Account` empty. `decideSignupCredit` reads that as
  `card-used`, as before.
- The key is the whole of the idempotency, as the Grant's name was.
- `Ledger.Revoke` (refund, dispute, superseded plan credit): archive the
  credit found by `grant_key` or `payment_intent`, then update
  `spec.credit`.

The order credit is used in: the plan's (ends with the period), then the
sign-up credit (90 days), then purchased credit (12 months), then an
admin's. Plan credit does not roll over: it ends at the period's end.

Money in Metronome is US cents and may be fractional; our API is
micro-dollars: `micros = floor(cents x 10000)`.

## What the Account records

Added to `Account.spec`, written by the backend only:

```yaml
metronomeCustomerId: <uuid>
credit:
  exhausted: true            # no credit left; true from creation until the first grant
  exhaustedAt: <time>        # when it became true; absent while false
  balanceMicros: 0           # as of checkedAt; for display when Metronome cannot be reached
  nextExpiryAt: <time>       # the earliest end of a credit with a balance; absent with none
  checkedAt: <time>
```

`ensureCredit(account)` is the only writer of `spec.credit`:

1. `POST /v1/contracts/customerBalances/list` (with balances) for the
   customer: the net balance and each credit's remaining amount and end.
2. `exhausted` = the balance is 0. `exhaustedAt` = now if it became true,
   kept if it was true, removed if false. `balanceMicros`, `nextExpiryAt`,
   `checkedAt` as read.
3. One `Accounts.Update`.

It runs: after every `EnsureGrant` and `Revoke`; on the alert's webhook;
in the balance pass.

**The balance pass**, in the backend, every 5 minutes: `ensureCredit` for
each Account that has an awake session, or `autoRecharge.enabled`, or a
`nextExpiryAt` in the past. It is what repairs a missed or late webhook
and what notices credit that ran out by expiring. Auto-recharge
(`stripe.md`) is decided in this pass from the balance it has just read.

`Account.status` is no longer written by anything.

## The webhook

`POST /metronome/webhook`, on the API host, public, beside Stripe's.

- Verify: HMAC-SHA256, keyed by `METRONOME_WEBHOOK_SECRET`, of
  `<X-Metronome-Date header> + "\n" + <raw body>`, hex, compared in
  constant time with the `Metronome-Webhook-Signature` header. A date more
  than 5 minutes old, or a bad signature: 400, nothing changed.
- `alerts.low_remaining_contract_credit_and_commit_balance_reached`: find
  the Account by `properties.customer_id`; `ensureCredit`. The handler
  **re-reads Metronome and believes that**, not the event, so replays,
  disorder and stale events are harmless.
- Any other type, or a customer no Account has: 200, nothing changed.
- A failure to read Metronome or write the Account: 500; Metronome retries
  for about two days.

## Reading for the UI

`GET /api/billing` and `/api/billing/usage` (`backend-api.yaml`,
unchanged) read Metronome at the request; nothing is cached.

| Field | From |
|---|---|
| `balanceMicros`, `balances` | the balances list: the net balance, and by `source` custom field with each one's end |
| `level` | `exhausted` if the balance is 0; `low` by the rule of `metering.md`; else `ok` |
| `exhaustedAt`, `sleepAt`, `deleteAt` | `spec.credit.exhaustedAt` |
| `burnMicrosPerHour` | the caller's sessions now and the catalogue's rates |
| `period`, and `Usage` | Metronome's usage for the customer over the period, grouped by `session_id` and by day; money = quantity x the catalogue's rate. The period is the **calendar month, UTC, for everyone** (Metronome's usage statement). |
| `ledger` | `ok`; `pending` while the Account has no Metronome customer; `stale` when Metronome could not be read (then `balanceMicros` is `spec.credit.balanceMicros` and usage is empty) or the observer's Lease is old |

## Configuration and secrets

| Env var | Where | Meaning |
|---|---|---|
| `METRONOME_API_TOKEN` | backend, observer | from the Secret `metronome`. Required when `BILLING` is not `off`. |
| `METRONOME_WEBHOOK_SECRET` | backend | from the Secret `metronome` |
| `METRONOME_URL` | backend, observer | default `https://api.metronome.com`; tests point it at the fake |
| `BILLING_BALANCE_PASS` | backend | default `5m` |

A token is a sandbox token or a production token, and that alone decides
the environment. The sandbox goes with `STRIPE_MODE=test`, production with
`live`; `deploy.md` has the secrets' names.

## Sandbox checks, before code depends on them

Run by the product owner or a developer with their own sandbox token
(scripts in the pull request of the track that needs them; results written
into `docs/billing-metronome.md`). A check that fails is fixed here first.

| # | Check | If it fails |
|---|---|---|
| M1 | A credit made with `uniqueness_key` twice answers 409 the second time; the key stays taken after the credit is archived and after it expires. | the sign-up key also goes on the Account as a label |
| M2 | With list rate 0 and a commit rate, usage draws a `COMMIT_RATE` customer-level credit at the commit rate; after it is used up the balance is 0, the draft invoice total is 0, and nothing more is owed. | credits become prepaid commits with `do_not_invoice` |
| M3 | 3600 `seconds` cost exactly 20 cents; 2628000 `gb_seconds` exactly 28. | the conversion or the unit changes |
| M4 | The alert with `threshold: 0` is accepted, applies to customer-level credits, and fires when usage takes the balance to 0. How long after the event (ten trials). Whether it fires again after credit is added and used up again, and whether it fires when credit expires. | the balance pass is the only signal: its period becomes 1 minute |
| M5 | Burn order: plan (10), then sign-up (20), then pack (30), with all three live. | priorities change |
| M6 | `starting_at` and `ending_before` must be on the hour, or need not be. | the rounding rule above is dropped |
| M7 | An event backdated one hour is charged; an event for an unknown alias: what is answered and whether it is kept. | |
| M8 | The rate limits of the balances list and of ingest, by pushing until a 429. | the pass is paced |
| M9 | Through Pomerium, a test notification from Metronome reaches the handler with a signature that verifies. | as for Stripe's route |
| M10 | Archiving a credit removes its balance at once. | revoke sets the end date instead |
| M11 | Usage grouped by `session_id` and by day can be read back for a customer, and what the endpoint is. | the per-session table comes from the draft invoice's line items |
