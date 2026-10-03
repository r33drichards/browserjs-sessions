# Metering contract

> **Changed 2026-10-02 by [`metronome.md`](metronome.md).** The meter and
> the ledger are Metronome's. Of this file:
>
> - **Still the contract**: "One balance, two charges" (the rates), "What
>   is observed", the **Seconds** part of "The step" (the gap rule,
>   `MAX_GAP`, `readySince`), "Which way errors fall", "Display". The
>   observer computes seconds exactly so, and the vectors' `awakeSeconds`
>   and `diskGBSeconds` are what its tests check.
> - **Replaced by `metronome.md`**: "The tick" from step 3 on (nothing is
>   written to an Account's status; events are sent to Metronome), the
>   **Money**, **Debit** and **Totals** parts of the step (Metronome rates
>   the usage and draws the credit down; the order is its priorities),
>   "Periods" (the calendar month, in Metronome; no `UsagePeriod`).
> - The vectors' money fields (`awakeMicros`, `diskMicros`,
>   `balanceMicros`, `level`, `overdraftMicros`, `exhaustedAt`) remain what
>   the **fake** Metronome of `testing.md` must answer, so that scenario
>   tests have a ledger that behaves; they are no longer the operator's.
> - Read "the operator" as "the observer" throughout.

What is charged, how it is measured, and how it is taken from an account's
credit. The billing operator implements this; `spike/meter_ref.py` is a
reference implementation and `metering-vectors.json` the cases both must
agree on.

```
python3 docs/contracts/billing/spike/meter_ref.py docs/contracts/billing/metering-vectors.json
18/18 vectors pass
```

## One balance, two charges

An account has one balance of credit in US dollars, held as whole
**micro-dollars** (1 USD = 1000000) in Grants. Two things use it up, at the
rates of `catalogue.yaml`:

| Charge | Rate (proposed) | Counted |
|---|---|---|
| **Awake time** | `awakeMicrosPerHour` = 200000 ($0.20 an hour) for a small session; `sizes.medium.awakeMicrosPerHour` = 600000 ($0.60) and `sizes.large.awakeMicrosPerHour` = 1600000 ($1.60) for the bigger sizes (`metronome.md`, "Sizes") | for each session, while it is awake, at the rate of the size it runs at |
| **Disk** | `diskMicrosPerGBHour` = 384 ($0.28 per GB-month; $1.40 a month for a 5 GB session) | for each session, from creation to deletion, awake or asleep |

The operator is the only thing that decrements the balance. The backend
never does.

## What is observed

For each Sandbox (`agents.x-k8s.io/v1beta1`, namespace `browserjs-sessions`)
that has the annotation `browserjs.dev/owner-id` (a warm-pool Sandbox before
adoption has none) and no `metadata.deletionTimestamp`:

