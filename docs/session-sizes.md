# Session sizes

A session is small, medium or large. The size is chosen when it is created
(`size`, default `small`) and can be changed later. It decides how much CPU
and memory the desktop has, and nothing else: every size is the same pod,
the same images, the same 5 GB disk.

What is and is not confirmed on the cluster is at the end.

## The three sizes

Per pod: the browser container (the desktop) plus mcp-js. Requests are what
the scheduler packs a node by; limits are what the session may use.

| | Desktop limit | mcp-js limit | Pod requests | `/dev/shm` | `run_js` heap default | Fit an empty node |
|---|---|---|---|---|---|---|
| **small** | 1.5 CPU, 2 GiB | 0.5 CPU, 1 GiB | 200m, 1280Mi | 1 GiB | 8 MB | 9 |
| **medium** | 2 CPU, 5 GiB | 0.5 CPU, 1 GiB | 550m, 3328Mi | 2 GiB | 16 MB | 3 |
| **large** | 3 CPU, 10 GiB | 0.5 CPU, 1 GiB | 2050m, 11264Mi | 4 GiB | 32 MB | 1 |

Why these numbers. A session node is a 4-vCPU, 16 GB machine
(`n2-standard-4`, or the `n2d` and `c3` fallbacks). After the kubelet's
reservation and GKE's own pods it has **3213m of CPU and 12097Mi of memory
left for sessions** (measured: the table in `deploy/gke/warmpool.yaml`).
Memory is what fills a node, so memory is what the sizes are cut from.

- **Small** is today's session, unchanged: `deploy/gke/blueprint.yaml` as it
  is written. Its memory request (1280Mi) is well under its limit (3 GiB for
  the pod), which is how nine share a node; a session started desktop-only
  idles at about 160 MiB.
- **Medium** requests 3328Mi, a little over a quarter of a node, so three
  fit an empty one. Its limit (6 GiB for the pod) is under twice its
  request.
- **Large** is the most one node can give. Its memory request **is** its
  limit (11264Mi of the 12097Mi), so the scheduler puts nothing beside it:
  the 833Mi left fit no session. It is never short of memory because of a
  neighbour, and it is never the neighbour that makes a small session short.
  CPU is not reserved in full (2050m): on a node of its own the rest is free
  anyway, and it may use 3.5 cores.

`/dev/shm` is memory and counts against the desktop's limit; Chromium uses
it for shared memory, and a small one crashes tabs on heavy pages. Chromium
itself is given no flags: it sizes its caches from the memory it sees. The
`run_js` heap default is mcp-js's `MCP_V8_HEAP_MEMORY_MAX`; a call can still
ask for up to 64 MB whatever the size.

## How many can run at once

The project's quota is 12 CPUs in all regions: the system node and **two
session nodes**. `session_max_nodes` in `infra/main` is 3 a pool, but the
quota is what binds. The first session node holds the warm pool: seven
small pods waiting (8960Mi), and room for two more small ones.

So, cluster-wide and for every user together, the second node holds one of:

| On the second node | And beside it, on the first |
|---|---|
| **1 large** | the warm pool, and 2 small sessions that were woken or started cold |
| **3 medium**, and 1 small in what they leave | the same |
| 2 medium and 4 small | the same |
| 9 small | the same |

- **At most one large session can be awake at a time**, and while it is,
  no medium one can be.
- **At most three medium sessions** can be awake at a time.
- A medium or large session never lands on the warm pool's node: seven
  waiting pods leave 3137Mi, and a medium needs 3328Mi.
- Each small session taken from the pool is replaced by a new waiting pod.
  With the second node taken, the pool cannot refill past the first node's
  nine places: its new pods wait as `Pending` until a session sleeps. That
  costs nothing and hurts nobody, but a create then finds no warm pod and
  falls back to a cold start, which the capacity check below may refuse.

Sessions that are asleep or stopped take no room. Fifteen idle minutes put a
session to sleep, so these are limits on sessions in use, not on sessions
owned.

### When there is no room

A session that does not fit is not left `starting` until it times out. The
backend looks before it makes or starts one
(`backend/internal/sessions/sizes.go`, `room`): it reads every Sandbox
(sessions and warm pods alike), adds up what each node's pods ask for, and
asks whether the new pod fits a node that is there, or whether a node can
still be added (`capacity.nodes` in `deploy/gke/sizes.yaml`). If neither:

```
409 Conflict
Retry-After: 120
{"error":"no capacity for a large session right now: every session node is full. Try again later, or pick a smaller size.","code":"no_capacity"}
```

Nothing is created, and a session that was to be woken stays asleep. This
is asked on a cold create, on `wake`, on `PATCH {"action":"resume"}` and on
the wake an MCP call causes. A small session taken from the warm pool is
not asked about: its pod is already running.

It is a look, not a reservation. Two creates at the same moment can both be
told there is room, and one of them then waits as `starting` for up to
`READY_TIMEOUT` (5 minutes) and fails. The check also trusts
`capacity`: raise `nodes` when the quota is raised.

