# Enforcement contract

Where the backend checks an account, what it answers, and what it stops.

> **Changed 2026-10-02 by [`metronome.md`](metronome.md)**: the ledger is
> Metronome's, and the backend keeps no copy of anything in memory.

A decision reads the owner's `Account` from the API server at that moment
(one GET; no informer, no cache) and `catalogue.yaml`. **It calls neither
Stripe nor Metronome**: whether the account has credit is
`spec.credit.exhausted`, which the backend itself keeps up to date from
Metronome (`metronome.md`, "What the Account records"). So an outage of
either service refuses nobody and stops nobody.

Neither Stripe nor Metronome stops anything. **Stopping is ours.**
Metronome says when the credit is gone; the backend refuses at the doors
and puts sessions to sleep.

## Modes

`BILLING` on the backend and the operator:

| Value | Operator | Backend |
|---|---|---|
| unset or `off` | not deployed, or idle | today's behaviour exactly; no `/api/billing`, no Account is made |
| `meter` | meters | makes Accounts, serves `/api/billing`, takes cards and payments if Stripe is configured; **refuses nothing and stops nothing**. Every decision below is computed and logged as `would_refuse`. |
| `enforce` | the same | everything below applies. Requires `STRIPE_MODE`: without Stripe nobody could ever have a card. |

## Account states

Derived, not stored; the first that matches:

| State | Condition | Can do |
|---|---|---|
| `blocked` | `spec.blocked` or `spec.deletedAt` | nothing; sessions are put to sleep |
| `exempt` | `spec.exempt`, or the email is in `BILLING_EXEMPT_EMAILS` (default: `ADMIN_EMAILS`) | everything, with no card and at any balance; metered for the record |
| `terms` | `TERMS_VERSION` is set and the account has not accepted it | read; accept the terms |
| `no_card` | `spec.paymentMethod.present` is not true | read, stop, delete, add a card. **Create and wake nothing.** Its sessions are put to sleep. Credit it has is kept and waits. |
| `active` | otherwise | everything, within its credit and limits |

`signed in, no card -> card saved -> active` is the first-run path
(`stripe.md`, "Saving a card"). `active -> no_card` happens when the last
payment method is removed; `no_card -> active` when one is back.

## The inputs of a decision

| Name | From |
|---|---|
| `state` | above |
| `exhausted` | `spec.credit.exhausted`; an Account with no `spec.credit` counts as exhausted |
| `tier` | the plan of `spec.subscription` while its status is `active`, `trialing` or `past_due`, otherwise `payg`, looked up in the catalogue (`maxSessions`, `maxAwake`) |
| `mine`, `awake` | the owner's sessions, and how many are `running` or `starting` |
| `clusterAwake` | every user's sessions that are `running` or `starting` |

The account is always the **session's owner's**, not the caller's: an admin
waking someone's session, or an API token, or Terraform, spends the owner's
credit and needs the owner's card. A token acts as its owner.

## Decision table

Evaluated top to bottom; the first row that matches answers. "Start" means
anything that makes a session awake: create, resume, wake on a call.

| # | When | Create | Resume, wake | Already running |
|---|---|---|---|---|
| 1 | `BILLING` is not `enforce` | allow | allow | leave |
| 2 | `blocked` | refuse `account_blocked` | refuse `account_blocked` | sleep now |
| 3 | `exempt` | allow | allow | leave |
| 4 | `terms` | refuse `terms_required` | refuse `terms_required` | leave |
| 5 | `no_card` | refuse `payment_method_required` | refuse `payment_method_required` | **sleep now** (stop sequence, no grace) |
| 6, 7 | (removed: there is no stale ledger to decide on) | | | |
| 8 | `exhausted` | refuse `out_of_credit` | refuse `out_of_credit` | stop sequence |
| 9 | create and `len(mine) >= tier.maxSessions` | refuse `session_limit` | n/a | n/a |
| 10 | `awake >= tier.maxAwake` | refuse `awake_limit` | refuse `awake_limit` | leave |
| 11 | more than `WAKES_PER_HOUR` (30) starts by this account in the last hour | refuse `rate_limited` | refuse `rate_limited` | leave |
| 12 | `clusterAwake >= MAX_AWAKE_SESSIONS` | refuse `at_capacity` | refuse `at_capacity` | leave |
| 13 | otherwise | allow | allow | leave |

**When something of ours or Metronome's is down** the table does not
change, because it reads only the Account:

| What is down | Effect |
|---|---|
| The observer | no usage is sent: that time is free. Nobody is refused or stopped. Logged, and shown as `ledger: stale`. |
| Metronome, for ingest | the observer keeps events for an hour, then drops them: free time |
| Metronome, for reads | `exhausted` keeps its last value. An account with credit carries on; one without stays refused; a purchase's credit is created when Metronome is back (the webhook is retried by Stripe, and the hourly reconcile repeats it). |
| Metronome's alert, late or lost | the balance pass (5 m) sets `exhausted`. Usage past zero is free: the list rate is 0. |
| The API server | the decision cannot be made: 503, as for any other request |

