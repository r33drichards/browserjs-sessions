# Warm pool

A new session used to take about 103 s to start (node 38 s, disk 25 s, image
pull 36 s, start 4 s). With the warm pool, one session pod is always running
with nobody in it, and creating a session hands that pod over. The pool then
starts the next one in the background.

Sources, and what is and is not confirmed, are at the end.

## How it works

Three Agent Sandbox resources of `extensions.agents.x-k8s.io/v1beta1`:

- `SandboxTemplate/session` (`deploy/gke/warmpool.yaml`): the session pod
  and its 5Gi disk, the same as `deploy/gke/blueprint.yaml`.
- `SandboxWarmPool/s`: keeps `replicas` (1) Sandboxes of that template
  running. Each is a complete `Sandbox` named `s-<5 characters>`, with its pod
  and its `data-s-…` disk, owned by the pool.
- `SandboxClaim`: made by the backend for each new session. The claim
  controller binds it to a waiting Sandbox, which keeps its name, pod and
  disk and becomes the claim's instead of the pool's.

`Store.Create` (`backend/internal/sessions/warm.go`), when `WARM_POOL` is set:

1. creates a `SandboxClaim` carrying the owner label, owner and name;
2. waits up to `WARM_POOL_WAIT` (5 s) for `status.sandbox.name`;
3. writes the owner label, the owner and name annotations, the adoption time
   and `operatingMode: Running` onto that Sandbox, in one compare-and-swap
   that refuses a Sandbox that is not the claim's or already has an owner.

The session's ID is the Sandbox's name, as it always was, so nothing after
creation changes: the proxy, the hosts, authorization, the idle sweep, sleep
and wake all see an ordinary session Sandbox. If any step fails the claim is
deleted and the session starts cold from the blueprint, as before.

Two things follow from "the ID is the Sandbox's name":

- **The pool must be named `s`.** The pool names its Sandboxes
  `<pool>-<5 characters>`; `ValidID` accepts `s-` plus ten base32 characters
  (cold) or five characters (warm). `WARM_POOL` set to anything else is
  refused at startup.
- **The public URL comes from the pod's name.** A cold session gets
  `MCP_V8_PUBLIC_URL` rendered into its pod. A warm pod exists before its
  session, and the claim controller cannot add a variable to a running pod
  (a claim with `env` is always started cold). But a Sandbox's pod has the
  Sandbox's name, which is the session ID from the start, so the template
  sets `SESSION_ID` from `metadata.name` and
  `MCP_V8_PUBLIC_URL=https://$(SESSION_ID).sessions.browserjs.com`. No image
  changes.

**Delete** removes the session's snapshots, then the claim, then the Sandbox. A Sandbox deleted from
under its claim would be replaced: the claim controller binds the claim to
another one. This holds whether or not `WARM_POOL` is still set.

**A crash between steps 1 and 3** leaves a running Sandbox with no owner,
which no user and no idle sweep can see. The claim already names the owner,
so `Store.RecoverClaims` finishes the job at the backend's next start.

**The pod has no owner label.** A cold session's pod is labelled
`browserjs.dev/owner`. A claim may only add pod labels from the controller's
allow-list (`sandbox.users.io` by default), and nothing reads the pod's
label; the Sandbox has it.

## With Pod Snapshots

A session from the pool sleeps to a snapshot and wakes from it like any
other ([gke-deployment.md](gke-deployment.md)); nothing in that path looks at
where the Sandbox came from.

- The `PodSnapshotPolicy` selects pods by `app: browserjs-session`, which
  the template has, and groups by `agents.x-k8s.io/sandbox-name-hash`, which
  the Sandbox controller derives from the Sandbox's name. Adoption changes
  neither.
- The pin to the snapshot's node pool is written into the Sandbox's
  `podTemplate.spec.nodeSelector`. The claim controller only ever rewrites
  the pod template's labels and annotations, so the pin stays.
- Delete removes the snapshots first, as before: a failure there leaves the
  whole session to delete again. Then the claim, then the Sandbox.
- **Names come round again.** A pod is restored from the newest snapshot of
  its Sandbox's name, and the pool has about 14 million names where the
  backend's own IDs have 2^50. A session removed without its snapshots (with
  kubectl rather than through the app) leaves one that a later pooled
  Sandbox of the same name would be restored from. So before a pooled
  Sandbox is given to anyone, the backend looks for snapshots under its
  name; if there are any, the Sandbox and then the snapshots are deleted and
  the session starts cold.