### Is a bigger node pool worth adding? Not within this quota.

An 8-vCPU pool (`n2-standard-8`, 32 GB) would allow an extra-large size of
about 7 CPU and 27 GiB, or a large session that shares its node. But the
quota leaves room for **one** such node beside the system node, in place of
both 4-vCPU nodes: the same 8 vCPUs for the same price (about $0.42 an hour
on demand, about $0.26 on Spot), with every session on one Spot machine
instead of two, and a snapshot taken on it restorable only on that pool. It
buys a bigger biggest size and nothing else, and costs the redundancy.
**Not added.** The thing to ask for is the quota: at 24 CPUs, five session
nodes, the limits above become four large or twelve medium, and a pool of
8-vCPU nodes becomes a real option.

## One blueprint, sizes as numbers

There is one pod template: `deploy/gke/blueprint.yaml`, which is the small
session, and the `SandboxTemplate` of `warmpool.yaml`, which a test holds
identical to it. The other sizes are **not** further templates. They are
`deploy/gke/sizes.yaml`, in the same ConfigMap as the blueprint, and that
file can say only three things for a size: each container's requests and
limits, the size of `/dev/shm`, and environment variables that go with the
size. The backend puts those into the blueprint when it makes a session of
that size. A size cannot differ from small in its image, its probes, its
security context or its volumes, because the file has no way to say so
(it is parsed strictly; an unknown key is an error at start).

A `SandboxTemplate` per size would only be needed to keep the bigger sizes
warm, and they are not kept warm: a waiting large pod would hold a whole
node, a quarter of the project's quota, for nobody.

`backend/internal/sessions/deploy_test.go` holds the files together:

- the warm pool's template is the blueprint (as before);
- a session of each size, made from the real files, is a small session but
  for resources, `/dev/shm` and the sizes' variables;
- 9 small, 3 medium, 1 large fit a node of `capacity`, and nothing fits
  beside a large one;
- every size has a rate in `deploy/base/catalogue.yaml`, and nothing else
  has.

A deployment with no `sizes.yaml` (kind, `deploy/local`) has small only:
`GET /api/sizes` lists one size and the create form shows no choice.

## Cold start

Only small sessions are kept warm. A medium or large session always starts
cold, from the blueprint:

| Where it lands | Expected |
|---|---|
| A session node that is running and has room | about 10 to 30 s: the disk is made and attached, the image is on the node already (measured for a small session on a warm node: "a few seconds" plus the disk, [cold-start.md](cold-start.md)) |
| A node that has to be started | about 75 to 105 s: 103 s was measured before image streaming, 75 to 85 s is expected with it |

A large session nearly always needs the second node to itself, so unless
that node happens to be up and empty its start is the second row. Medium is
the first row when the second node is already up.

## Changing the size of a session

`PATCH /api/sessions/{id}` (and `/v1/...`) with `{"size": "large"}`.

- **Asleep or stopped**: the session has the new size at once, and starts
  at it.
- **Awake**: it keeps running as it is. The answer shows `size` unchanged
  and `pendingSize`; it has the new size from its next start (after a sleep,
  an idle sleep or a stop). `{"size": "large", "action": "stop"}` in one
  request does both.

Either way **the next start is a fresh one**: the desktop starts from the
session's disk, as after a stop. A resize cannot keep the running state,
for two reasons. A Pod Snapshot is restored only into a pod whose spec is
the one it was taken from ("distilled spec hash", [infrastructure.md](infrastructure.md),
section 3), and a size is a different spec. And the settings that go with a
size (`/dev/shm`, the heap default) are in the memory of processes that a
restore brings back as they were. So:

- a session resized while asleep loses its saved state (`stateSaved` goes),
  and its snapshot is deleted;
- a session with a resize waiting is put to sleep **without** a snapshot;
- the app says this before the change is made.

A Sandbox's pod template is only read when its pod is made, so the backend
never edits the template of a running session: the size asked for is an
annotation (`browserjs.dev/resize-to`) until the session is next suspended,
when it is put into the template (`browserjs.dev/size`). If that moment is
missed it is done when the session next starts.

Resizing does not need room at the time. Starting does: a session resized
to large while asleep can then be refused its wake (`409`, above) until a
node is free.

## What a size costs

Billing is off. The catalogue (`deploy/base/catalogue.yaml`) has the rates
for when it is on:

| | Awake, per hour | Disk |
|---|---|---|
| small | $0.20 | $1.40 a month |
| medium | $0.60 | the same |
| large | $1.60 | the same |

The rule: a size must make a profit on the cheapest plan at the pessimistic
cost, and earn at least what the small sessions it keeps off the node would.
Medium takes three small places and large nine, so medium is three times
small's rate and large is eight times (a round $1.60 rather than $1.80).

Costs per awake hour, from section 7.4 of the billing design: half-full
nodes, disk and traffic included; "pessimistic" is the design's pessimistic
case, about 1.36 times the planning cost. The large row is extrapolated: one
session to a node leaves no half-full node to average over, so it is the
whole node ($0.21 an hour on demand) plus its share of disk and traffic.

