# Testing contract

The product owner's requirement, in their words: "make a test for this that
users without a card cannot create an instance, and a case-coverage test
that has: user without card, create fails; user with card, create works;
user without card, instance sleeps, create fails. If the stateful
components are not interfaces, make them interfaces to make it easy to test
this."

This file fixes the interfaces, the fakes, and the scenarios with their
expected statuses and bodies. The backend tracks implement them as written;
a scenario that cannot be written against these interfaces means an
interface here is wrong, and is fixed here first.

## Interfaces

Every stateful dependency of the billing and enforcement code is a Go
interface, defined in the package that **consumes** it, with an in-memory
fake in `backend/internal/billing/billingtest`. No test of billing or
enforcement logic needs a Kubernetes API (not even client-go's fake),
Stripe or Metronome.

> **Changed 2026-10-02 by [`metronome.md`](metronome.md)**: `Ledger` is
> implemented over Metronome and loses `View`; `Metronome` is new, with a
> fake; `Accounts` is implemented by reads of the API server, with no
> informer. `Accounts`, `Stripe`, `Sessions`, `InFlight` and `Clock` keep
> their signatures.

```go
package billing // backend/internal/billing: the interfaces and the logic over them

// Clock is the only source of time in billing and enforcement code.
type Clock interface{ Now() time.Time }

// Accounts is the account and payment-method state: Account.spec.
type Accounts interface {
	// Ensure returns the owner's Account, making it if there is none.
	Ensure(ctx context.Context, owner string) (Account, error)
	Get(ctx context.Context, name string) (Account, error)
	ByCustomer(ctx context.Context, stripeCustomerID string) (Account, error)
	// ByPaymentMethod finds the Account whose paymentMethod.ids has id.
	ByPaymentMethod(ctx context.Context, id string) (Account, error)
	WithCustomer(ctx context.Context) ([]Account, error)
	// Update applies change to the Account's spec with optimistic
	// concurrency, retrying on conflict. It is the only way spec is written.
	Update(ctx context.Context, name string, change func(*AccountSpec) error) (Account, error)
}

// Ledger is the credit, kept in Metronome (metronome.md). Its production
// implementation is written over the Metronome interface below and holds
// no state. No decision at create, resume or wake calls it: they read
// Account.spec.credit.
type Ledger interface {
	// EnsureCustomer makes the account's Metronome customer and contract if
	// there are none, and returns the customer's ID.
	EnsureCustomer(ctx context.Context, account string) (customerID string, err error)
	// Balance reads the account's credit from Metronome now: the net
	// balance, and what is left of each credit with its source and end.
	Balance(ctx context.Context, account string) (Balance, error)
	// Usage reads what the account used between from and to, by session
	// and by day.
	Usage(ctx context.Context, account string, from, to time.Time) (Usage, error)
	// EnsureGrant creates the credit whose uniqueness key is g.Key, then
	// runs EnsureCredit. created is false when the key was used: existing
	// is that credit if it is this account's (a replay), and has an empty
	// Account if it is another's.
	EnsureGrant(ctx context.Context, g Grant) (created bool, existing Grant, err error)
	// Revoke archives the credits sel finds, then runs EnsureCredit.
	Revoke(ctx context.Context, sel GrantSelector, reason string) error
	// EnsureCredit reads Balance and writes Account.spec.credit (exhausted,
	// exhaustedAt, balanceMicros, nextExpiryAt, checkedAt). The only writer
	// of that field.
	EnsureCredit(ctx context.Context, account string) (AccountCredit, error)
}

// Metronome is every call the backend makes to Metronome (metronome.md),
// as Stripe is for Stripe. One method per endpoint; no logic.
type Metronome interface {
	CreateCustomer(ctx context.Context, p MetronomeCustomerParams) (id string, err error)
	CustomerByAlias(ctx context.Context, alias string) (id string, found bool, err error)
	CreateContract(ctx context.Context, p MetronomeContractParams) error // a 409 is nil
	// CreateCredit reports conflict (not an error) when the key was used.
	CreateCredit(ctx context.Context, p MetronomeCreditParams) (id string, conflict bool, err error)
	Credits(ctx context.Context, customer string) ([]MetronomeCredit, error) // with balances and custom fields
	ArchiveCredit(ctx context.Context, customer, id string) error
	Usage(ctx context.Context, customer string, from, to time.Time) (MetronomeUsage, error)
}

// Stripe is every call the backend makes to Stripe (stripe.md).
type Stripe interface {
	CreateCustomer(ctx context.Context, p CustomerParams) (id string, err error)
	CreateCheckout(ctx context.Context, p CheckoutParams) (CheckoutSession, error) // setup, subscription or payment
	CreatePortal(ctx context.Context, customer, returnURL string) (url string, err error)
	CreateRecharge(ctx context.Context, p RechargeParams) (PaymentIntent, error)
	Checkout(ctx context.Context, id string) (CheckoutSession, error)
	PaymentMethods(ctx context.Context, customer string) (methods []PaymentMethod, defaultID string, err error)
	Subscription(ctx context.Context, id string) (Subscription, error)
	PaymentIntent(ctx context.Context, id string) (PaymentIntent, error)
	Prices(ctx context.Context, lookupKeys []string) (map[string]string, error)
	CancelSubscription(ctx context.Context, id string) error
	DetachPaymentMethod(ctx context.Context, id string) error
	// ...the list calls of the reconcile
}

// Sessions is what enforcement and the API need of the session store.
type Sessions interface {
	Create(ctx context.Context, name, owner string, policy *sessions.PolicySpec) (sessions.Session, error)
	Get(ctx context.Context, id string) (sessions.Session, error)
	List(ctx context.Context, owner string) ([]sessions.Session, error)
	ListAll(ctx context.Context) ([]sessions.Session, error)
	// Sleep snapshots the session, then suspends it, recording why.
	Sleep(ctx context.Context, id, stoppedBy string, stillWanted func() bool) error
	Wake(ctx context.Context, id string) error
	Update(ctx context.Context, id string, name *string, action string) error
	Delete(ctx context.Context, id string) error
	SetDraining(ctx context.Context, id, reason string) error // "" clears it
}

// InFlight is the proxy's knowledge of work in progress on a session.
type InFlight interface {
	Calls(id string) int      // MCP calls and uploads being proxied now
	CloseStreams(id string)   // VNC viewers and MCP event streams
}
```

Webhook signature verification is **not** behind an interface: the real
handler verifies with the SDK against a secret, and tests sign their events
with a made-up secret (`whsec_test`) using the SDK's test helper or an
HMAC-SHA256 of `"<timestamp>.<body>"`. The fake `Stripe` holds the state an
event refers to (a customer's payment methods, a subscription), because
every handler re-reads Stripe before acting.

### Existing concrete types that go behind an interface

| Today | Consumed by | Change (track D) |
|---|---|---|
| `*sessions.Store` | `api.API.store` (`api.New`) | the field and the constructor take an interface declared in `api` with the methods it calls; `*sessions.Store` satisfies it unchanged |
| `*sessions.Store` | `proxy.Waker.Store` | the same, an interface declared in `proxy` (`Get`, `Wake`, `ColdStart`) |
| `*sessions.Store` | `idle.Sweep`, `idle.Run` | the same, an interface declared in `idle` (`ListAll`, `Sleep`) |
| `*sessions.Store` | `policy.Handlers.store` | not changed (not on the billing path) |
| `Store.Sleep(ctx, id, stillIdle)` | the idle sweep | gains the reason (`idle`, `credit`, `payment-method`, `blocked`); `Store.Wake` accepts the three sleeping reasons |
| `time.Now()` in `sessions`, `idle`, `proxy.Waker.clock` | | billing code takes `Clock`; existing code is left as it is |
| the proxy's per-session connection counts (`idle.Tracker.Open`) | | the proxy also counts MCP calls and uploads in flight and exposes `InFlight` |

`sessionstest.New` (the real `Store` over client-go's fake dynamic client)
stays for the store's own tests and is used by the warm-pool case below.

### Fakes (`billingtest`)

| Fake | Behaviour |
|---|---|
| `Clock` | set and advanced by the test |
| `Accounts` | a map; `Update` applies the change under a mutex |
| `Metronome` | customers, contracts and credits in maps (a used `uniqueness_key` answers conflict), and the usage it was sent. `Tick(now)`, called by the test, observes the fake `Sessions`, runs **the reference step** ported to Go and checked against `metering-vectors.json`, and draws the credits down in priority order, never below zero. It returns the **alert event** Metronome would send when a customer's balance reached zero in that tick (nil otherwise), for the test to sign with a made-up secret and post to `POST /metronome/webhook`. `Unreachable(true)` makes every call fail. |
| `Ledger` | **not faked**: the real implementation, over the fake `Metronome` and the fake `Accounts` |
| `Stripe` | customers, payment methods (with `fingerprint`, `funding`, `wallet`), checkout sessions, subscriptions, payment intents in maps; helpers `AttachCard(customer, card)`, `DetachCard(pm)`, `CompleteCheckout(id)`; each helper returns the **event** Stripe would send, for the test to sign and post |
| `Sessions` | a map of sessions with a state machine (`running`, `asleep`, `stopped`), a count of snapshots taken per session, the `stoppedBy` reason, the draining mark |
| `InFlight` | counts set by the test |

## Scenario 1 (required): the card gate

`TestCardGateScenario`, in `backend/internal/billing`, step-driven, one
user `u@example.com`, `BILLING=enforce`, the real API mux, the real webhook
handlers (Stripe's and Metronome's), the real sweep and balance pass (run
by the test, not on a timer), the fakes above. The catalogue is
`catalogue.yaml` of this directory.

In every scenario below, "`Metronome.Tick`" is `Tick(now)` on the fake
and, **if it returns an alert event, the test posting it signed to
`POST /metronome/webhook`**. "Grant" reads "credit in the fake Metronome,
by its uniqueness key". "`level: exhausted`, `exhaustedAt` set" reads
"`spec.credit.exhausted` true and `exhaustedAt` set, and `GET /api/billing`
says `level: exhausted`". The steps, statuses and bodies are unchanged.

| Step | Action | Expected |
|---|---|---|
| a1 | `GET /api/billing` | 200, `state: no_card`, `hasPaymentMethod: false`, `balanceMicros: 0` |
| a2 | `POST /api/sessions` `{}` | **402**, body `{"error":"Add a payment method to create or wake sessions.","code":"payment_method_required","billingUrl":"https://app.example.test/billing"}`. `Sessions` has no session. No Grant exists. |
| b1 | `POST /api/billing/checkout` `{}` | 200 with a `url`; the fake Stripe has one Checkout Session, `mode: setup`, for the account's customer |
| b2 | fake `CompleteCheckout` attaches card A (fingerprint `fpA`, funding `credit`); the test posts the signed `checkout.session.completed` to `POST /stripe/webhook` | 200. Account: `paymentMethod.present: true`, `signupCredit.state: granted`. Exactly one Grant, key `signup/fpA`, 5000000 micro-dollars. |
| b3 | the same event posted again; then `payment_method.attached` for the same card | 200 both. Still exactly one Grant. |
| b4 | `Metronome.Tick`; `GET /api/billing` | `state: active`, `balanceMicros: 5000000`, `signupCredit.state: granted` |
| b5 | `POST /api/sessions` `{}` | **201**; the session is `running`, owned by the user |
| c1 | fake `DetachCard(A)`; the test posts the signed `payment_method.detached` (its object has `customer: null`) | 200. Account: `paymentMethod.present: false`, `removedAt` set. The Grant is untouched. |
| c2 | the sweep runs | the session has one snapshot, is `asleep`, `stoppedBy: payment-method`. `GET /api/sessions/{id}` shows `state: asleep`, `stoppedBy: payment-method`. |
| c3 | `Metronome.Tick`; `GET /api/billing` | `state: no_card`, `balanceMicros` is 5000000 less what was charged: the credit is kept |
| d1 | `POST /api/sessions` `{}` | **402** `payment_method_required`, as a2. Still one session. |
| d2 | `PATCH /api/sessions/{id}` `{"action":"resume"}` | **402** `payment_method_required`. The session is still `asleep`. |
| d3 | an MCP request to the session through the proxy (`POST /<id>/mcp`) | **402**, `Content-Type: application/json`, a JSON-RPC error with code -32002 whose message contains "no payment method" and the billing URL. No `Retry-After` header. `Sessions.Wake` was not called. |
| e1 | fake attaches card B (fingerprint `fpB`); the test posts `payment_method.attached` | 200. `paymentMethod.present: true`, `removedAt` cleared. **Still exactly one Grant** (`signupCredit` was already decided). |
| e2 | `GET /api/sessions/{id}` | still `asleep` (it is not woken automatically) |
| e3 | `PATCH /api/sessions/{id}` `{"action":"resume"}` | 200, the session is `running` |
| e4 | (variant) instead of e3, the MCP request of d3 | the session is woken and the request forwarded |

## Simple tests (required)

| Test | Where | Asserts |
|---|---|---|
| `TestNoCardCannotCreate` | `backend/internal/api` | a user whose account has no card: `POST /api/sessions` is 402 `payment_method_required` and the store's `Create` was never called |
| `TestNoCardCannotCreateWithToken` | `backend/internal/api`, the API host | the same with an API token of that user on `POST /v1/sessions` |
| `TestNoCardCannotCreateWarm` | `backend/internal/api` with `sessionstest.New` and a warm pool configured | the same, and **no `SandboxClaim` was created** and no warm Sandbox was adopted |
| `TestExemptNeedsNoCard` | `backend/internal/billing` | an address in `BILLING_EXEMPT_EMAILS` with no card creates a session |
| `TestBillingOffIsToday` | `backend/internal/api` | with `BILLING` unset, the responses of create, resume and wake are byte for byte today's, and no Account is made |
| `TestMeterModeRefusesNothing` | `backend/internal/billing` | `BILLING=meter`, no card: create succeeds and a `would_refuse` line is logged |

## Scenario 2: exhaustion with a call in flight

`TestExhaustionDrain`. An active account with one running session, a
balance of 3000 micro-dollars, `BILLING_GRACE` 5 m, `BILLING_DRAIN_TIMEOUT`
10 m.

| Step | Action | Expected |
|---|---|---|
| 1 | clock +60 s, `Metronome.Tick` | `level: exhausted`, `exhaustedAt` set; the sweep does nothing yet; a new MCP request is still forwarded |
| 2 | `POST /api/sessions` | 402 `out_of_credit` |
| 3 | clock to `exhaustedAt` + 5 m; `InFlight.Calls` = 1; the sweep runs | the session is marked draining `credit`; `CloseStreams` was called; **no snapshot yet, still running** |
| 4 | a new MCP request | 402, JSON-RPC error containing "out of credit"; not forwarded |
| 5 | `InFlight.Calls` = 0; the sweep runs | one snapshot, `asleep`, `stoppedBy: credit`, the draining mark gone |
| 5' | (variant) the call never ends; clock +10 m; the sweep runs | the same as 5 |
| 6 | (variant, from 3) a pack is bought (signed `checkout.session.completed`), `Metronome.Tick`, the sweep runs | the draining mark is removed, the session is still `running`, a new MCP request is forwarded, no snapshot was taken |
| 7 | from 5: a pack is bought, `Metronome.Tick` | the session is still `asleep`; `resume` now succeeds |

## Scenario 3: disk charges while asleep

`TestDiskAccruesAsleep`, on the fake ledger (and as a vector for the
operator). One session, asleep, a balance of $1.

| Step | Expected |
|---|---|
| 24 hours of ticks | the balance fell by 5 x 24 x 384 = 46080 micro-dollars; no awake charge |
| ticks until the balance is 0 | `level: exhausted`, `exhaustedAt` set; further ticks add to `overdraftMicros` only; the balance stays 0 |
| `GET /api/billing` with `ZERO_BALANCE_DELETE=on` | `deleteAt` = `exhaustedAt` + 14 days; the session view has the same `deleteAfter` |
| clock to `deleteAt`; the deletion pass runs | the session is deleted; the Account remains |
| (variant) credit bought at day 10 | `exhaustedAt` and `deleteAt` gone; the session remains |
| (variant) `ZERO_BALANCE_DELETE=off` | nothing is ever deleted |

## Scenario 4: webhook replay and disorder

`TestWebhookIdempotency`, table-driven over the event table of `stripe.md`.

| Case | Expected |
|---|---|
| every event posted twice | the state after the second is the state after the first |
| a subscription's events in reverse order (`invoice.paid`, then `customer.subscription.updated`, then `.created`) | one plan Grant for the period |
| `payment_method.detached` arriving **before** the `payment_method.attached` of the same card (the fake's state already has it detached) | `present: false` after both |
| `payment_method.attached` replayed after the card was detached | `present: false` (the handler re-reads) |
| the same card saved on a second account | that account is active, `signupCredit: refused, card-used`, no second Grant |
| a prepaid card; a wallet card | active, `refused` with `prepaid` / `wallet`, no Grant |
| a bad signature; a timestamp 6 minutes old; `livemode` of the other mode | 400, nothing changed |
| an event for a customer no Account has | 200, nothing changed |
| auto-recharge: the balance pass crashes after writing `seq` and before the call, then runs again | one PaymentIntent (the same idempotency key), one Grant |

## Scenario 5: Metronome's webhook and its absence

`TestMetronomeWebhook` and `TestMetronomeUnreachable`.

| Case | Expected |
|---|---|
| the alert event posted twice; posted after credit was bought (a stale event) | the Account's `credit` is what the fake Metronome's balance says each time: the handler re-reads |
| a bad signature; an `X-Metronome-Date` 6 minutes old | 400, nothing changed |
| an alert for a customer no Account has; an event of another type | 200, nothing changed |
| the alert is never posted; the balance pass runs | `exhausted` is set by the pass |
| a credit's end passes with nothing awake; the balance pass runs | `exhausted` is set (the pass follows `nextExpiryAt`) |
| `Unreachable(true)`; an active account with credit: create, resume, wake | all allowed; no call to Metronome was made by the decision |
| `Unreachable(true)`; an exhausted account: create | 402 `out_of_credit` |
| `Unreachable(true)`; a pack's `checkout.session.completed`; then `Unreachable(false)` and the event again | 500 the first time (Stripe retries); 200 the second, one credit, `exhausted` false |
| `Unreachable(true)`; `GET /api/billing` | 200, `ledger: stale`, `balanceMicros` = `spec.credit.balanceMicros` |
| a search of the backend's billing packages | no informer, no cache type and no package-level map holding accounts or balances (`TestNoLedgerCopy`: the production `Accounts` and `Ledger` are constructed with only their clients, and two calls make two reads) |

## The observer

Its seconds function runs every vector of `metering-vectors.json`
(`awakeSeconds`, `diskGBSeconds`), and the property tests of the tracks
document that are about seconds. Its sender is tested against a fake
ingest endpoint: the same tick sent twice has the same `transaction_id`s;
batches hold at most 100 events; a 5xx is retried with the same keys and
given up after an hour; a 4xx is dropped and logged; a restart charges
nothing for the time it was down beyond `MAX_GAP`. The Go port of the step
in `billingtest` runs the whole file, money included.
