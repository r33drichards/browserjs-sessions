# Metering contract

What "awake time" is, how it is measured, and how it is taken from grants.
The billing operator implements this; `spike/meter_ref.py` is a reference
implementation and `metering-vectors.json` the cases both must agree on.

```
python3 docs/contracts/billing/spike/meter_ref.py docs/contracts/billing/metering-vectors.json
17/17 vectors pass
```

## What is billable

A session is **billable at an instant** when all of these hold for its
Sandbox (`agents.x-k8s.io/v1beta1`, namespace `browserjs-sessions`):

1. it has the annotation `browserjs.dev/owner-id` (it is somebody's session;
   a warm-pool Sandbox before adoption has none),
2. `metadata.deletionTimestamp` is unset,
3. `spec.operatingMode` is `Running`,
4. its `Ready` condition is `True`.

That is exactly `sessions.FromSandbox(...).State == Running` in the backend,
and the two must stay the same rule.

| Time | Billable | Why |
|---|---|---|
| Warm pod waiting in the pool | no | Rule 1. The pool is the product's cost. |
| Starting: waiting for a node, pulling, restoring a snapshot | no | Rule 4. The user cannot use it yet, and how long it takes is ours to fix. |
| Running and in use | yes | |
| Running and idle (the 15 minutes before it sleeps) | yes | It holds its place on a node. The user can stop it to stop the clock; the UI says so. |
| The snapshot taken on the way to sleep (up to 2 minutes) | yes, mostly | The pod is still Running while it is taken. In practice the tail rule below makes the last part free. |
| Suspended, not yet down (`stopping`) | no | Rule 3. |
| Asleep, stopped | no | Rule 3. Storage is not metered in v1. |
| Failed | no | Rule 4. |

`readySince` of a billable session is the `lastTransitionTime` of its `Ready`
condition.

## The tick

The operator is one replica. Every `TICK` (60 s) it makes one pass:

1. Read the time once: `now`, UTC, truncated to the second, from the
   operator's own clock (the node's, kept by GKE's NTP).
2. List every Sandbox in the namespace once. Group by the label
   `browserjs.dev/owner`. For each, note `billable` and `readySince`.
3. For every Account that has a billable session now, or has entries in
   `status.meter.sessions`, or whose `level`, grants or period changed:
   run **the step** below and write `status` in one update with the
   `resourceVersion` it was read at. On a conflict, read again and redo the
   step with the same `now`; if it conflicts three times, skip the account
   until the next tick (its gap stays within `MAX_GAP` for one missed tick).
4. An owner label with no Account: create nothing (the backend makes
   Accounts), log it, and carry on. Such sessions are unmetered until the
   Account exists; the first tick after bills from then, not from before.

A pass that takes longer than `TICK` is not overlapped by the next; the next
starts when it ends.

## The step

Inputs: the account's `status.meter` as stored, its Grants, the observation
`{session ID: (billable, readySince)}`, and `now`. Constants: `MAX_GAP` =
150 s.

**Credit.** For each session billable now:

- If it is in `meter.sessions` with `lastSeen` = p and `0 < now - p <= MAX_GAP`:
  credit `now - p`.
- Otherwise (first sight of this run; or the meter was away longer than
  `MAX_GAP`; or the clock went backwards): credit `now - readySince` if
  `0 <= now - readySince <= MAX_GAP`, else 0.
- Set its `lastSeen` to `now`.

Every session in `meter.sessions` that is not billable now is removed, with
no credit for the time since its `lastSeen`.

**Debit.** `owed` = the sum of the credits. A grant is **live at `now`**
when `validFrom <= now`, `now < expiresAt` (or it has no expiry) and it is
not revoked. Take `owed` from the live grants in this order until it is
zero: earliest `expiresAt` first (no expiry last), then earliest `validFrom`,
then name. What is taken from a grant is added to `meter.consumed[grant]`
and never exceeds the grant's `seconds`. Anything left over is added to
`overdraftSeconds`: used, counted, owed by nobody.

**Totals.**

- `balanceSeconds` = sum over live grants of `seconds - consumed`.
- `purchasedSeconds` = the same over live grants of source `purchase` or `admin`.
- `period.allowanceSeconds` = sum of `seconds` over live grants of source
  `free` or `plan`.
- `level` = `exhausted` if the balance is 0; else `low` if the balance is at
  most `max(0.2 x allowance, 1800)`; else `ok`.
- `exhaustedAt` = set to `now` when the balance is 0 and it was unset;
  removed when the balance is above 0.