| | Places | Planning cost | Pessimistic cost | Rate | Kept on the cheapest plan (Scale, 40 % of the rate after its credit and Stripe's fee) | Over pessimistic cost |
|---|---|---|---|---|---|---|
| small | 1 | $0.039 | $0.053 | $0.20 | $0.080 | +51 % |
| medium | 3 | $0.098 | $0.134 | $0.60 | $0.240 | +79 % |
| large | 9 | about $0.29 | about $0.40 | $1.60 | $0.640 | +60 % |

Medium was proposed at $0.40 first. That keeps $0.16 on the cheapest plan:
only 19 % over the pessimistic cost, and less than the three small sessions
it displaces would keep ($0.24). So it is $0.60. Large at $0.80 kept $0.32,
under its pessimistic cost; at $1.60 it is above it on every plan. Both
margins rest on the unverified CPU and memory a large session really gets
under gVisor; because rates are data, a change is one line of the catalogue
(and a later `metronome_rates_starting_at` once the rates have been applied
to Metronome).

Which sizes a plan includes (`sizes` on `payg` and on each plan; small is
in every plan): pay as you go and Starter have small and medium; Pro and
Scale have all three. A create or a resize to a size the plan does not
include is refused with `403 size_not_included`.

How it is metered ([contracts/billing/metronome.md](contracts/billing/metronome.md),
"Sizes"): the observer reads each session's size and sends the awake
seconds of a medium or large session as their own event type
(`session.awake.medium`, `session.awake.large`), each with its own metric,
product and rate in `infra/billing`. Small is unchanged. A size is a metric
of its own rather than a dimension on the existing one because a billable
metric's definition is fixed for good.

## Settings

| Where | What |
|---|---|
| `deploy/gke/sizes.yaml` | the sizes other than small, and `capacity` |
| `SIZES_PATH` (backend) | where that file is; by default `sizes.yaml` beside the blueprint. Absent file: small only |
| `deploy/base/catalogue.yaml`, `sizes` and each plan's `sizes` | the rates, and which plans include which sizes |
| `infra/billing` | a metric, a product and a rate for each size in the catalogue |

A change to `sizes.yaml` applies to sessions made or resized afterwards (the
backend restarts with the ConfigMap). Sessions that exist keep the numbers
they were made with until they are resized.

## UNVERIFIED on the cluster

Everything above is tested against a fake API server. None of it has run on
GKE. To check there:

1. **Memory under gVisor.** That a large session really has 10 GiB to use:
   gVisor's own overhead (the Sentry, the Gofer) is charged to the pod, and
   how much of the limit it takes at this size is not known. Also whether
   the desktop sees the container's limit or the node's memory.
2. **CPU under gVisor on Intel.** GKE Sandbox turns SMT off on Intel nodes
   ([infrastructure.md](infrastructure.md), section 4): an `n2-standard-4`
   reports 3920m allocatable and has two physical cores. Whether a large
   session's "3 CPU" is three cores' worth of work there, or two, is not
   measured. `n2d` keeps SMT.
3. **Scheduling a large session.** That one lands on an empty node, that
   the autoscaler adds the second node for it, and that the fallback pools'
   nodes have the same room (`capacity` was measured on `n2-standard-4`
   only). And the refusal: that with both nodes in use the create answers
   `409` and not a pod that waits.
4. **The warm pool beside a large session**: that its waiting pods being
   `Pending` does no harm, and that the autoscaler does not thrash trying
   to place them.
5. **Resize and snapshots.** That a session resized while asleep starts
   cold and is never restored from the deleted snapshot (the delete is
   asynchronous on GKE's side), and that changing a suspended Sandbox's pod
   template is accepted by the managed add-on's admission policies.
6. **`MCP_V8_HEAP_MEMORY_MAX`** is read by the pinned mcp-js image. If it is
   not, the heap default stays 8 MB at every size and nothing else changes.
7. **Cold-start times** for medium and large are from the small session's
   measurements, not measured.
8. **Metronome**: the per-size metrics, products and rates are planned by
   `tofu test` with mocked providers and have not been applied to a
   Metronome environment.

## Rolling it out

Nothing here is applied by merging: `infra/main` is not changed, the deploy
and `billing-apply` workflows are started by hand.

1. Pin the backend image built from this change (`hack/pin-images.sh`) and
   deploy. The ConfigMap gains `sizes.yaml` in the same apply. A backend
   from before sizes ignores the file and offers small only, so the order
   within one deploy does not matter.
2. While billing is off the catalogue is not read. Before billing is turned
   on (`meter` or `enforce`), the backend that reads the catalogue must be
   this one or later: an older one parses the catalogue strictly and does
   not know `sizes`. Apply `infra/billing` (the per-size metrics, products
   and rates) before the first medium or large session is metered; until
   then the observer's `session.awake.<size>` events match no metric and
   that time is free.
3. Work through "UNVERIFIED on the cluster", starting with one large
   session on an empty second node.
