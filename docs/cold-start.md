# Session cold start

How long a session takes to start when no session node is running, where the
time goes, and what can be done about it. Measured on the production cluster
on 2026-10-02 (GKE 1.36.4-gke.1247000, containerd 2.2.7, gVisor pool
`sessions-n2d-standard-4`, us-west1-c) with the `cluster info` workflow
(runs 36954819323, 36955000257, 36956372096; session `s-yqm5374u4p`).

Anything not measured here or stated in a Google document is marked
UNVERIFIED.

## Where the time goes

103 seconds from the autoscaler's decision to a ready pod, for a session that
had never run before:

| UTC | Step | Seconds | Running total |
|---|---|---|---|
| 02:15:34 | `TriggeredScaleUp` (n2d pool, us-west1-c, 0 to 1) | | 0 |
| 02:16:03 | Node object created: VM provisioned, booted, kubelet registered | 29 | 29 |
| 02:16:12 | Node `Ready` | 9 | 38 |
| 02:16:30 | Pod `Scheduled`. It waited for the PD CSI driver to register on the node (`ProvisioningFailed: no topology key found for node` at 02:16:20) and for the session's new disk (provisioned 02:16:28 to 02:16:29) | 18 | 56 |
| 02:16:37 | Disk attached | 7 | 63 |
| 02:16:39 | `Pulling` the browser image | 2 | 65 |
| 02:17:10 | `Pulled`: "in 31.448s … Image size: 905274159 bytes" | 31 | 96 |
| 02:17:11 | browser container started; `Pulling` mcp-js | 1 | 97 |
| 02:17:15 | mcp-js `Pulled` "in 4.289s … 80931663 bytes", started | 4 | 101 |
| 02:17:17 | Sandbox `Ready` (both startup probes passed) | 2 | 103 |

The pod's start time, the containers' start times, the node's creation and
Ready times and the Sandbox's Ready time are exact (from `describe` and the
Sandbox status). The other rows are event ages ("2m7s") subtracted from the
time the workflow ran, good to a second or two.

By cause:

| Cause | Seconds | Share |
|---|---|---|
| Node: VM, boot, kubelet, Ready | 38 | 37 % |
| Image pulls (browser 31.4, mcp-js 4.3, one after the other) | 36 | 35 % |
| Storage: CSI registration, new disk, attach | 25 | 24 % |
| Container start and probes | 4 | 4 % |

Not in the 103 seconds:

- **The autoscaler's reaction**, from pod creation to `TriggeredScaleUp`. On
  a small cluster Google says only that "the inspection might happen every few
  seconds" (CA). Not measurable in this sample, see the next point.
- **A zone without capacity.** This session was created at 01:52:53, the first
  scale-up (n2, us-west1-a) failed with "GCE out of resources", and the pod
  then sat for 22 minutes marked `cluster_autoscaler_unhelpable_until: Inf`
  until the fallback pools existed. Google documents the retry: "the
  underlying Managed Instance Group retries the operation after an initial
  five-minute backoff. If errors continue, this backoff period increases
  exponentially to a maximum of 30 minutes" (CA). The three zones and the two
  fallback machine types (#7) are what keep this from recurring; it dwarfs
  everything else on this page when it happens.
- **A warm node.** The second session, `s-n5uslvwkxf`, landed on the same
  node four minutes later: "Container image … already present on machine",
  scheduled, attached and started inside one minute-resolution event bucket.

The browser image is 905 MB compressed and about 3.6 GB unpacked, in a single
layer (`images/browser/Dockerfile` ends in one `COPY --from=build /rootfs /`).
3.6 GB in 31 s is about 115 MB/s, which is the speed of one gzip stream being
decompressed: the pull is bound by unpacking, not by the network (ANALYSIS,
not measured separately).

## Options

Sources: IS = <https://docs.cloud.google.com/kubernetes-engine/docs/how-to/image-streaming>,
SBD = <https://docs.cloud.google.com/kubernetes-engine/docs/how-to/data-container-image-preloading>,
GVISOR = <https://docs.cloud.google.com/kubernetes-engine/docs/concepts/sandbox-pods>,
AS = <https://docs.cloud.google.com/kubernetes-engine/docs/how-to/agent-sandbox>,
CA = <https://docs.cloud.google.com/kubernetes-engine/docs/concepts/cluster-autoscaler>,
BLOG = <https://cloud.google.com/blog/products/containers-kubernetes/accelerate-agentic-rl-with-gke-agent-sandbox>
(29 September 2026).

### 1. Image streaming (done: `session_image_streaming`)

The node mounts the image remotely and the container starts at once, reading
blocks as it touches them. Google's own example: a 327 MB image "Pulled" in
1.5 s instead of 24 s (IS).

- **With gVisor:** neither IS nor GVISOR mentions the other; GVISOR's list of
  incompatible features does not include it, and both need `cos_containerd`.
  Google's Agent Sandbox team describes running exactly this: "a 10-node
  gVisor sandbox pool with GKE image streaming enabled" (BLOG). So: supported
  in practice according to Google, not stated in the reference docs, and
  UNVERIFIED on this cluster until the first session after the apply.
- **Requirements** (IS), all met by this change: the Container File System
  API; images in Artifact Registry; Private Google Access on a private
  subnet (already on); for a custom node service account, "Service Usage
  Consumer (roles/serviceusage.serviceUsageConsumer)"; GKE 1.30.1-gke.1329000
  or later for `COS_CONTAINERD`.
- **Cost:** no charge for the feature. GKE reserves a little node memory for
  it ("1% of the first 4 GiB", less above; IS).
- **Saves:** up to 31 s of pull on a new node. Chromium then reads its
  binary and libraries over the network while starting, and IS warns that
  workloads which "require a large proportion of the image to be available
  before code can execute" gain less, so expect 20 to 28 s net (UNVERIFIED
  estimate).
