# Enforcement contract

Where the backend checks an account, what it answers, and what it stops.
The backend reads `Account.status` (through an informer: no call per
request) and `catalogue.yaml`; it never calls Stripe to decide anything.

## Modes

`BILLING` on the backend and the operator:

| Value | Operator | Backend |
|---|---|---|
| unset or `off` | not deployed, or idle | today's behaviour exactly; no `/api/billing`, no Account is made |
| `meter` | meters, makes free grants | makes Accounts, serves `/api/billing`, takes payments if Stripe is configured; **refuses nothing and stops nothing**. Every decision of the table below is computed and logged as `would_refuse`. |
| `enforce` | the same | the table below applies |

## The inputs of a decision

| Name | From |
|---|---|
| `blocked` | `spec.blocked` present, or `spec.deletedAt` present |
| `exempt` | `spec.exempt`, or the caller's email is in `BILLING_EXEMPT_EMAILS` (default: `ADMIN_EMAILS`) |
| `balance` | `status.balanceSeconds` |
| `tier` | `status.plan` looked up in `catalogue.yaml` (limits `maxSessions`, `maxAwake`) |
| `stale` | `status.meter.observedAt` is older than `BILLING_STALE_AFTER` (10 m), or there is no status |
| `paying` | `status.plan` is not `free` (as last written) |
| `mine`, `awake` | the caller's sessions, and how many are `running` or `starting` |
| `clusterAwake` | every user's sessions that are `running` or `starting` |
| `terms` | `spec.termsAcceptedAt` set and `termsVersion` current (only checked when `OPEN_SIGNUP` is on) |

The account is always the **session's owner's**, not the caller's: an admin
waking someone's session, or an API token, spends the owner's hours. A token
acts as its owner, so the two are the same for tokens.

## Decision table

Evaluated top to bottom; the first row that matches answers. "Start" means
anything that makes a session awake: create, resume, wake.

| # | When | Create | Resume, wake | Already running |
|---|---|---|---|---|
| 1 | `BILLING` is not `enforce` | allow | allow | leave |
| 2 | `blocked` | refuse `account_blocked` | refuse `account_blocked` | sleep now |
| 3 | `exempt` | allow | allow | leave |
| 4 | `OPEN_SIGNUP` and not `terms` | refuse `terms_required` | refuse `terms_required` | leave |
| 5 | `stale` and `paying` | allow | allow | leave |
| 6 | `stale` and not `paying` | refuse `metering_unavailable` | refuse `metering_unavailable` | leave |
| 7 | `balance == 0` | refuse `out_of_hours` | refuse `out_of_hours` | sleep at `exhaustedAt + BILLING_GRACE` |
| 8 | create and `len(mine) >= tier.maxSessions` | refuse `session_limit` | n/a | n/a |
| 9 | `awake >= tier.maxAwake` | refuse `awake_limit` | refuse `awake_limit` | leave |
| 10 | more than `WAKES_PER_HOUR` (30) starts by this account in the last hour | refuse `rate_limited` | refuse `rate_limited` | leave |
| 11 | `clusterAwake >= MAX_AWAKE_SESSIONS`, or not `paying` and `clusterAwake >= FREE_AWAKE_CEILING` | refuse `at_capacity` | refuse `at_capacity` | leave |
| 12 | otherwise | allow | allow | leave |

Row 5 and 6 are the answer to "fail open or closed": with the ledger stale,
people who pay are let through and their time is not counted (never
over-bill, never lock out a customer because of our fault), and people who
do not pay wait. Row 8 replaces `MAX_SESSIONS_PER_USER` while `BILLING` is
`enforce`; the old setting remains the limit otherwise.

The count for row 10 and the counts for row 11 are in the backend's memory
and its informer: lost on a restart, which errs towards allowing.

## Where each check is made