Never over-bill, never lock a customer out for our fault. The code
`metering_unavailable` is no longer answered by a decision.

Row 9 replaces `MAX_SESSIONS_PER_USER` while `BILLING` is `enforce`.

Row 11 counts starts in the backend's memory (a rate limiter: lost on a
restart, which errs towards allowing); row 12 counts from the session list
the decision already reads. Neither is a copy of anyone's credit.

## Sizes

Beside the table, and not a row of it: a create with a `size`, and a resize
(`PATCH` with `size`), are refused with `size_not_included` when the
account's tier does not include that size (`sizes` of `payg` or of the plan
in the catalogue; `small` is in every tier). It is asked after the table
allows the create, and before anything is made. An exempt account has every
size. In `meter` mode it is logged as `would_refuse` and allowed. A session
that already has a size its owner's plan no longer includes keeps it and
wakes at it: only a create and a resize are judged.

That there is room in the cluster for a size is not billing's question: a
session that does not fit is a `409` with `"code": "no_capacity"`
(docs/session-sizes.md), whether billing is on or off.

## Where each check is made

| Entry point | Code today | Check | Refusal reaches the user as |
|---|---|---|---|
| `POST /api/sessions`, `POST /v1/sessions` (UI, API token, Terraform) | `api.create`, before `Store.CreateWithPolicy` and so before any warm-pool claim | Create | the HTTP answer below |
| `PATCH .../sessions/{id}` with `action: resume`, `POST .../sessions/{id}/wake` | `api.patch`, `api.wake` | Resume | the HTTP answer below |
| Any proxied request to a session that is not running: MCP on the sessions host and on the API host, the upload URL, the VNC websocket | `proxy.Waker.EnsureAwake`, before `Store.Wake` | Wake | MCP: below. VNC and upload: the HTTP answer. |
| A **new** proxied request to a running session that is draining (stop sequence) | `proxy`, before forwarding | refused with the reason of the drain | the same |
| Session policy routes, token routes, list, get, delete, rename, stop, sleep (`POST /api/sessions/{id}/sleep`) | | never refused for billing | |

Reading, stopping and deleting always work, in any state and at any
balance, so that a user with no card or no credit can still see their
sessions and remove them (which also ends their disk charges).

Creating an API token or a policy is not gated: neither costs anything, and
neither can start a session without passing the table.

## Answers

HTTP, on the app and the API host:

| Code | Status | Message (the `error` field; the UI adds the buttons) |
|---|---|---|
| `payment_method_required` | 402 | Add a payment method to create or wake sessions. |
| `out_of_credit` | 402 | You are out of credit. Add credit or change plan to continue. |
| `session_limit` | 409 | Your plan allows N sessions. Delete one, or change plan. |
| `awake_limit` | 409 | Your plan runs N sessions at once. Stop one, or change plan. |
| `at_capacity` | 503, `Retry-After: 120` | Every desktop is in use right now. Try again in a few minutes. |
| `rate_limited` | 429, `Retry-After` | Too many starts in the last hour. Try again later. |
| `metering_unavailable` | 503, `Retry-After: 120` | Billing is unavailable right now. Try again in a few minutes. |
| `account_blocked` | 403 | This account is suspended. Contact support. |
| `terms_required` | 403 | Accept the terms to continue. |
| `size_not_included` | 403 | Your plan does not include sessions of this size. Pick a smaller size, or change plan. |

Body: `{"error": "<message>", "code": "<code>", "billingUrl": "<PUBLIC_URL>/billing"}`
(`Error` of `backend-api.yaml`). `billingUrl` is the link an API or
Terraform user follows; the Terraform provider prints `error` and
`billingUrl` as its diagnostic.

`payment_method_required` and `out_of_credit` are **402 with no
`Retry-After`**: they are not transient, and must not look like something a
client should retry. A 5xx or a 429 would.

MCP (the request is JSON-RPC and its client is a program): the same HTTP
status, `Content-Type: application/json`, and a JSON-RPC error whose message
a model can relay to its user:

```json
{"jsonrpc":"2.0","id":null,"error":{"code":-32002,"message":"This session is asleep because its owner is out of credit. It is kept as it was. Add credit at https://app.computeruse.site/billing and call again."}}
{"jsonrpc":"2.0","id":null,"error":{"code":-32002,"message":"This session is asleep because its owner has no payment method. It is kept as it was. Add one at https://app.computeruse.site/billing and call again."}}
```