- **Applying it:** "This change requires recreating the nodes", and GKE does
  so immediately, without waiting for the maintenance window (IS). Pools at
  zero nodes have nothing to recreate. A session node that is running is
  replaced by surge upgrade and its sessions restart from their disks.
- **Fallback:** an image GKE cannot stream is pulled the ordinary way (IS).
- **UNVERIFIED:** restoring a Pod Snapshot onto a node that streams the
  image. The Pod Snapshots page does not mention image streaming. If restores
  misbehave, set `session_image_streaming = false` (which again recreates
  the nodes).
- **To check afterwards:** the pod's events show `ImageStreaming` and a
  `Pulled` in about a second (IS).

### 2. Secondary boot disk with the images preloaded (recommendation)

A disk image holding the unpacked browser and mcp-js images is attached to
every new node, so nothing is pulled or lazily read.

- "Configure Image streaming to use the secondary boot disk feature" (SBD):
  it builds on option 1.
- "When you modify the disk image, you must create a new node pool. Updating
  the disk image on existing nodes is not supported" (SBD). Every browser
  image release would mean building a disk image (`gke-disk-image-builder`
  starts a VM, pulls, snapshots; SBD) and **replacing the three session
  pools**, not updating them.
- **With gVisor:** not mentioned in SBD or GVISOR. UNVERIFIED.
- **Cost:** "Persistent Disks are billed based on Compute Engine disk
  pricing" (SBD): the stored disk image (about 5 GB, about $0.25 a month)
  plus a small disk per node while it runs. No always-on node.
- **Saves:** the same 31 + 4 s as option 1 without Chromium's lazy reads,
  so perhaps 5 to 10 s more than option 1. Not worth a pool replacement per
  image release unless option 1 measures badly.

### 3. A smaller, better layered browser image (recommendation)

The image's content, from the binary cache for the pinned nixpkgs
(`nix path-info -rs --store https://cache.nixos.org`, 458 store paths,
2.4 GB before the npm package; the 3.6 GB figure was not re-measured):

| What | Closure, MB | Note |
|---|---|---|
| chromium | 1815 | `chromium-unwrapped` alone is 737 |
| novnc + websockify | about 495 | python 142, numpy 54, blas 70, lapack 70, openblas 34, gfortran 14 |
| openbox | 346 | mostly shared with chromium (gtk, icu) |
| nodejs_22 | 260 | |
| xorg-server | 146 | |
| caddy | 89 | not used in a session pod (`SESSION_MODE=1`) |
| fonts | about 70 | |
| Not shared with chromium | 608 | |

- **Smaller:** websockify drags in numpy and three BLAS libraries (about
  240 MB) for an optional speed-up of its unmasking loop; an override without
  numpy, and a session-only image without caddy, would cut about 330 MB
  unpacked, roughly 9 % (UNVERIFIED: needs a build, and `images/browser/test`
  rerun). Chromium's own closure (flite and freepats for speech, 96 MB; perl,
  58 MB) is harder to trim.
- **Layered:** one 905 MB layer means every image release re-downloads
  everything, and nothing is fetched in parallel. Putting
  `chromium-unwrapped` and its closure in one layer and the rest in a second
  (two `COPY --from=build` lines over a split rootfs, or
  `dockerTools.buildLayeredImage`) makes a release that only changes
  `server.js` or `entrypoint.sh` a pull of a few megabytes on a node that has
  the old image. On a brand-new node it saves little: containerd downloads
  layers in parallel but the pull here is bound by unpacking.
- **zstd layers** (`outputs: type=image,compression=zstd` in buildx) unpack
  several times faster than gzip and would attack the 31 s directly without
  image streaming. UNVERIFIED whether image streaming accepts zstd layers
  (IS does not say), so do not combine the two without testing.
- **Cost:** none. **Saves:** about 3 s (smaller) on a cold node without
  streaming, close to nothing with it; the real gain is on image releases.
