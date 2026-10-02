# Billing: developing with Stripe

How to work on the backend's Stripe side
([billing-stripe.md](billing-stripe.md)) on this machine. Contract:
[`contracts/billing/deploy.md`](contracts/billing/deploy.md), "Local
development".

## Without Stripe (the default)

Nothing here needs a Stripe account. The tests run on in-memory fakes:

```bash
cd backend
nix develop .. -c go vet ./...
nix develop .. -c go test -race ./...
```

- `internal/billing/stripe/stripetest` is an in-memory Stripe that holds
  customers, cards, checkout sessions, subscriptions and payment intents in
  maps. Each of its helpers (`AttachCard`, `DetachCard`,
  `CompleteCheckout`, ...) returns the event Stripe would send. The clock,
  the accounts and Metronome are `billingtest`'s; the ledger is the real one
  over that fake Metronome.
- A test signs an event with a made-up secret and posts it to the real
  handler: `stripetest.Sign(payload, "whsec_test", time.Now())`. Signature
  verification is not behind an interface: the handler verifies with the
  SDK, as in production.
- `harness_test.go` has the `world` the tests share: the `Service` on the
  fakes, the routes behind a stand-in for the sign-in, and `saveCard`,
  `subscribe`, `buy` for the common paths.
- The real client (`client.go`) is tested against a stand-in for Stripe's
  API in the same process (`client_test.go`): what it sends, and how it
  reads the answers.

`deploy/local` runs with `BILLING=meter` and no Stripe: the buying routes
answer `503 payments_off`.

## With a Stripe sandbox

You need your own sandbox (Dashboard, "Sandboxes") and its secret key. **The
key never goes into the repository, a log, a pull request or a chat**: keep
it in an untracked `.env` (`.local/` is ignored) and export it in the shell
that needs it.

```bash
# .local/stripe.env, untracked
STRIPE_API_KEY=sk_test_...           # the sandbox's secret key, or a restricted one
STRIPE_WEBHOOK_SECRET=whsec_...      # what `stripe listen --print-secret` prints: stable for a key
```

```bash
set -a; . .local/stripe.env; set +a
```

### 1. Stripe's objects

Products, Prices and the portal configuration come from OpenTofu
(`infra/billing`), applied to your sandbox with its setup key. Until they
are there, the backend offers nothing to buy (it logs each lookup key it
finds no price for); saving a card works without them.

### 2. Events

Stripe cannot reach this machine; the Stripe CLI brings its events here:

```bash
stripe listen --forward-to http://localhost:8080/stripe/webhook
```

It prints its `whsec_...` (the same each time for a key): that is
`STRIPE_WEBHOOK_SECRET`. `stripe trigger checkout.session.completed` sends a
canned event.

To see a signature verify with nothing else running, the real handler on
fakes:

```bash
backend/internal/billing/stripe/checks/1-webhook-signature.sh
```

### 3. The backend

The backend takes `STRIPE_MODE=test`, `STRIPE_API_KEY` and
`STRIPE_WEBHOOK_SECRET` (with `BILLING=meter` and `API_URL`). It refuses to
start with a live key in test mode.

Once it runs, in the app: "Add a payment method" opens a setup Checkout.

| Card | For |
|---|---|
| `4242 4242 4242 4242` | a card that works: the sign-up credit, a pack, a subscription |
| `4000 0000 0000 0341` | saves, then declines when charged: a failed renewal, a failed auto-recharge |
| `4000 0025 0000 3155` | requires authentication: an auto-recharge that ends `authentication-required` |
| a prepaid test card, from the list on Stripe's testing page | `signupCredit: refused, prepaid` |

Any future expiry, any CVC, any postal code.

### 4. Time

Renewals, failed payments and cancellations need time to pass: a **test
clock** (a "simulation"; sandbox only, three customers a clock, at most two
billing intervals an advance). `checks/3-test-clock.sh` is a whole
subscription's life on one, and a pattern for others.

### 5. The sandbox checks

`backend/internal/billing/stripe/checks/`, one script each, with what each asks at
its top. Run them with the sandbox key exported; write what they print into
the "Results" table of [billing-stripe.md](billing-stripe.md). They refuse a
key that is not a sandbox key.

## Changing the event table

1. The row in `contracts/billing/stripe.md` first, in a pull request of its
   own.
2. The `case` in `handle` (`webhook.go`). It must end in an ensure function
   that reads Stripe: nothing is written from the event's payload.
3. A case in `eventCases` (`webhook_test.go`): the state the event is about,
   the event, what it must leave. `TestEventTable` runs it once and
   `TestWebhookIdempotency` twice.
4. The event's name in the Dashboard's endpoint: it subscribes to exactly
   the events of the table.

## Logging

`stripe:` begins every line this side logs. Lines in capitals are the ones a
person must act on: two subscriptions on one account, a paid checkout that
matches no account, a disputed payment, a customer deleted at Stripe. A
refused webhook request logs the fact and the source address and nothing
else, and stops logging an address that keeps failing.