## What a waiting session is

It runs Chromium and mcp-js on a fresh disk and has never run anybody's code:
a Sandbox is handed out once and is never returned to the pool. The
`session-pods` NetworkPolicy applies to it (same `app` label). It has no owner
label, so it is in nobody's list, the proxy refuses its host, and the idle
sweep never puts it to sleep.

## Turning it off, sizing it

- `replicas: 0` in `warmpool.yaml`: nothing waits, nothing is paid; sessions
  are still made through claims and start cold.
- Remove `WARM_POOL` from `deploy/gke/patch-backend.yaml`: the backend makes
  Sandboxes itself as before. Sessions that came from the pool keep working
  and can be deleted. `deploy/local` does not set it.
- More than one user arriving within a start-up time of each other: raise
  `replicas`. Ten sessions fit on a node, so this costs disks, not nodes.

## Cost

A waiting session keeps one sessions node up around the clock. From the
table in [infrastructure.md](infrastructure.md) section 7 (n2-standard-4,
us-west1, 730 h; node + 100 GB boot disk + NAT):

| | per hour | per month |
|---|---|---|
| On demand | $0.2093 | about $153 |
| Spot | $0.1316 | about $96 |

plus $0.50 a month for the waiting 5 GB disk. That node also serves real
sessions (about ten fit), so the extra cost is the hours in which nobody has
a session running: at worst the whole amount, taking the cluster's idle bill
from about $78 to about $231 on demand or $174 on Spot. On the n2d pool the
node is $0.169/h on demand, about $135 a month all in.

## Sources and what is confirmed

- **Served versions.** VERIFIED on the cluster (`cluster info` run
  [36957250957](https://github.com/r33drichards/browserjs-sessions/actions/runs/36957250957)):
  `sandboxclaims`, `sandboxtemplates` and `sandboxwarmpools` in
  `extensions.agents.x-k8s.io` serve `v1alpha1,v1beta1`; the add-on's
  component version is 1.36.12.
- **Behaviour.** Read from upstream
  [kubernetes-sigs/agent-sandbox](https://github.com/kubernetes-sigs/agent-sandbox)
  at v0.5.6, the last release that has both `v1alpha1` and `v1beta1`
  (v1.0.0 dropped `v1alpha1`): `extensions/api/v1beta1/*_types.go`,
  `extensions/controllers/sandboxwarmpool_controller.go` (`buildSandboxCR`:
  the pool creates whole Sandboxes, volume claim templates included, with
  `generateName: <pool>-`), `sandboxclaim_controller.go` (`completeAdoption`:
  ownership moves to the claim, the name stays; `getOrCreateSandbox`: a claim
  with `env` or `volumeClaimTemplates` skips the pool; a claim whose Sandbox
  is gone takes another) and `controllers/sandbox_controller.go` (the pod is
  named after the Sandbox, the disk `<template>-<sandbox>`; `operatingMode`
  is handled there, and the claim controller never writes it).
- **UNVERIFIED: that GKE's managed controller is that code.** Google's
  [CRD reference](https://docs.cloud.google.com/kubernetes-engine/docs/reference/crds/agentsandbox)
  describes a claim with `sandboxTemplateRef` (the `v1alpha1` shape) and the
  [how-to](https://docs.cloud.google.com/kubernetes-engine/docs/how-to/agent-sandbox)
  speaks of warm *pods* owned by the pool, which is how upstream worked
  before v0.5. If the cluster behaves that way, a warm pod has no disk and
  not the session's name. The backend then still makes working sessions
  (the claim is refused or times out, and the session starts cold), but the
  pool is wasted. Check before relying on it: `hack/gke-status.sh --full`
  prints the fields the served `v1beta1` schemas accept and each Sandbox's
  owner.
- **UNVERIFIED on the cluster, with snapshots:** that a pod adopted from the
  pool restores (GKE restores "into an identical pod spec"; the spec is the
  same, but the pod's labels were changed by adoption between its start and
  its snapshot), and that the waiting pod, which matches the snapshot
  policy's selector, is left alone until the backend triggers a snapshot.
- **UNVERIFIED on the cluster:** that adoption does not restart the pod
  (the claim controller rewrites the Sandbox's pod labels), sleep and wake of
  an adopted Sandbox, and how the pool behaves when a Spot node is reclaimed.