| Entry point | Code today | Check | Refusal reaches the user as |
|---|---|---|---|
| `POST /api/sessions`, `POST /v1/sessions` (UI, API token, Terraform) | `api.create` | Create | the HTTP answer below |
| `PATCH .../sessions/{id}` with `action: resume` | `api.patch` | Resume | the HTTP answer below |
| Any proxied request to a session that is asleep: MCP on the sessions host and on the API host, the upload URL, the VNC websocket | `proxy.Waker.EnsureAwake`, before `Store.Wake` | Wake | MCP: below. VNC and upload: the HTTP answer. |
| A proxied request to a session that is running | nothing | none (the sweep stops it, not the request) | |
| Warm-pool adoption and claim recovery | `Store.createWarm`, `RecoverClaims` | none of their own: they run after Create allowed | |
| Session policy routes, token routes, list, get, delete, rename, stop | | never refused for billing | |

Reading, stopping and deleting always work, at any balance, so that a user
who is out of hours can still see their sessions and remove them.

## Answers

HTTP, on the app and the API host:

| Code | Status | Message (the `error` field; the UI adds the buttons) |
|---|---|---|
| `out_of_hours` | 402 | You have used all your hours. Buy more hours or change plan to continue. |
| `session_limit` | 409 | Your plan allows N sessions. Delete one, or change plan. |
| `awake_limit` | 409 | Your plan runs N sessions at once. Stop one, or change plan. |
| `at_capacity` | 503, `Retry-After: 120` | Every desktop is in use right now. Try again in a few minutes. |
| `rate_limited` | 429, `Retry-After` | Too many starts in the last hour. Try again later. |
| `metering_unavailable` | 503, `Retry-After: 120` | Usage metering is unavailable right now, so free sessions cannot start. Try again in a few minutes. |
| `account_blocked` | 403 | This account is suspended. Contact support. |
| `terms_required` | 403 | Accept the terms to continue. |

Body: `{"error": "<message>", "code": "<code>", "billingUrl": "<PUBLIC_URL>/billing"}`
(`Error` of `backend-api.yaml`). `session_limit` keeps the 409 the API
gives today for the same thing.

MCP (the request is JSON-RPC and its client is a program): the same HTTP
status, `Content-Type: application/json`, and a JSON-RPC error whose message
is one a model can relay to its user:

```json
{"jsonrpc":"2.0","id":null,"error":{"code":-32002,"message":"This session cannot wake: its owner has used all their hours. Add hours at https://app.computeruse.site/billing"}}
```

What Claude's clients show for a 402 or 503 on an MCP call is **not
verified**; track C records it for each code, and if a client hides the
body, the status becomes 200 with the JSON-RPC error (the request's `id`
then has to be read from the body).

## Stopping what is running

A sweep in the backend, every 30 s, beside the idle sweep and using the
same `Store.Sleep` (snapshot first, so the session wakes as it was):

- For every account with `level: exhausted` and `now >= exhaustedAt +
  BILLING_GRACE` (5 m), not exempt: put each `running` session to sleep with
  `browserjs.dev/stopped-by: billing`.
- For every blocked account: the same, at once.
- A session stopped by billing is shown as `asleep` with the message "out of
  hours"; `Wake` and `Resume` go through the table (row 7 refuses until
  there is a balance), after which it wakes like an idle sleep. `Store.Wake`
  must accept `stopped-by: billing` as it accepts `idle`.
- A sleep that fails is retried at the next sweep. The snapshot failing does
  not stop the sleep (as today).
- With `stale` true the sweep stops nothing.

The grace is counted from the ledger's `exhaustedAt`, so a user who buys
hours within it is never interrupted.

## Free and pay-as-you-go storage

A daily pass in the backend deletes the sessions of accounts whose tier has
`idleDeleteDays`, when the session has been asleep or stopped that long
(`browserjs.dev/last-awake`, an annotation the backend writes when a session
goes to sleep). The session view carries `deleteAfter` from 7 days before,
and the UI shows it. Subscribers' sessions are never deleted this way. This
pass runs only in `enforce`, and is separately switchable
(`IDLE_DELETE=off`), default off until the product owner turns it on.

## Rate of new accounts (stage 4)

With `OPEN_SIGNUP` on, the backend makes an Account for any signed-in user,
but not more than `SIGNUPS_PER_DAY` (50) and `SIGNUPS_PER_IP_PER_DAY` (3)
free accounts; beyond that a new user sees "Sign-ups are paused for today"
and no Account is made. Counted from the Accounts' creation timestamps and
an annotation `browserjs.dev/signup-ip-hash` (a salted hash, deleted after
35 days).