What Claude's clients show for a 402 on an MCP call, and whether any of
them retries it, is **not verified**; track D records it, and if a client
hides the body or retries, the answer becomes HTTP 200 with the JSON-RPC
error (the request's `id` then has to be read from the body).

## The stop sequence

One sweep in the backend, every 30 s, beside the idle sweep.

**Triggers**

| Trigger | Starts |
|---|---|
| `exhausted` (row 8), not exempt | at `spec.credit.exhaustedAt + BILLING_GRACE` (5 m). The grace exists so that a top-up in progress does not kill work: a purchase or an auto-recharge that lands inside it clears `exhaustedAt` and nothing is stopped. |
| `no_card` (row 5) | at once: no grace |
| `blocked` (row 2) | at once |

**For each running session of the account**, in this order:

1. **Drain.** The session is marked `browserjs.dev/draining: <reason>`
   (`credit`, `payment-method`, `blocked`) with the time. From now the proxy
   refuses every **new** request to it with the reason's answer, and
   closes its VNC viewers and MCP event streams (they are not work in
   progress).
2. **Calls in flight finish.** MCP calls and uploads that were already
   being proxied are left to complete. The bound is the proxy's own:
   `mcpResponseTimeout`, 10 minutes, after which the proxy has given up on
   the call anyway. The sweep waits until the session has no call in
   flight, or `BILLING_DRAIN_TIMEOUT` (10 m) has passed since the mark,
   whichever is first.
3. **Snapshot, then sleep**: `Store.Sleep`, as for an idle session, with
   `browserjs.dev/stopped-by: credit` (or `payment-method`, `blocked`). The
   snapshot failing does not stop the sleep (as today); the session then
   wakes cold. The draining mark is removed.

A session that was **starting** is suspended at once (nothing to drain). A
sleep that fails is retried at the next sweep. The sweep lists Accounts
from the API server each time it runs; it keeps nothing between runs but
what is on the sessions (the draining mark).

If credit arrives (or a card is back) while a session is draining, the mark
is removed, the session stays up and new calls are accepted again.

Awake time after the credit is gone (until Metronome's alert or the
balance pass, then the grace and the drain) is rated at zero and owed by
nobody. The most a user gets this way is about 25 minutes per exhaustion
(up to 5 for the window that is being added up, some minutes for the
alert, 5 of grace, 10 of drain),
and each exhaustion needs a purchase to recover from.

**Afterwards.** A session stopped for `credit` or `payment-method` shows as
`asleep` with that reason (`stoppedBy` on the session view). It is **not
woken automatically** when credit or a card returns: it becomes wakeable,
and wakes on its next use or on Resume, through the table. `Store.Wake`
must accept the two new `stopped-by` values as it accepts `idle`.

## Disks at zero

A disk is charged while its session exists (`metering.md`). At a zero
balance the charge is rated at zero: the balance does not go negative and
nothing is owed. So that this is not free storage for ever:

| Time at zero (`now - spec.credit.exhaustedAt`, continuously) | What happens |
|---|---|
| day 0 | sessions asleep (above). Banner: "You are out of credit. Your sessions are kept until <date>. Add credit to keep them." Each session shows "Deleted on <date> unless you add credit." |
| day 7 | the same banner turns to an error and cannot be dismissed |
| day 13 | banner: "Your sessions will be deleted tomorrow." |
| day 14 (`ZERO_BALANCE_DELETE_AFTER`) | a daily pass deletes the account's sessions, disks and snapshots. The Account, its history and any later credit remain. |

The same clock runs for an account in `no_card`, from
`spec.paymentMethod.removedAt`, **only if** its balance is also zero; a
`no_card` account with credit keeps its sessions, and its disks go on
drawing on that credit.

Any credit arriving clears `exhaustedAt` and with it the clock. Subscribers
are covered by the same rule: a paid renewal is credit arriving.

The deletion pass is switched separately (`ZERO_BALANCE_DELETE`, default
`off`) and is not turned on until the product can send email, because the
notices above are seen only by someone who opens the app (design, 11).
Until then disks at zero are kept and are a cost.

## Auto-recharge

Not enforcement. It runs in the balance pass (`metronome.md`): when the
balance just read from Metronome is below the account's threshold and
auto-recharge is on, the pass asks the Stripe component for a charge
(`stripe.md`, "Auto-recharge"). Its success is credit arriving.

## Sign-up limits (open sign-up)

With `OPEN_SIGNUP` on, the backend makes an Account for any signed-in user,
but not more than `SIGNUPS_PER_DAY` (200) and `SIGNUPS_PER_IP_PER_DAY` (5);
beyond that a new user sees "Sign-ups are paused for today" and no Account
is made. Setup checkouts are limited to 5 per account per day, and an
account may have at most 5 saved cards. Counted from the Accounts' creation
timestamps and an annotation `browserjs.dev/signup-ip-hash` (a salted hash,
deleted after 35 days).
