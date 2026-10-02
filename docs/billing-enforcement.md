# Billing: accounts and enforcement in the backend

How the backend decides who may start a session, and what it puts to sleep.
The contract is [`contracts/billing/enforcement.md`](contracts/billing/enforcement.md);
this page says where each part of it lives in the code, how it is switched,
and how it is tested. Track D of
[the plan](plans/2026-10-02-metering-billing-tracks.md).

## The switch

`BILLING` on the backend:

| Value | What the backend does |
|---|---|
| unset, `off` | Nothing. No Account is read or made, no route exists, no check is made, the session view has no billing field. The server is byte for byte what it was (`TestBillingOffIsToday`, `TestBillingOffIsNotWiredIn`). |
| `meter` | Makes an Account for each user it sees, serves the read routes, works every decision out and logs what it would have done (`would_refuse`, `would_stop`). Refuses nothing, stops nothing. `MAX_SESSIONS_PER_USER` still applies. |
| `enforce` | The decision table applies at create, resume and wake; the sweep runs the stop sequence. The plan's `maxSessions` replaces `MAX_SESSIONS_PER_USER`. Refuses to start without `STRIPE_MODE`. |

With `BILLING` not off the backend needs the `accounts` custom resource
served, the catalogue at `BILLING_CATALOGUE` and `METRONOME_API_TOKEN`; it
fails at start, naming what is missing (never a secret's value). Without
`METRONOME_WEBHOOK_SECRET` there is no webhook route and the balance pass
alone notices credit running out.

The other settings and their defaults are in
[`contracts/billing/deploy.md`](contracts/billing/deploy.md):
`BILLING_GRACE`, `BILLING_DRAIN_TIMEOUT`, `BILLING_BALANCE_PASS`,
`BILLING_EXEMPT_EMAILS`, `MAX_AWAKE_SESSIONS`, `WAKES_PER_HOUR`,
`ZERO_BALANCE_DELETE`, `ZERO_BALANCE_DELETE_AFTER`, `SIGNUP_CREDIT`,
`METRONOME_URL`.

## Where the credit is

Metronome is the meter and the ledger
([`contracts/billing/metronome.md`](contracts/billing/metronome.md)). The
backend keeps no copy of it:

- A decision at create, resume or wake reads the owner's `Account` from the
  API server at that moment (no informer, no cache) and calls nobody else.
  Whether there is credit is `spec.credit.exhausted`.
- `spec.credit` has one writer, `Ledger.EnsureCredit`, which reads the
  balance from Metronome and writes what it found. It runs after every
  grant and revoke, when Metronome's zero-balance alert arrives
  (`POST /metronome/webhook`, on the API host), and in the balance pass
  (every `BILLING_BALANCE_PASS`, for accounts with an awake session, with
  auto-recharge on, or with a credit whose end has passed).
- `GET /api/billing` and `/api/billing/usage` read Metronome at the
  request. When it cannot be read they answer with the balance last stored
  on the Account and `ledger: stale`.

**What a decision costs.** One GET of the owner's Account from the API
server, and for an account that passes the account's own rows, one list of
the owner's sessions and one of everyone's (for the plan's and the
cluster's limits). It is made at create, at resume, and when a request
finds a session asleep and is about to wake it: per start, not per call. A
proxied request to a running session reads no Account; the draining check
uses the session the proxy has already read.

**Per mode.** An Account keeps its Stripe state under `spec.stripe.<test|live>`
and its Metronome state under `spec.metronome.<sandbox|production>`.
`kube.Accounts` is made for one mode (from `STRIPE_MODE`; test and sandbox
while it is unset) and shows `billing.AccountSpec` as it is in that mode,
leaving the other mode's state untouched. Nothing else in the code knows
there are modes. Switching test to live clears nothing.

**The seam.** `billing.Accounts` and `billing.Ledger` are the only things
enforcement holds. `kube.Accounts` is a thin adapter over the custom
resource; a client of another store replaces it in
`cmd/server/billing.go`, where both are constructed, and nowhere else.

So with Metronome unreachable nobody is refused and nobody is stopped: an
account with credit carries on, one without stays refused, and a purchase
answers Stripe's webhook with 500 until the credit can be made
(`TestMetronomeUnreachable`).

## Where things are

| | |
|---|---|
| `backend/internal/billing/billing.go` | The interfaces every stateful dependency is behind (`Clock`, `Accounts`, `Ledger`, `Metronome`, `Stripe`, `Sessions`, `InFlight`, `Observer`) and their types. |
| `ledger.go` | `NewLedger`: the `Ledger` over the `Metronome` interface and the Accounts. It holds no state. |
| `decide.go` | Account states, the decision table (`Decide`, a pure function), the answers in their HTTP and MCP forms. |
| `enforcer.go` | `Enforcer`: gathers a decision's inputs and makes it at the three doors (`Create`, `Start`), counts starts for the rate limit, says what a session's view carries. |
| `sweep.go` | `Sweep` (drain, wait for calls in flight, snapshot, sleep) and `DeletePass` (sessions of an account at zero for 14 days). |
| `handlers.go` | `GET /api/billing`, `/api/billing/usage`, `/api/billing/catalogue`, `DELETE /api/account`. |
| `catalogue.go` | `catalogue.yaml` as a type; the file is looked at every 10 s and re-read when it changed; one that does not parse keeps the last good one. |
| `kube/` | `Accounts` over the cluster: every read is a read of the API server; writes carry the read's `resourceVersion` and are retried on conflict. The observer's Lease. |
| `metronome/` | The HTTP client (`billing.Metronome`), the webhook, the balance pass. The metrics, products, rate card and alert are OpenTofu's (`infra/billing`); the client finds the Credit product and the two metrics by name. |
| `billingtest/` | The in-memory fakes. The fake Metronome holds the credit, runs the Go port of the metering step in `Tick`, draws credit down in priority order and returns the alert it would send. The `Ledger` is not faked. |
| `scenarios/` | The scenario tests. |
| `backend/internal/api/billing.go` | The two checks in the API (before create, before resume) and billing's fields on the session view. |
| `backend/internal/proxy/billing.go` | The check before a wake, the refusal of new requests to a draining session, the count of calls in flight. |
| `backend/cmd/server/billing.go` | The wiring. |

## The enforcement points

Each is one call, and does nothing when billing is off (the field it calls
through is nil):

| Where | Call | Then |
|---|---|---|
| `api.create`, after the user's sessions are listed and before `Store.CreateWithPolicy` (and so before any warm-pool claim) | `Billing.Create(owner, mine)` | a refusal is the answer; nothing was made |
| `api.patch` with `action: resume`, for a session that is not awake | `Billing.Start(session)` | a refusal is the answer; nothing was written, not even a rename in the same request |
| `proxy.Waker`, before `Store.Wake` (`Waker.Allow`) | `Billing.Start(session)` | the refusal travels up as the wait's error; `Store.Wake` is not called |
| `proxy`, after a session is found and before a request is sent to its pod (MCP, the event stream, uploads, VNC, files) | `Billing.Draining(session)` | a session marked draining takes no new request |

The account judged is the **session's owner's**, whoever asks: an admin
waking a user's session, an API token, Terraform.

Reading, renaming, stopping and deleting are not checked at all.

## The stop sequence

`Enforcer.Sweep`, every 30 s, for each account that has sessions:

1. The account's running sessions are to stop when it is blocked or has no
   card (at once), or `spec.credit.exhausted` is true and
   `spec.credit.exhaustedAt + BILLING_GRACE` has passed.
2. Each running session is marked `browserjs.dev/draining: <reason>`, with
   `browserjs.dev/draining-since`. `InFlight.CloseStreams` ends its VNC
   viewers and MCP event streams and makes the proxy forget what it
   remembered of the session, so the next request sees the mark.
3. While `InFlight.Calls(id)` is above zero and `BILLING_DRAIN_TIMEOUT` has
   not passed since the mark, the sweep leaves it and looks again in 30 s.
4. `Store.Sleep(id, reason, stillWanted)`: the snapshot, then the suspend
   with `browserjs.dev/stopped-by: <reason>`. `stillWanted` asks the account
   again after the snapshot, so credit that arrived meanwhile leaves the
   session running.
5. A mark whose reason has gone is removed, and new requests are taken
   again.

A session that is still starting is suspended at once, with no snapshot.

`credit` and `payment-method` show as `asleep` and wake on the next request
or on Resume, through the table. `blocked` shows as `stopped`: `Store.Wake`
does not take it, and Resume is refused while the account is blocked.

What the proxy counts as a call in flight: an MCP `POST` or `DELETE`, an
artifact upload, a file upload from the session page. Not the MCP event
stream (`GET`), not a VNC viewer.

## Routes

`GET /api/billing`, `GET /api/billing/usage`, `GET /api/billing/catalogue`
and `DELETE /api/account`, as
[`contracts/billing/backend-api.yaml`](contracts/billing/backend-api.yaml).
Cookie only for now: see "Not done here".

`DELETE /api/account` goes to Stripe first (the subscription cancelled, each
card detached), so that a failure there deletes nothing; then the sessions,
the API tokens, the credit (archived in Metronome), and last the Account is
marked `deletedAt`. The two customer IDs stay on it: the CRD does not let
them go once set.

## Tests

Everything runs on the fakes: no Kubernetes API, no Stripe.

```
cd backend && go vet ./... && go test -race ./...
```

| Test | Where | What |
|---|---|---|
| `TestCardGateScenario` | `billing/scenarios` | Steps a1 to e4 of the contract, twice (ending with e3, and with its variant e4). |
| `TestNoCardCannotCreate`, `TestNoCardCannotCreateWithToken`, `TestNoCardCannotCreateWarm` | `api` | The refusal at the handler; through the API host with a token; with the real store and a warm pool (no `SandboxClaim`). |
| `TestBillingOffIsToday` | `api` | The bodies of create, resume and wake with billing off, as they were before billing. The same test passes on the commit before this work. |
| `TestExemptNeedsNoCard`, `TestMeterModeRefusesNothing` | `billing/scenarios` | |
| `TestExhaustionDrain` | `billing/scenarios` | Steps 1 to 7 and 5'; the same with a real call held open in the pod and counted by the proxy; a starting session. |
| `TestMetronomeWebhook`, `TestMetronomeUnreachable` | `billing/scenarios` | Scenario 5: the alert twice and stale, bad signatures, the alert lost (the balance pass), a credit expiring, and everything with Metronome down. |
| `TestNoLedgerCopy` | `billing` | Two calls make two reads of Metronome and of the Account; no informer, cache type or package-level map in the billing packages. `TestEveryReadIsARead` (`billing/kube`) says the same of the cluster's Accounts. |
| `TestClientCalls` | `billing/metronome` | What the client sends, against a stand-in written from `metronome.md`. |
| `TestDiskAccruesAsleep` | `billing/scenarios` | The disk charge, the overdraft, `deleteAt`, the deletion pass on and off, credit at day 10. |
| `TestDecisionTable`, `TestDecisionOrder`, `TestStateOf`, `TestAnswers` | `billing` | One case per row and column; the order of the rows; every answer's status, message and `Retry-After`. |
| `TestStepRunsTheMeteringVectors` | `billing/billingtest` | The 18 vectors of `metering-vectors.json` through the Go port of the step, money included. |
| `TestTheOwnersAccountIsJudged`, `TestNoCardCanStillReadStopAndDelete`, `TestBlockedAccount`, `TestLimits`, `TestDeleteAccount` | `billing/scenarios` | |
| `TestSweepOverTheRealStore`, `TestBillingIsWiredIn` | `cmd/server` | The sweep and the gate through the server's own route table and the real session store. |

## The Stripe side

Track C's package (`backend/internal/billing/stripe`) has the checkout
route and the webhook handler. Until it is merged the scenarios post their
correctly signed events to `billingtest.StripeStub`, which implements
`ensurePaymentMethods`, `decideSignupCredit` and `ensurePurchase` of
`stripe.md` against the same interfaces. The stub is deleted when C merges,
and the scenarios must then pass unchanged with C's handler in its place.

The balance pass offers auto-recharge to a `metronome.Recharger` (nil until
C supplies one), with the balance it has just read.

## Metronome: what is not verified

Nothing here has been run against Metronome. The client's paths and the
shapes of its requests and answers are from `metronome.md` and Metronome's
documentation as it records them; its tests run against a stand-in written
from the same reading, so they show what the client sends, not what
Metronome accepts. The sandbox checks M1 to M11 of the contract settle
that, and the ones this code leans on are:

| Check | What depends on it here |
|---|---|
| M1 | `CreateCredit` reading a 409 as "the key was used" |
| M4 | the alert's type and that `properties.customer_id` names the customer; the webhook's signature (`X-Metronome-Date`, a newline, the body) |
| M6 | credits starting and ending on the hour |
| M10 | `ArchiveCredit`, which sets the credit's end to now (`customerCredits/updateEndDate`) |
| M11 | `Usage`, which asks `/v1/usage/groups` by `session_id` and day |
| not numbered | finding a customer by alias (`GET /v1/customers?ingest_alias=`), the products and metrics lists the client finds its IDs in |

## What MCP clients show for a 402

**Not verified.** The contract asks track D to record what Claude's MCP
clients show for a 402 from a session's MCP endpoint and whether any of
them retries it. That needs the real clients against a deployed backend
with `BILLING=enforce`, which is stage 3; it has not been done. Until it
is, the answer is the contract's first form: HTTP 402, `Content-Type:
application/json`, a JSON-RPC error `-32002` with `id: null`, no
`Retry-After`. If a client hides the body or retries, the contract's
fallback (HTTP 200 with the JSON-RPC error, the request's `id` read from
the body) is a change in `Refusal.WriteMCP` and one line in
`proxy.refused`.

## Not done here

- `GET /v1/billing` and `GET /v1/billing/usage` on the API host. The API
  host's route list is in `backend/internal/auth`, which this track does
  not touch; adding the two routes there is two lines.
- `card` (brand, last four digits) in `GET /api/billing`. The Account does
  not hold it, and the backend does not call Stripe to answer a read.
- `TERMS_VERSION` is not read from the environment (track G). The state
  `terms` is derived when `billing.Config.TermsVersion` is set.
- The deletion pass runs every hour rather than once a day.
- The balance pass finds accounts whose credit has expired among those that
  have a Stripe customer (and those with an awake session). An account
  with only an admin's grant and nothing awake is read again when it is
  next used.