- Not done here: the image cannot be built or tested on the machine this was
  written on, and with option 1 the gain at cold start is small.

### 4. A warm spare node (recommendation; costs money every month)

`min_node_count = 1` on one session pool, or a low-priority placeholder pod.

- **Works with gVisor:** yes, it is the same pool.
- **Saves:** the whole 103 s for a session that fits the node: start takes
  the few seconds the second session took.
- **Cost:** about $0.21 an hour on demand for an n2-standard-4 with its disk
  (see infrastructure.md, section 7), **about $153 a month**; about $95 on
  Spot; n2d-standard-4 about 13 % less.
- **Limits:** a session's disk is zonal, so a sleeping session can only wake
  on the spare node if the node is in the disk's zone, and a Pod Snapshot
  only restores on the machine series it was taken on. One spare node covers
  one zone and one series out of nine combinations. It helps new sessions
  always and waking sessions only when the pools are narrowed to match.
  `min_node_count` is per zone: with three zones it would be three nodes
  unless the pool's zones are narrowed.

### 5. Agent Sandbox warm pools (recommendation; costs money every month)

"A pre-warmed sandbox is a running Pod that's already initialized. This
pre-initialization enables new sandboxes to be created in under a second"
(AS). `SandboxWarmPool` keeps N such pods; a `SandboxClaim` takes one.

- **Works with gVisor:** yes, it is the feature's intended use (AS, BLOG).
- **Cost:** warm pods are running pods, so a node is always on: option 4's
  $153 a month, plus the pods' memory.
- **Saves:** everything, including container start, for new sessions.
- **Does not fit as is:** the backend creates a `Sandbox` with its own
  `volumeClaimTemplates`; a warm pool needs `SandboxTemplate` and
  `SandboxClaim`, and AS does not say whether a claimed warm pod can be given
  a per-session disk (UNVERIFIED). It does nothing for waking a sleeping
  session, which is the Pod Snapshot path.

### 6. Faster node boot (nothing to do)

The node is registered 29 s after the scale-up decision and Ready 9 s later,
on Container-Optimized OS, which GKE Sandbox requires ("Nodes must use the
Container-Optimized OS with containerd", GVISOR). No documented setting
shortens this. Boot disk type: the unpack is CPU-bound, so `pd-ssd` or
Hyperdisk would not obviously help (UNVERIFIED), and changing it recreates
nodes.

### 7. Storage wait (observation)

18 s passed between node Ready and the pod being scheduled, most of it
waiting for the PD CSI driver to register its topology on the new node
before the session's disk could be provisioned (`WaitForFirstConsumer`).
A waking session already has its disk, so it may skip part of this
(UNVERIFIED: no wake onto a new node was captured). Creating the disk when
the session is created, before a node exists, would need an `Immediate`
StorageClass pinned to one zone, which undoes the three-zone fallback. Not
recommended.

### 8. Autoscaler tuning (recommendation; costs a little)

- GKE exposes no scan interval or scale-down delay: "It is not possible to
  define an exact timeframe" (CA).
- `autoscaling_profile = "OPTIMIZE_UTILIZATION"` (set today) "can remove
  more nodes, and remove nodes faster"; `BALANCED` "prioritizes keeping more
  resources readily available for incoming pods" (CA). Switching back keeps
  an emptied node around longer (about 10 minutes; UNVERIFIED figure), so a
  user who comes back soon after their session sleeps finds a warm node.
  Cost: those extra node-minutes at $0.21 an hour, a few dollars a month at
  most. It is a cluster-level, in-place change. Not made here because it is a
  cost decision.
- `location_policy = "ANY"` instead of `BALANCED` would let the autoscaler
  "search for requested capacity across all specified zones" (CA); it does
  not change speed when capacity exists.

## Order of work

| # | What | Seconds saved, cold | Monthly cost | Status |
|---|---|---|---|---|
| 1 | Image streaming on the session pools | 20 to 28 of the 36 s of pulls (estimate) | $0 | Done here; gVisor support per BLOG, not in the docs; measure after apply |
| 2 | `BALANCED` autoscaling profile | all 103, but only for a return within the longer idle window | a few $ of node-minutes | Recommendation; delay figures UNVERIFIED |
| 3 | Browser image: two layers, drop numpy/caddy | about 3 cold; most of the pull on an image release | $0 | Recommendation; needs a build and the image tests |
| 4 | Secondary boot disk | 5 to 10 beyond #1 | under $1, plus a pool replacement per image release | Recommendation; UNVERIFIED with gVisor |
| 5 | One warm node (n2-standard-4, one zone) | all 103 for sessions that fit its zone and series | about $153 (Spot about $95) | Recommendation |
| 6 | Agent Sandbox warm pool | all 103 plus container start, new sessions only | #5 plus pod memory | Recommendation; needs backend redesign |

After #1 the expected cold start is about 75 to 85 s, of which 63 s is the
node and its storage: without an always-on node, that part is the floor.
