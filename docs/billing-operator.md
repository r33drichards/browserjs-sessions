# The billing operator (the observer)

`images/billing-operator/` is track A of the metering and billing plan, as
[the Metronome integration](plans/2026-10-02-metronome-integration.md)
changed it: a [kopf](https://kopf.readthedocs.io/) process that, once a
minute, looks at every session's Sandbox, counts the seconds each was awake
and the GB-seconds its disk was kept, and sends them to Metronome as usage
events. Contracts: [`metronome.md`](contracts/billing/metronome.md) ("Usage
events"), the seconds of [`metering.md`](contracts/billing/metering.md), the
operator's rows of [`deploy.md`](contracts/billing/deploy.md).

It does no arithmetic in money, reads no Account, and writes nothing in the
cluster but one Lease. Metronome is the meter and the ledger.

**It is off by default.** With `BILLING` unset (or `off`) the process
starts, says so, and reads, writes and sends nothing. `meter` and `enforce`
are the same to it.

## What is in the directory

| Path | What |
|---|---|
| `billing_operator/meter.py` | `seconds(sessions, observed, now)`: the gap rule of `metering.md`, a pure function |
| `billing_operator/observe.py` | what is seen of a Sandbox: whose it is, awake or not, since when |
| `billing_operator/events.py` | the two events and their `transaction_id`s; the hour's sum of each session's GB-seconds |
| `billing_operator/sender.py` | delivery: the sink interface (`async ingest(events)`), `Metronome` (`POST /v1/ingest`), `FakeMetronome` (in memory), and the `Sender` with its batches, retry and give-up |
| `billing_operator/passes.py` | the pass and the loop |
| `billing_operator/kube.py` | the calls to the API server: list Sandboxes; get, create, update the Lease |
| `billing_operator/memory.py` | the same calls answered from memory, for the command line and the tests |
| `billing_operator/catalogue.py` | the catalogue file, for `sessionDiskGB` only; re-read when it changes |
| `billing_operator/handlers.py` | the kopf handlers: start, stop, the switch, the watchdog, the liveness probe |
| `billing_operator/config.py`, `cli.py` | the environment; `python -m billing_operator` |
| `examples/` | three Sandboxes for the command below |
| `tests/`, `flake.nix`, `requirements.*`, `Dockerfile*` | as in `images/policy-operator` |

## The pass

Every `TICK` (60 s), never two at once:

1. The time is read once, truncated to the second: `now`.
2. Every Sandbox is listed, once. Those with the annotation
   `browserjs.dev/owner-id` and no `deletionTimestamp` are sessions; a
   warm-pool Sandbox has no owner and is never counted.
3. `seconds` counts, for each session, against the observer's memory of the
   last tick (`lastSeen`, `awake`).
4. For each session that counted awake seconds, one `session.awake` event.
   Each session's GB-seconds are added to its hour's sum; the sum is sent
   as one `session.kept` event at the first tick at or after the hour's
   end, or at the tick where the session is no longer there.
5. Everything waiting is delivered. If nothing is left waiting, the Lease
   `billing-observer` is renewed (`renewTime` = `now`).

The customer of an event is `acct-<the session's browserjs.dev/owner
label>`: the Account's name, which the backend gives Metronome as the
customer's ingest alias. The observer does not look the Account up.

One line per pass:

```
pass now=2026-10-02T10:01:00Z sessions=2 awake_seconds=60 disk_gb_seconds=600 events=1 sent=1 pending=0 dropped=0 lease=renewed duration_ms=3
```

### What is seen

- **awake**: not `Suspended`, and `Ready` is `True`: the rule of
  `sessions.FromSandbox(...).State == Running` in the backend.
- **readySince**: `Ready`'s `lastTransitionTime`, or the time the Sandbox
  was taken from the warm pool (`browserjs.dev/created`) if that is later.
- **diskGB**: the catalogue's `sessionDiskGB`.

### The events

| | `session.awake` | `session.kept` |
|---|---|---|
| `transaction_id` | `awake/<session>/<tick, unix seconds>` | `kept/<session>/<hour start, unix seconds>` |
| `timestamp` | the tick | the last tick that added to the sum (the hour's end, for a whole hour) |
| `properties` | `session_id`, `seconds` | `session_id`, `gb_seconds` |

An hour is `(start, start + 3600]`: the tick at 11:00:00 completes the hour
that began at 10:00:00, so a session kept for the whole hour sends
`gb_seconds: 18000` at 11:00:00.

The key depends only on the session and the tick (or the hour), so the
same tick looked at twice, by this process or by one that replaced it,
makes the same key, and Metronome ignores the repeat.

### Delivery

- At most 100 events a request, oldest first.
- A 429, a 5xx or no answer: the events stay in memory and are tried again,
  the same events with the same keys, after a backoff (one tick, doubling,
  at most five minutes). Later ticks' events queue behind them.
- An event that has waited more than an hour is dropped, with its key in
  the log.
- Any other 4xx: the batch is dropped, with its keys in the log.
- The Lease is not renewed while anything is waiting.
- A restart forgets what was waiting, each session's last sight and the
  hour's GB-seconds.

Every one of these losses is time the user is not charged for. Nothing
charges time that was not observed.

## Where the seconds are stricter than the reference

`spike/meter_ref.py` and `metering.md` count, for a session that is awake
but was not "seen awake at both sights", the time since it became Ready if
that is at most `MAX_GAP`. The observer also caps that at the time since
the session was last seen, when it was seen before:

```
a = now - readySince  if 0 <= now - readySince <= MAX_GAP else 0
a = min(a, max(now - lastSeen, 0))        # the observer's addition
```

Without it a tick repeated with the same `now` counts the time since Ready
a second time, and a session seen asleep 60 s ago can be counted 100 s
awake. All 18 vectors give the same seconds with and without it. A test
runs generated histories through both: they agree tick for tick except
where this cap bites, and there the observer counts less. The backend's
fake Metronome ports the reference; it should take the same line.

## Configuration

| Variable | Default | |
|---|---|---|
| `BILLING` | `off` | `off`: idle. `meter`, `enforce`: observe and send. Anything else: refuses to start |
| `TICK` | `60s` | between passes |
| `MAX_GAP` | `150s` | the longest gap between two sights that is counted; must be longer than `TICK` |
| `BILLING_CATALOGUE` | `/etc/browserjs/catalogue.yaml` | for `sessionDiskGB`. With `BILLING` on, the observer refuses to start without a catalogue that parses; later, a bad file keeps the last good one |
| `METRONOME_API_TOKEN` | | from the Secret `metronome`. Required when `BILLING` is not `off`. Sent as the bearer token of ingest requests; never logged, never in an error |
| `METRONOME_URL` | `https://api.metronome.com` | |
| `BILLING_NAMESPACE` | `browserjs-sessions` | must be the namespace `kopf run --namespace` names |

## Liveness

`GET :8081/healthz` (kopf's endpoint) answers while the process is alive:

```json
{"meter": {"metering": true, "passes": 1234, "lastPass": "2026-10-02T10:01:00Z", "pending": 0, "leaseRenewed": true, "secondsSinceLoop": 12}}
```

Every call has a 30 s limit. If the loop stops coming round (for five
ticks, and at least five minutes), or its task ends, the process stops
itself and the Deployment starts another. Whether usage is reaching
Metronome is the Lease's age, which the backend reads.

## Running it without a cluster or Metronome

```
cd images/billing-operator
nix develop -c python -m billing_operator simulate examples \
    2026-10-02T10:00:00Z 2026-10-02T10:01:00Z 2026-10-02T11:00:00Z
```

`simulate <dir> <time> [<time> ...]` reads every Sandbox from the
directory's YAML files (multi-document files and `kubectl get sandboxes -o
yaml` lists both work), runs the observer's own pass at each time with the
fake Metronome as its sink, and prints after each tick the pass's line, the
sessions remembered and the events that tick sent. With `examples/` (one
session awake since 09:59:40, one asleep, one warm-pool Sandbox), shortened:

```yaml
---
tick: '2026-10-02T10:00:00Z'
events:
- transaction_id: awake/s-aaaaa/1790935200
  customer_id: acct-7615aafcb45bcc853c4ed32cc5539842
  event_type: session.awake
  timestamp: '2026-10-02T10:00:00Z'
  properties: {session_id: s-aaaaa, seconds: '20'}     # since Ready
---
tick: '2026-10-02T10:01:00Z'
events:
- transaction_id: awake/s-aaaaa/1790935260
  properties: {session_id: s-aaaaa, seconds: '60'}
---
tick: '2026-10-02T11:00:00Z'       # 59 minutes later: beyond the gap, nothing more is counted
events:
- transaction_id: kept/s-aaaaa/1790935200
  properties: {session_id: s-aaaaa, gb_seconds: '300'}   # the hour's disk, as far as it was seen
- transaction_id: kept/s-bbbbb/1790935200
  properties: {session_id: s-bbbbb, gb_seconds: '300'}
```

The contract's vectors, through the seconds function:

```
nix develop -c python -m billing_operator vectors ../../docs/contracts/billing/metering-vectors.json
18/18 vectors pass (awakeSeconds, diskGBSeconds)
```

## Tests

`cd images/billing-operator && nix develop -c pytest`. No cluster, no
Docker, no network, no token.

| File | What |
|---|---|
| `test_vectors.py` | every vector's `awakeSeconds` and `diskGBSeconds` |
| `test_step_properties.py` | generated histories: awake seconds never exceed `now - lastSeen`; nothing negative; a repeated tick counts nothing; a gap over `MAX_GAP` counts at most `MAX_GAP`; an hour awake in any pattern of ticks is 3600 seconds; and the seconds against `spike/meter_ref.py` |
| `test_sender.py` | the events' shape; the hour's disk; batches of at most 100; a 429, 5xx or no answer retried with the same keys and backoff; given up after an hour; a 4xx dropped and logged; the real client against an ingest endpoint over HTTP; the token in no error |
| `test_pass.py` | the pass: one event per awake session per tick, one per kept session per hour; a warm-pool Sandbox sends nothing; the same tick twice has the same `transaction_id`s; a restart counts nothing for the time it was down beyond `MAX_GAP`; Metronome away (the Lease waits; after an hour that usage is free) |
| `test_kube.py` | the real client over HTTP: the paged list, the Lease made then renewed, a conflict, the ServiceAccount token read again |
| `test_units.py`, `test_cli.py` | the catalogue, the configuration, the observation of a Sandbox; the command above |
| `test_handlers.py` | the switch, start and stop, the watchdog, and the whole observer under `kopf run` as `deploy.md` starts it, against a fake API server and a fake ingest endpoint |

## The image

`docker build -f images/billing-operator/Dockerfile .` from the repository
root, as for the policy operator (the directory itself also works as the
context). `python:3.12-slim`, the locked requirements, the package; the
build runs `python -m billing_operator selfcheck`. It runs as uid 65532
with `USER` set in the environment (kopf asks `getpass.getuser()`), and
listens only on 8081.

## Not verified here

No cluster and no Metronome were used, and no token was handled. Still to
be seen: the image building and starting; the Role of `deploy.md` being
enough (Sandboxes, the Lease, kopf's start); egress to Metronome; the real
Sandbox controller's `Ready` and `operatingMode`; and of Metronome, what
`metronome.md` marks as sandbox checks: that ingest takes these events and
the Account's name as `customer_id`, what it answers for an alias it does
not know (M7), and its rate limits (M8).
