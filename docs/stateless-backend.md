# The backend holds no state

The backend (`backend/`) can run as more than one replica, and as more than
one version at a time. Nothing it decides from is in a process: every durable
fact is an object in the cluster, and what the replicas need to tell each
other is written on the session it concerns. There is no database.

This note says where each fact lives, how the races between replicas are
closed, what it costs the API server, and what has not been run on a real
cluster. **Everything here is tested with fakes only** (two replicas over one
fake API server); see [What is not verified](#what-is-not-verified).

## Where each fact lives

| Fact | Where | Written by |
|---|---|---|
| A session, its owner, name, state, why it stopped, its snapshot | the Sandbox (`spec.operatingMode`, annotations `owner-id`, `name`, `stopped-by`, `snapshot`) | `sessions.Store`, as before |
| A session's policy, API tokens, accounts | `SessionPolicy`, `APIToken`, `Account` | as before |
| When a session was last used | annotation `browserjs.dev/last-active` (RFC 3339) on the Sandbox | the replica that proxied the use; the store at create, resume and wake |
| That a replica has a call in flight to a session | annotation `browserjs.dev/in-flight.<replica>`: until when that replica vouches for it | that replica |
| That a session is being drained for billing | annotations `draining`, `draining-since` | the replica that sweeps, as before |
| Who runs the periodic passes | Lease `backend-leader` | client-go's leader election |
| That a VNC ticket is good | the ticket itself: signed with a key derived from `API_SIGNING_KEY` (Secret `api-tokens`) | the replica that issued it |

### What was in the process, and where it went

| Was | Now |
|---|---|
| `idle.Tracker.last`: last activity per session | `last-active` on the session |
| `idle.Tracker.open`: open viewers and calls per session | nothing shared. The replica that holds a connection rewrites `last-active` every 30 s while it does (a heartbeat) |
| `proxy.flights.calls`: calls in flight, for billing's drain | each replica counts its own, and says so on the session (`in-flight.<replica>`) before it forwards a call |
| `proxy.flights.streams`: cancel functions of open streams | still per replica (they are its connections). A replica closes its streams of a draining session when it next sees the mark: in the answer to its own heartbeat, or by looking once per 30 s at sessions it only has a stream to |
| `proxy.tickets`: one-time VNC tickets | signed tickets, 10 s, redeemable at any replica |
| The idle sweep, billing's sweep, deletion pass and balance pass, Stripe's reconciles: run by "the" backend | run by the replica that holds the Lease |
| `api.creating`: a lock per user around "count, then create" | still there, for one replica's own creates; a create through another replica is caught by counting again afterwards (`lostRace`) |

### What is still in a process, and why that is fine

Each of these is rebuilt from the cluster or lost at no cost.

| What | Kind | With N replicas |
|---|---|---|
| `idle.Tracker`'s entries: this replica's own open connections, and when it last wrote | bookkeeping of its own sockets | lost with the sockets |
| `proxy.Waker.running`, `authz.Owners`: a session seen running (2 s), its owner (2 s) | cache | a replica may proxy to a pod for up to 2 s after another put the session to sleep; the request fails and the next one wakes it, as after a user's stop today |
| `Waker`'s singleflight groups | de-duplication | each replica wakes on its own; `Store.Wake` is a conditional write, so one resumes and the others find it awake |
| `proxy.viewers` | for a clean shutdown | per replica |
| Redeemed-ticket memory | replay guard | a ticket can be redeemed once per replica within its 10 s (see below) |
| `tokens.Store.touched`, Stripe prices and portal ID, Metronome IDs, the catalogue file | caches of what is read elsewhere | each replica reads its own |
| `billing.Enforcer.starts` (`WAKES_PER_HOUR`), `auth.FailureLimiter`, Stripe webhook's failure limiter | rate limiters | **weaker**: each replica counts what it saw, so the worst case is N times the limit. They already reset at every restart |
| `tokens.Handlers.creating` | lock around "count, then create" for API tokens | **weaker**: two creates by one user through two replicas at the same moment can pass the token cap by one |
| `stripe.Service.locks` | orders two reads of Stripe for one account | **weaker**: two webhooks for one account at two replicas can write in either order; the 15-minute reconcile puts it right |
| `policy.Gated.inForce`: sessions whose first policy was seen in force | cache of a fact on the SessionPolicy that never reverts | each replica reads it once per session |
| `policy.Watch`: follows a new session's policy into force | goroutine of the replica that created the session | if that replica dies in those seconds nobody deletes a session whose policy was refused; true of one replica too |

## Idleness

`internal/idle`. A replica that proxies a call, or holds a viewer open, writes
`last-active` on the session:

- at most once per 30 s per session per replica (four times per idle period
  if `IDLE_AFTER` is under two minutes);
- before the call is forwarded, if nothing was written in the last 30 s, so
  the write is on the session before the pod is asked;
- again every 30 s for as long as it holds a call or a viewer;
- and, for a use that ended between two writes, once more with the moment it
  ended, written only if nothing later is there.

The sweep lists the sessions and puts to sleep each running one whose
`last-active` is `IDLE_AFTER` + 5 s old. It reads nothing from the replica it
runs on. A running session with no annotation (started by an older backend)
gets one at the first sweep that sees it.

With one replica the idle period is what it was, to within those 5 s. Two
things differ: the period starts when the session is created, resumed or
woken, not at the first sweep that sees it running; and it survives a
restart of the backend, which used to give every session a fresh period.

### Idleness from telemetry instead

The alternative considered: the backend only emits metrics, and a separate
component queries the telemetry system for each session's last activity and
sleeps the idle ones. The backend now emits those metrics
([metrics.md](metrics.md)), and the source of "last active" is behind an
interface (`idle.Activity`, `idle.Rule.Source`), so that implementation can
be written. It is not the one in use, for these reasons.

| | From the session object (in use) | From telemetry queries |
|---|---|---|
| Delay from a use to it being visible to the sweep | none for the first use in 30 s (written before the call is forwarded); otherwise the session is under 30 s from its last stamp | scrape interval (30 s) + ingestion (seconds to a minute or more in a managed service) + query |
| The race "sleep vs. a call just taken" | closed: the suspend is conditional on the object the stamp is on | open: the suspend cannot be made conditional on a time series. A call inside the pipeline's delay is invisible, and the session is suspended under it |
| When the path is down or late | the API server is down: nothing is written, and nothing can be suspended either. They fail together | a scrape gap, a collector restart, a NetworkPolicy mistake or a query error looks exactly like "no activity". Either every session sleeps (if absence means idle) or none ever does (if absence means unknown), and telling the two apart needs a second signal |
| A replica dies | its heartbeat stops; the stamp it left stands | its series go stale; "stale" and "idle" must be told apart again |
| What it costs | at most 2 API writes a minute per session in use; no new component | samples (cheap) plus a query per session or one per sweep; a new component with credentials to the monitoring API and to the cluster |
| What has to be right | one annotation, one conditional write | scrape config, relabeling, retention of a per-session series, the query, its lookback window, clock agreement across three systems |
| Observability | none by itself | dashboards and alerts for free |

So the metrics are there for watching the system and for canary analysis,
and the decision stays on the object. If the owner still prefers telemetry
as the source, the place to change is one implementation of `LastActive`;
the race in the second row would then be open, and the sweep would need a
rule for missing data.

## Races, and how each is closed

**The sweep decides to sleep a session while another replica takes a call.**
The suspend is a write conditional on the read it was decided from
(`resourceVersion`), and the question "still idle?" is asked of that read
(`sessions.Store.Sleep`). The other replica writes `last-active` before it
forwards. If its write lands first, the suspend conflicts, reads again and
finds the session in use. If the suspend lands first, the replica's write
returns the session as suspended; the replica drops what it remembered and
wakes it.
Not closed: a call that arrives within 30 s of that replica's last write
writes nothing, but then `last-active` is under 30 s old and the session is
nowhere near idle.

**A snapshot takes minutes; the session is used meanwhile.** Same mechanism:
the question is asked after the snapshot, on a fresh read. The snapshot is
discarded.

**Clock skew.** `last-active` is one replica's clock, read against
another's. The sweep adds 5 s. A session that is held open has a stamp at
most about 35 s old against a period of 15 minutes, so skew matters only
for sessions nobody holds, where it moves the sleep by the skew.

**A replica dies with connections open.** Its heartbeat stops. The session
sleeps one idle period after the last heartbeat.

**Billing's drain while another replica has a call in flight.** The replica
that sweeps counts its own calls and reads the other replicas' `in-flight`
marks; it ignores its own mark, so with one replica the drain is exactly as
it was. A mark reaches 45 s ahead and is renewed every 30 s while the call
runs. A replica writes its mark before forwarding a call, and the suspend
asks "anything in flight?" of the read it is conditional on, so a call taken
in between the mark and the suspend stops the suspend.
Cost: a replica does not write when its call ends, so after a call at
another replica the drain waits for the mark to run out, up to 45 s.
`BILLING_DRAIN_TIMEOUT` still bounds the whole drain.
Not closed: a replica that has not yet seen the draining mark (it remembers
a session for 2 s) can take in a new call in that window if its own mark is
still fresh. The call is waited for like any other; it is one call that
should have been refused.

**Two replicas wake one session.** `Store.Wake` is conditional: one resumes,
the other reads again and finds it awake. If a restore from a snapshot
fails, both may start it cold; the second finds nothing left to give up on
and may answer its caller 502 while the first carries on. A retry succeeds.

**Two replicas create for one user at the cap.** Each counts again after
creating; one that finds more sessions than it expected is judged as if it
came last, and deletes its own if refused. The user never ends past the cap;
both may give way, and the user asks again.

**Two replicas believe they lead.** A Lease allows it for a moment. Every
step of every pass is a conditional write decided from the cluster, so the
work is done twice and nothing else: at worst two snapshots of one session,
one of which is discarded.

## Tickets

A ticket is `base64url(session.user.role.expiry.nonce) "." base64url(HMAC)`,
where `user` is the hash the owner label uses, never an address (the ticket
travels in a URL). It is good for 10 s, for that session, and the screen is
opened only if the session's owner is the ticket's user (or the ticket is
an admin's).

"One-time" became "once per replica": each replica remembers the tickets it
redeemed until they expire. With one replica that is what it was. With N, a
ticket could be replayed at another replica within its 10 s by someone who
read it off the owner's browser or the proxy in front. That someone could
equally ask for a ticket of their own with the same access, and what a
ticket gives is the screen of a session its user may already see.

Every replica needs the same `API_SIGNING_KEY`. Without the Secret each
makes its own key and refuses the others' tickets and access tokens; the
backend says so at start.

## One leader for the passes

`internal/leader`, client-go's leader election over the Lease
`backend-leader`: lease 60 s, renew deadline 45 s, retry 15 s. A replica
that shuts down releases it. After a crash the passes resume within about a
minute, plus the pass's own interval.

With `ACTIVE_FILE` set to the pod's labels file (the downward API), a
replica campaigns only while it carries the label
`browserjs.dev/role="active"`, and gives the Lease up when it loses it. A
backend that a release is still checking then serves what it is sent and
runs no pass, and neither does it finish unfinished warm-pool claims
(`RecoverClaims`, now the leader's). Unset, every replica may lead.

A replica that may not read the Lease logs an error at start and never
leads. If that is every replica, nothing is ever put to sleep: apply
`deploy/base/backend.yaml`'s Role with the image.

## Load on the API server

Writes of activity are `PATCH`es of a Sandbox's annotations.

| | Per minute |
|---|---|
| A running session nobody is using | 0 |
| A session in use, or held by a viewer, through one replica | at most 2 writes |
| 20 running sessions, all in use, one replica | at most 40 writes |
| The same, every session's traffic spread over two replicas | at most 80 writes |
| A session a replica only has an event stream to | 2 reads |
| The Lease | 4 writes by the leader, 4 reads by each other replica |
| Sweeps | 1 list (idle) and 2 lists (billing), by the leader only: no longer per replica |

Each write is an event for whatever watches Sandboxes (the Agent Sandbox
controller, the billing observer, the policy operator).

## Running two replicas

Not done here: `deploy/base` still says `replicas: 1`, `Recreate`, and
`deploy/gke` is untouched.

- **The first rollout of this version must not overlap the old one.** A
  backend from before keeps idle clocks in memory, would sleep sessions in
  use through the new one, and refuses signed tickets. Hence `Recreate`
  stays for now.
- After that, `replicas: 2` with a rolling update is safe, and so is a
  weighted canary of two versions. Either version may hold the Lease, so a
  canary's passes act on every session, whatever its share of requests.
- Cost on GKE: a second replica requests 100m CPU and 128Mi (limit 512Mi)
  on the system node, an `e2-standard-2`. No new node, and so no new cost,
  if that fits beside what is there; a second system node is about $49 a
  month. Whether it fits was not measured. Two replicas on one node do not
  survive the node.

## What is not verified

- No run with two real replicas, on kind or GKE. `test/integration.py`
  (kind, local) was not run; CI's kind jobs use a stand-in for the backend
  and check only the Role (`test/billing/run.sh` now asks about the Lease).
- Leader election against a real API server (the test uses client-go's
  fake, made to refuse stale writes).
- How the Agent Sandbox controller and the other watchers take two
  annotation writes a minute per session in use.
- That the conflict behaviour of `PATCH` against `PUT` on a real API server
  is what the fake's is.
- Whether a second replica fits on the system node.