- `period.usedSeconds`, `period.days[date of now]`, `period.sessions[id]`
  and `meter.usedSeconds` each grow by the credits.
- `meter.observedAt` = `now`.

The reference keeps `sessions` and `consumed` as maps; the CRD stores them
as keyed lists. They are the same data.

## Which way errors fall

Every approximation is in the user's favour. None bills time that was not
observed.

| Situation | Effect |
|---|---|
| The up to 60 s between the last billable sight and the sleep | free |
| A run that starts and ends between two ticks | free (wakes are rate limited instead, `enforcement.md`) |
| Operator down, restarting, or upgraded for longer than 150 s | the whole gap is free, for everyone |
| Operator down for less than 150 s | billed exactly, by the next tick |
| The cluster's API refuses the status write | nothing was recorded, so the next tick's gap decides: within 150 s billed, beyond it free |
| Clock stepped backwards | that interval is free |
| The Sandbox controller's clock ahead of the operator's (`readySince` in the future) | the first partial interval is free |
| The interval that straddles a period's end | billed to the new period's grant |
| Used with no grant left (the grace, a slow stop) | overdraft: counted, not charged |
| The cluster is lost and the Accounts are restored from an export | usage since the export is forgotten; purchases are re-made from Stripe (`stripe.md`, "Reconcile") |

The one direction in which a user could be billed for time they did not
have is a Sandbox that reports `Ready` while its pod cannot be reached.
That is the Sandbox controller's truth, the same one the UI shows as
"running", and is accepted.

## Seconds, minutes, hours

The ledger is whole seconds, signed 64 bit. Nothing is rounded in it.
Enforcement compares seconds. For display: hours **used** are rounded down
to the minute, hours **left** are rounded up to the minute. A plan's "40
hours" is 144000 seconds.

## Periods

- **A subscriber** (`spec.subscription.status` is `active`, `trialing` or
  `past_due`): the period is Stripe's, `currentPeriodStart` to
  `currentPeriodEnd` as the backend wrote them. The plan's hours are a Grant
  the **backend** makes for each paid period (`stripe.md`).
- **Anyone else**: the calendar month in UTC. The **operator** makes the
  free Grant, lazily: the first tick of a month on which the account exists,
  is not a subscriber, is not blocked or deleted, and `FREE_TIER` is on. Key
  `free/<ownerHash>/<yyyy-mm>`, `seconds` from `catalogue.yaml`, valid from
  the first to the first.
- A subscriber gets no free Grant. Someone whose subscription ends
  mid-month gets that month's free Grant at the next tick.

**Closing.** When `now >= period.end`, before the step: the operator writes
the `UsagePeriod` (name `up-<ownerHash>-<period start as yyyymmddhhmm>`,
create only; "already exists" is success), then starts `status.period`
afresh. `meter.consumed` entries of grants that expired more than 35 days
ago are dropped. `UsagePeriod`s older than 13 months and Grants that
expired or were revoked more than 13 months ago are deleted by a daily pass.

**The plan a user is on** (`status.plan`), from `catalogue.yaml`: the
subscription's plan while its status is `active`, `trialing` or `past_due`;
otherwise `payg` if any live `purchase` grant has seconds left; otherwise
`free`.

## Test vectors

`metering-vectors.json`: a list of `{name, why, grants, state?, ticks, expect}`.
`state` is `status.meter` before the first tick. Each tick is `{now,
observed}`; each `expect` entry is what the step must report after that
tick: `credited` (by session), `balanceSeconds`, `allowanceSeconds`,
`level`, `overdraftSeconds`, `exhaustedAt`. The operator's tests run every
vector; so does the backend's view code where it derives anything from the
same numbers (it should not need to).

Worked example ("low at 20 percent...", in the file): a free account with a
3 hour grant has used 10680 s, so 120 s are left. Its one session was last
seen at 10:00:00.

| Tick | Gap | Credit | Balance | Level |
|---|---|---|---|---|
| 10:00:30 | 30 s | 30 | 90 | low |
| 10:01:30 | 60 s | 60 | 30 | low |
| 10:02:30 | 60 s | 60 (30 from the grant, 30 overdraft) | 0 | exhausted, `exhaustedAt` 10:02:30 |

The backend puts the session to sleep at 10:07:30 (`BILLING_GRACE` 5 m)
unless hours were bought first. Until then every tick adds 60 to
`overdraftSeconds` and nothing to anyone's bill.