- **exists**: it is in the observation, with `diskGB` (`sessionDiskGB` of
  the catalogue: every session's disk is 5 GB today).
- **awake**: `spec.operatingMode` is `Running` and its `Ready` condition is
  `True`. That is exactly `sessions.FromSandbox(...).State == Running` in
  the backend, and the two must stay the same rule.
- **readySince**: the `lastTransitionTime` of `Ready`, when awake.

| Time | Awake charge | Disk charge |
|---|---|---|
| Warm pod waiting in the pool | no | no |
| Starting: waiting for a node, pulling, restoring a snapshot | no | yes |
| Running and in use | yes | yes |
| Running and idle (the 15 minutes before it sleeps) | yes | yes |
| A call finishing after the credit ran out, the grace, the snapshot | counted, but there is no credit: overdraft, owed by nobody | the same |
| Suspended, not yet down (`stopping`) | no | yes |
| Asleep, stopped, failed | no | yes |
| Being deleted, deleted | no | no |

## The tick

The operator is one replica. Every `TICK` (60 s) it makes one pass:

1. Read the time once: `now`, UTC, truncated to the second, from the
   operator's own clock (the node's, kept by GKE's NTP).
2. List every Sandbox in the namespace once. Group by the label
   `browserjs.dev/owner`.
3. For every Account that has a session, or has entries in
   `status.meter.sessions`, or whose grants or period changed: run **the
   step** below and write `status` in one update with the `resourceVersion`
   it was read at. On a conflict, read again and redo the step with the
   same `now`; if it conflicts three times, skip the account until the next
   tick (its gap stays within `MAX_GAP` for one missed tick).
4. An owner label with no Account: create nothing (the backend makes
   Accounts), log it, carry on.

A pass that takes longer than `TICK` is not overlapped by the next.

## The step

Inputs: the account's `status.meter` as stored, its Grants, the observation
`{session ID: (awake, readySince, diskGB)}`, `now`, and the two rates.
Constant: `MAX_GAP` = 150 s.

**Seconds.** For each observed session, with `prev` its entry in
`meter.sessions` and `gap = now - prev.lastSeen`; the pair was **seen** when
`prev` exists and `0 < gap <= MAX_GAP`:

- Disk: if seen, `gap x diskGB` GB-seconds. Otherwise nothing.
- Awake, only if awake now: if seen and `prev.awake`, `gap` seconds.
  Otherwise (first sight of this run; or not seen): `now - readySince` if
  `0 <= now - readySince <= MAX_GAP`, else 0.
- Its entry becomes `{lastSeen: now, awake: awake}`.

An entry whose session is not in the observation is removed, with nothing
charged for the time since its `lastSeen`.

**Money.** With `carry` the two remainders kept in `meter.carry`:

```
awakeMicros, carry.awake = divmod(awake seconds  x awakeMicrosPerHour  + carry.awake, 3600)
diskMicros,  carry.disk  = divmod(GB-seconds     x diskMicrosPerGBHour + carry.disk,  3600)
owed = awakeMicros + diskMicros
```

Integer arithmetic throughout. The carry makes an hour of awake time cost
exactly 200000, however the ticks fall.

**Debit.** A grant is **live at `now`** when `validFrom <= now`, `now <
expiresAt` (or it has no expiry) and it is not revoked. Take `owed` from the
live grants in this order until it is zero: earliest `expiresAt` first (no
expiry last), then earliest `validFrom`, then name. So a plan's credit
(which ends with the period) goes before the sign-up credit (90 days), and
purchased credit (12 months) last. What is taken from a grant is added to
`meter.consumed[grant]` and never exceeds its `amountMicros`. Anything left
over is added to `overdraftMicros`: used, counted, **owed by nobody**. The
balance never goes below zero and there is no debt.

**Totals.**

- `balanceMicros` = sum over live grants of `amountMicros - consumed`;
  `balances` the same by source, with the earliest expiry of each.
- `level` = `exhausted` if the balance is 0; else `low` if the balance is
  at most `max(0.2 x the live plan grants' amount, 1000000)`; else `ok`.
- `exhaustedAt` = set to `now` when the balance is 0 and it was unset;
  removed when the balance is above 0. It keeps its value while the balance
  stays at zero: it is the start of "N days at zero".
- `burnMicrosPerHour` = awake sessions now x the awake rate + the sum of
  `diskGB` x the disk rate.
- `period.*`, `meter.chargedMicros` grow by the tick's amounts;
  `meter.observedAt` = `now`.

A change of rate in the catalogue applies from the tick after the operator
reads it; nothing is recomputed.

The reference keeps `sessions` and `consumed` as maps; the CRD stores them
as keyed lists. They are the same data.

## Which way errors fall

Every approximation is in the user's favour. None charges for time that was
not observed.

| Situation | Effect |
|---|---|
| The up to 60 s between the last awake sight and the sleep | awake time free |
| A run that starts and ends between two ticks | awake time free (wakes are rate limited instead, `enforcement.md`) |
| The first minute of a new session's disk; the last of a deleted one | free |
| Operator down, restarting, or upgraded for longer than 150 s | the whole gap is free, awake time and disk, for everyone |
| Operator down for less than 150 s | charged exactly, by the next tick |
| The cluster's API refuses the status write | nothing was recorded; the next tick's gap decides |
| Clock stepped backwards; `readySince` in the future | that interval is free |
| An interval that straddles a grant's expiry or a period's end | charged to the grants live at the tick |
| Used with no credit left | overdraft: counted, not owed |
| The cluster is lost and the Accounts are restored from an export | charges since the export are forgotten; everything paid for is re-made from Stripe (`stripe.md`, "Reconcile") |

## Display

The ledger is micro-dollars; nothing is rounded in it. Enforcement compares
micro-dollars. For display, credit **left** is rounded down to the cent and
amounts **charged** are rounded down to the cent; "about N hours left" is
`balanceMicros / burnMicrosPerHour`, rounded down to the minute, and is
called an estimate.

## Periods

A period is only for showing usage and for the plan's credit; charging does
not depend on it.

- **A subscriber** (`spec.subscription.status` is `active`, `trialing` or
  `past_due`): Stripe's billing period, `currentPeriodStart` to
  `currentPeriodEnd` as the backend wrote them. The plan's credit is a Grant
  the **backend** makes for each paid period (`stripe.md`); it expires at
  the period's end and does not roll over.
- **Anyone else**: the calendar month in UTC.

**Closing.** When `now >= period.end`, before the step: the operator writes
the `UsagePeriod` (name `up-<ownerHash>-<period start as yyyymmddhhmm>`,
create only; "already exists" is success), then starts `status.period`
afresh. `meter.consumed` entries of grants that expired more than 35 days
ago are dropped. A daily pass deletes `UsagePeriod`s older than 13 months,
and Grants that expired or were revoked more than 13 months ago, **except
Grants of source `signup`, which are never deleted**: their existence is
what stops a card earning the credit twice.

**The plan a user is on** (`status.plan`): the subscription's plan while
its status is `active`, `trialing` or `past_due`; otherwise `payg`.

## Test vectors

`metering-vectors.json`: a list of `{name, why, grants, state?, ticks, expect}`.
`state` is `status.meter` before the first tick. Each tick is `{now,
observed}`; each `expect` entry is what the step must report after that
tick: `awakeSeconds` and `diskGBSeconds` (by session), `awakeMicros`,
`diskMicros`, `balanceMicros`, `level`, `overdraftMicros`, `exhaustedAt`.
The operator's tests run every vector.

Worked example ("low, then exhausted", in the file): $0.005 of the sign-up
credit is left; one session is awake, last seen at 10:00:00. A minute awake
is 60 x 200000 / 3600 = 3333 (remainder carried), its disk 60 x 5 x 384 /
3600 = 32.

| Tick | Awake | Disk | Balance | Level | Overdraft |
|---|---|---|---|---|---|
| 10:01:00 | 3333 | 32 | 1635 | low | 0 |
| 10:02:00 | 3333 | 32 | 0 | exhausted, `exhaustedAt` 10:02:00 | 1730 |
| 10:03:00 | 3334 | 32 | 0 | exhausted | 5096 |

From 10:02:00 the backend's stop sequence runs (`enforcement.md`). What
accrues until the session is asleep is overdraft and on nobody's bill.
