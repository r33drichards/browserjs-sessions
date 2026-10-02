# Infrastructure research: browserjs sessions on GKE

Date: 2026-10-01. Companion to the OpenTofu code in `infra/` (how it is run:
`infra/README.md`). Nothing described here has been applied to a cloud
account.

Every claim is marked **VERIFIED** (read in the cited source on 2026-10-01) or
**UNVERIFIED** (from memory, inferred, or only testable against a real
project). Google's docs were read as raw page text, not summaries. "Provider"
means `hashicorp/google`, which OpenTofu installs from its own registry.

## Summary

| Question | Answer |
|---|---|
| Is Agent Sandbox enablement in the provider? | Yes. `addons_config { agent_sandbox_config { enabled = true } }` on `google_container_cluster`; added in 7.34.0, GA in the `google` provider from 7.39.0. |
| Is Pod Snapshots enablement in the provider? | Yes. `addons_config { pod_snapshot_config { enabled = true } }`; GA in the `google` provider from 7.33.0. |
| gVisor node pools in the provider? | Yes, long-standing: `node_config { sandbox_config { type = "GVISOR" } }` with `image_type = "COS_CONTAINERD"`. |
| GA or preview? | Mixed signals. No Preview banner on any of the pages; the provider calls both fields GA; but Agent Sandbox can only be switched on with `gcloud beta`, and Google's concept page says Pod Snapshots "might be in Preview". Treat both as pre-GA until tested. |
| Autopilot or Standard? | Both support both features. This code uses **Standard**, because the session pool's machine type, CPU platform, zone and scale-to-zero need to be controlled. |
| Who installs the Sandbox controller and CRDs? | GKE, when the add-on is on. But Google's own snapshot tutorial still installs the open-source controller by hand "until the snapshot features are fully available in the GKE Agent Sandbox add-on". |
| Minimum version | 1.36.3-gke.1767000 for Agent Sandbox's `v1beta1` API; 1.35.3-gke.1234000 for Pod Snapshots. |
| Edge | Pomerium directly behind a passthrough Network Load Balancer, terminating TLS itself with a cert-manager certificate. The Gateway + Certificate Manager alternative is implemented behind `edge_mode`. |
| Cost at idle | About **$78 a month** with the GKE free-tier credit, about $151 without. Estimate. |
| Cost of use | About **$0.21 per session-node-hour**; $0.05 to $0.07 per session-hour when a node is full. Estimate. |

## 1. GKE Agent Sandbox

Sources: [Enable Agent Sandbox on GKE](https://docs.cloud.google.com/kubernetes-engine/docs/how-to/how-install-agent-sandbox) (ENABLE),
[About GKE Agent Sandbox](https://docs.cloud.google.com/kubernetes-engine/docs/concepts/machine-learning/agent-sandbox) (ABOUT),
[Isolate AI code execution with Agent Sandbox](https://docs.cloud.google.com/kubernetes-engine/docs/how-to/agent-sandbox) (ISOLATE),
[GKE Sandbox](https://docs.cloud.google.com/kubernetes-engine/docs/concepts/sandbox-pods) (GVISOR),
[Harden workload isolation with GKE Sandbox](https://docs.cloud.google.com/kubernetes-engine/docs/how-to/sandbox-pods) (GVISOR-HOWTO).

**How it is enabled.** VERIFIED (ENABLE). A cluster-level add-on:

- Autopilot, new cluster: `gcloud beta container clusters create-auto … --enable-agent-sandbox`.
- Standard: "you must create the cluster, add a node pool with gVisor enabled,
  and then enable the Agent Sandbox feature":
  `gcloud beta container clusters update … --enable-agent-sandbox`.
- API field: `addonsConfig.agentSandboxConfig.enabled` (the page's verify
  command reads exactly that path).
- Disable: `--no-enable-agent-sandbox`.

**Version and channel.** VERIFIED (ENABLE): "Ensure that your cluster is
running GKE version 1.36.3-gke.1767000 or later (supports the v1beta1 API)."
ABOUT gives an older floor, "1.35.2-gke.1269000 or later for full feature
support (including snapshots)", which predates `v1beta1`. No release channel is
named anywhere. UNVERIFIED: which channel's default is at or above
1.36.3-gke.1767000 today; that needs
`gcloud container get-server-config`, which was out of bounds here. The code
defaults to `REGULAR` and has a `kubernetes_version` variable.

**Autopilot vs Standard.** VERIFIED (ENABLE): both; Google recommends
Autopilot. Standard is chosen here anyway: see section 4.

**gVisor node pools (Standard).** VERIFIED (ENABLE, GVISOR, GVISOR-HOWTO):

- `gcloud container node-pools create … --image-type=cos_containerd --sandbox=type=gvisor`.
- "Nodes must use the Container-Optimized OS with containerd (cos_containerd) node image."
- "You cannot enable GKE Sandbox on the default node pool."
- "When using GKE Sandbox, your cluster must have at least two node pools. You
  must always have at least one node pool where GKE Sandbox is disabled. This
  node pool must contain at least one node, even if all your workloads are
  sandboxed."
- GKE labels the nodes `sandbox.gke.io/runtime: gvisor` and taints them
  `sandbox.gke.io/runtime=gvisor:NoSchedule`.
- Machine types: no restriction for gVisor itself beyond old versions and
  `e2-micro/small/medium`. ABOUT says Agent Sandbox is "optimized for specific
  node configurations (such as N2 machine types)". Google's quick start uses
  `e2-standard-2`; its snapshot tutorials use `n2-standard-2`.
- SMT: "Machine types with Intel processors: SMT disabled by default. Machine
  types without Intel processors: SMT enabled by default." You are billed for
  every vCPU either way. So an `n2-standard-4` gVisor node has 2 usable CPUs.
- Incompatible with gVisor: hostPath volumes, privileged containers,
  `kubectl port-forward`, seccomp/AppArmor/SELinux, sysctls, per-container
  memory metrics. The Compute Engine Persistent Disk CSI driver is supported.

**Who installs the controller and CRDs.** VERIFIED (ABOUT): "As a managed GKE
add-on, Google manages the full lifecycle of the controller, including
automatic upgrades and security patches." The add-on "is based on the
open-source Agent Sandbox controller project and follows its release cycles."
CRDs: `sandboxes.agents.x-k8s.io`,
`sandboxclaims|sandboxtemplates|sandboxwarmpools.extensions.agents.x-k8s.io`,
at `v1beta1` (with a conversion webhook from `v1alpha1`).

One caveat, VERIFIED
([Save and restore Agent Sandbox environments with Pod snapshots](https://docs.cloud.google.com/kubernetes-engine/docs/how-to/agent-sandbox-pod-snapshots)):
that tutorial does **not** use the add-on. It applies the open-source release
manifests and says: "This manual installation is a temporary workaround until
the snapshot features are fully available in the GKE Agent Sandbox add-on."
Its manifests are still `v1alpha1`. So whether a `Sandbox` suspended under the
managed add-on is restored from a Pod Snapshot is UNVERIFIED and is the first
thing to test on the real cluster. `enable_agent_sandbox = false` plus the
open-source controller is the fallback the tutorial itself uses.

**Admission policies.** VERIFIED (ENABLE, ISOLATE). GKE enforces two
ValidatingAdmissionPolicies on `Sandbox` and `SandboxTemplate`:

- `sandbox-core-policy`, managed in `Reconcile` mode, not modifiable: gVisor
  required, no `hostNetwork`, no `hostPath`.
- `sandbox-hardening-policy`, `EnsureExists` mode: you may edit it or delete
  its binding (`sandbox-hardening-binding`). The split exists from
  1.36.0-gke.2459000.

Required in the pod template: `runtimeClassName: gvisor`;
`automountServiceAccountToken: false`; `securityContext.runAsNonRoot: true`;
each container `capabilities.drop: ["ALL"]`; CPU and memory `resources.limits`;
`nodeSelector` `sandbox.gke.io/runtime: gvisor`; a toleration for
`sandbox.gke.io/runtime=gvisor:NoSchedule`.

Prohibited: `hostNetwork`/`hostPID`/`hostIPC`; `privileged: true`; hostPath
volumes; `capabilities.add`; `hostPort`; custom sysctls; "Projected volumes for
service account tokens or certificates."

**Network.** VERIFIED (ABOUT, ISOLATE): the add-on applies a default-deny
network posture to sandboxes; "To use Workload Identity Federation for GKE or
access other private resources, you must define custom network policies in the
SandboxTemplate." The session pods need internet egress and ingress from the
backend, so `deploy/` must state both.

**Price.** VERIFIED (ENABLE): "Agent Sandbox is offered at no extra charge in
GKE."

**GA or preview.** No Preview banner or launch-stage marker is on ENABLE,
ABOUT or ISOLATE (VERIFIED by searching the page HTML). Against that: every
enable command is `gcloud beta` (VERIFIED), and the API is `v1beta1`. Whether
it is covered by the GKE SLA is UNVERIFIED.

## 2. Provider support

Sources: provider docs at tag v8.5.0
([container_cluster](https://github.com/hashicorp/terraform-provider-google/blob/v8.5.0/website/docs/r/container_cluster.html.markdown)),
the provider [CHANGELOG](https://github.com/hashicorp/terraform-provider-google/blob/main/CHANGELOG.md),
and its `.changelog/` entries, read with `gh api`.

| Feature | Argument | Provider status |
|---|---|---|
| Agent Sandbox add-on | `google_container_cluster.addons_config.agent_sandbox_config.enabled` (Required bool) | VERIFIED. Added in 7.34.0 ("added `agent_sandbox_config` field", PR 27482); "promoted `agent_sandbox_config` addon field under `addons_config` … to GA" in 7.39.0 (PR 28017). |
| Pod Snapshots add-on | `google_container_cluster.addons_config.pod_snapshot_config.enabled` | VERIFIED. Beta first; "added `pod_snapshot_config` field to `google_container_cluster` resource (GA)" in 7.33.0. Docs: "The status of the Pod Snapshot addon. It is disabled by default. Set `enabled = true` to enable." |
| gVisor node pool | `node_config.sandbox_config.type = "GVISOR"` | VERIFIED. `sandbox_type` is the deprecated spelling. Docs: "you must specify `image_type = "COS_CONTAINERD"`". |

Latest release: `v8.5.0`, 2026-09-29 (VERIFIED, GitHub releases). The code pins
`~> 8.5`, and `tofu validate` accepts all three arguments against 8.5.0
(VERIFIED locally). `google-beta` is not needed.

UNVERIFIED, and only an apply can settle it: whether GKE accepts
`agent_sandbox_config.enabled = true` in the **create** call of a Standard
cluster. Google's procedure enables it after a gVisor pool exists, and OpenTofu
necessarily creates the cluster before its node pools. If creation is rejected,
apply once with `enable_agent_sandbox = false` and again with `true` (the
provider then issues the equivalent of `clusters update`). No `gcloud`
fallback is needed for any of the three features.

## 3. GKE Pod Snapshots

Sources: [About Pod snapshots](https://docs.cloud.google.com/kubernetes-engine/docs/concepts/pod-snapshots) (PS),
[Prepare for Pod snapshots](https://docs.cloud.google.com/kubernetes-engine/docs/how-to/pod-snapshots-prepare) (PREP),
[Restore from a Pod snapshot](https://docs.cloud.google.com/kubernetes-engine/docs/how-to/pod-snapshots) (RESTORE),
[PodSnapshot CRD reference](https://docs.cloud.google.com/kubernetes-engine/docs/reference/crds/podsnapshot) (CRD),
[Trigger Agent Sandbox snapshots from inside a cluster](https://docs.cloud.google.com/kubernetes-engine/docs/how-to/sandbox-trigger-inside-cluster) (INSIDE).
All VERIFIED unless marked.

**What it is.** A checkpoint of a running gVisor pod, stored in Cloud Storage:
process memory, threads, CPU registers, open file descriptors, the container
root filesystem, `emptyDir` and tmpfs mounts, listening and loopback and Unix
sockets. Not included: PersistentVolumeClaim contents and other volume types,
active external connections, custom routes and iptables rules. For this app:
the browser's memory and tabs come back from the snapshot, and the profile
comes back because the same 5 Gi disk is re-attached.

**Enabling.** Standard, new cluster:
`gcloud container clusters create … --enable-pod-snapshots --workload-pool=PROJECT_ID.svc.id.goog --workload-metadata=GKE_METADATA`,
version "1.35.3-gke.1234000 or later". Existing Standard cluster:
`gcloud beta container clusters update … --enable-pod-snapshots`. "Your cluster
must have Workload Identity Federation for GKE enabled." Pods "must run in GKE
Sandbox". Both Autopilot and Standard are supported.

**Bucket.** Required: hierarchical namespace ("must be enabled to allow for
higher read and write queries per second", which in turn needs uniform
bucket-level access); soft delete off ("soft deletions of the temporary objects
can increase your storage bill significantly"; snapshots use parallel composite
uploads); "the Cloud Storage bucket location must be the same location as the
GKE cluster". Google's command:
`gcloud storage buckets create … --uniform-bucket-level-access --enable-hierarchical-namespace --soft-delete-duration=0d --location=…`.
`infra/main/snapshots.tf` is that, plus public access prevention.

**IAM.**

- The pod's Kubernetes ServiceAccount (default `tokenSource: podKSA`):
  `roles/storage.bucketViewer` and `roles/storage.objectUser` on the bucket,
  granted to
  `principal://iam.googleapis.com/projects/NUMBER/locations/global/workloadIdentityPools/PROJECT.svc.id.goog/subject/ns/NAMESPACE/sa/KSA`.
  No Google service account and no key is involved, which is why the code
  creates none for snapshots.
- Or `tokenSource: federatedP4SA` (GKE 1.35.3-gke.1737000+): one grant of
  `roles/storage.admin` on the bucket to
  `service-NUMBER@gcp-sa-gkenode.iam.gserviceaccount.com`, which "creates
  short-lived tokens on-demand for specific paths". Avoids per-ServiceAccount
  bindings and their propagation delay ("IAM policies can take several minutes
  to populate in GKE").
- Always: `roles/storage.objectUser` to the GKE service agent
  `service-NUMBER@container-engine-robot.iam.gserviceaccount.com`, so the
  controller can delete snapshot objects. PREP grants it on the project with an
  IAM condition restricting it to the bucket; the Agent Sandbox tutorial grants
  it on the bucket. The code grants it on the bucket.
- Optional finer isolation: one managed folder per ServiceAccount with a
  custom role (`storage.objects.get/create/delete`, `storage.folders.create`).
  Not implemented: all sessions share one ServiceAccount and the pods never
  hold the credential themselves.

UNVERIFIED: that `podKSA` works for a pod with
`automountServiceAccountToken: false` (which the Agent Sandbox admission policy
demands). Google's own Agent Sandbox + snapshot tutorial sets
`serviceAccountName` and relies on `podKSA`, which suggests the node-side agent
obtains the token, but that tutorial does not run the managed add-on's
policies. If it fails, switch `snapshot_token_source` to `federatedP4SA`.

**Kubernetes resources.** API group `podsnapshot.gke.io/v1`:

- `PodSnapshotStorageConfig` (cluster-scoped in Google's examples):
  `spec.snapshotStorageConfig.gcs.{bucket,path,tokenSource}`.
- `PodSnapshotPolicy` (namespaced): `storageConfigName`, `selector.matchLabels`,
  `triggerConfig.type` (`manual` or `workload`), `triggerConfig.postCheckpoint`
  (e.g. `resume`), `snapshotScope` (`whole-pod` default, or `rootfs-only`),
  `retentionConfig.lastAccessTimeout`,
  `snapshotGroupingRules…maxSnapshotCountPerGroup`. "If not set, the Pod
  snapshot will always persist unless manually deleted."
- `PodSnapshotManualTrigger` (namespaced): names the pod to snapshot.
- `PodSnapshot` (namespaced): the result; status such as
  `AllSnapshotsAvailable`.

Restore is implicit: "you can delete the existing Pod after a snapshot is
taken, and then re-deploy the Pod… GKE automatically restores the Pod from the
matching snapshot."

**Limits and restore requirements.**

- `whole-pod` (needed here; `rootfs-only` loses memory): the new pod's
  "distilled spec hash" must be identical (containers, images, commands,
  volumes, security context…); "the target Pod must run on a node with an
  identical machine series and CPU architecture"; "the GKE Sandbox kernel
  version and GPU driver version" must match.
- "Pod snapshots don't support E2 machine types when using the default
  whole-pod snapshot scope."
- CPU features (INSIDE): "restoring a snapshot on a node with missing CPU
  features fails (for example, with the error `OCI runtime restore failed:
  incompatible FeatureSet`)"; Google's advice for production is "specifying a
  minimum CPU platform". Hence `session_min_cpu_platform`, which applies
  in every zone of `session_node_zones` (the pool spans the region because
  us-west1-a alone ran out of n2-standard-4 capacity).
- After restore: new pod IP and hostname, external connections closed, wall
  clock jumps, memory is streamed back lazily so the first seconds can be slow.
- No Cloud Storage FUSE sidecar, no TPUs, no MIG GPU sharing.
- Maximum snapshot size: none is documented (UNVERIFIED that there is none).
  Size follows the pod's memory use, so a 3 GiB browser means a snapshot of up
  to about that.
- ANALYSIS (follows from "kernel version must match", not stated outright): a
  node auto-upgrade that changes the gVisor version makes older snapshots
  unrestorable. Sessions then fall back to a cold start with Chromium's session
  restore, which the design already allows for.

**How the app uses it** (`deploy/gke/snapshots.yaml`, `docs/gke-deployment.md`).
Added when the feature was wired in:

- VERIFIED on the cluster (cluster-info workflow, GKE 1.36.4-gke.1247000):
  `podsnapshotstorageconfigs`, `podsnapshotpolicies`, `podsnapshots`,
  `podsnapshotmanualtriggers` and `podsnapshottokenrequests` are served at
  `podsnapshot.gke.io/v1`; a `pod-snapshot-agent` runs on the session nodes
  (namespace `gke-managed-pod-snapshots`); a
  `gke-pod-snapshot-validating-admission-policy` exists (contents not read
  yet); a session pod has the Sandbox's name and the label
  `agents.x-k8s.io/sandbox-name-hash` (FNV-1a of the name), and the Sandbox
  reports `status.nodeName`.
- Trigger and result (VERIFIED in the upstream Python client,
  `clients/python/agentic-sandbox-client/k8s_agent_sandbox/gke_extensions/snapshots/`,
  and [Trigger a Pod snapshot](https://docs.cloud.google.com/kubernetes-engine/docs/how-to/pod-snapshots-trigger)):
  a trigger is done when its `Triggered` condition is `True` with reason
  `Complete` and `status.snapshotCreated.name` names the PodSnapshot;
  `False` with reason `Failed` or `Error` is a failure. A restore only
  considers a PodSnapshot whose `Ready` condition is `True`. A restored pod
  has a `PodRestored` condition whose message names the snapshot.
- Restore choice (RESTORE): "By default, GKE restores workloads from the
  most recent PodSnapshot resource that matches the Pod"; with
  `snapshotGroupingRules`, "the restored Pod must have matching label keys
  and values". Upstream's per-session pattern, used here, groups by
  `agents.x-k8s.io/sandbox-name-hash`
  ([openclaw-fleet-gke/60-snapshots](https://github.com/kubernetes-sigs/agent-sandbox/tree/main/examples/openclaw-fleet-gke/60-snapshots)),
  and flips `operatingMode` to `Suspended` once the snapshot is Ready and
  back to `Running` to restore. The same example says a failed match
  "silently cold-starts" with no event. `podsnapshot.gke.io/ps-name` on a
  pod names one snapshot instead; not used.
- The distilled spec hash leaves out `nodeSelector`, most labels,
  environment and resources (PS), so the pool pin does not invalidate a
  snapshot. UNVERIFIED on the cluster.
- Deleting a PodSnapshot "also removes the files stored in Cloud Storage"
  (RESTORE).
- UNVERIFIED: whether PodSnapshotStorageConfig is cluster-scoped (the status
  script now prints each CRD's scope; the manifest works either way); time
  to snapshot and to restore; any size limit.

**GA or preview.** No Preview banner on PS, PREP or RESTORE (VERIFIED). ABOUT
says "Some underlying features, such as GKE Pod snapshots, might be in Preview
or have specific regional availability" (VERIFIED). Enabling it on an existing
Standard cluster is `gcloud beta` (VERIFIED). Availability in `us-west1`:
UNVERIFIED.

**Price.** No charge for the feature is documented; the Cloud Storage bucket is
billed normally. UNVERIFIED that there is no separate fee.

## 4. Node pools, scale to zero, machine types, Spot

**Why Standard.** ANALYSIS. The session pool has to (a) be N2 or similar, never
E2, (b) have a pinned CPU platform, (c) sit in one zone because session disks
are zonal, and (d) scale to zero. Standard gives all four directly and makes
the bill legible (nodes). Autopilot would need a custom ComputeClass for (a) to
(c) (Google's own advice in PREP) and bills per pod request, which for
bursty, mostly idle browsers is not obviously cheaper. Autopilot remains a
reasonable later move.

**Scale to zero.** `autoscaling { min_node_count = 0 }` on the gVisor pool is
ordinary cluster autoscaler behaviour; nothing in GVISOR forbids it, and the
"at least one node" rule applies to the non-gVisor pool (VERIFIED, GVISOR).
UNVERIFIED on a real cluster: scale-up from zero for a pod whose only match is
the gVisor label and taint. Expect about 1 to 2 minutes from a pending pod to a
ready node, plus the image pull, and about 10 minutes of idle before a node is
removed (`autoscaling_profile = "OPTIMIZE_UTILIZATION"` shortens it;
UNVERIFIED figures).

**Machine type.** Default `n2-standard-4` (4 vCPU billed, 2 usable under
gVisor on Intel, 16 GB): about four sessions at a 3 GiB limit each. N2 is the
family Google's snapshot tutorials use and names as what Agent Sandbox is
"optimized for". Alternatives:

- `n2-standard-8`: twice the node for twice the price; fewer scale-ups, worse
  for a lone session.
- `n2d-standard-4` (AMD): SMT stays on, so 4 usable CPUs, and about 13 % cheaper
  ($0.169/h). UNVERIFIED that whole-pod snapshots restore on N2D; the docs
  exclude only E2 and name n2 and c3 in their ComputeClass example.
- `c3-standard-4`: named in the docs' ComputeClass example; $0.2016/h, slightly
  dearer than N2 for the same shape.

Chromium renders in software here; expect one busy tab to use a full core.

**Spot vs on-demand.** A Spot N2 is about 40 % cheaper in us-west1 ($0.1165/h
vs $0.1942/h for n2-standard-4, source in section 7). Compute Engine can
reclaim it with about 30 seconds' notice (UNVERIFIED figure), which is not
enough to guarantee a fresh snapshot of a multi-GiB browser. The disk survives
and re-attaches (same zone), so the damage is "tabs reload", not "data lost".
Default is on-demand (`session_spot = false`): a browser someone is watching
should not vanish. Spot is sensible once preemption-triggered snapshots are
proven or if cost matters more than continuity.

**Persistent disks.** Zonal, `ReadWriteOnce`. A session's pod can only run in
the zone its disk was created in, so the pool is single-zone. Google's storage
guide for Agent Sandbox recommends Hyperdisk Balanced for private per-agent
workspaces, minimum 4 GiB, "several seconds" to attach (VERIFIED,
[Choose storage for AI agentic workloads](https://docs.cloud.google.com/kubernetes-engine/docs/concepts/machine-learning/agent-sandbox-storage)).
N2 supports both Hyperdisk Balanced and `pd-balanced` (UNVERIFIED for every N2
size); the StorageClass is a `deploy/` decision.

## 5. Exposing the app

**GatewayClasses.** VERIFIED
([Gateway API on GKE](https://docs.cloud.google.com/kubernetes-engine/docs/concepts/gateway-api)):
`gke-l7-global-external-managed` (global external Application Load Balancer)
and `gke-l7-regional-external-managed` (regional external ALB), both GA;
`gke-l7-gxlb` is the classic one.

**Certificates on a Gateway.** VERIFIED
([Secure a Gateway](https://docs.cloud.google.com/kubernetes-engine/docs/how-to/secure-gateway)):
a global Gateway takes a Certificate Manager certificate map through the
annotation `networking.gke.io/certmap`; a regional Gateway references
Certificate Manager certificates through the listener TLS option
`networking.gke.io/cert-manager-certs`.

**Wildcards.** VERIFIED
([DNS authorizations](https://docs.cloud.google.com/certificate-manager/docs/dns-authorizations)):
"If you're creating a DNS authorization for a wildcard certificate, such as
`*.myorg.example.com`, configure the DNS authorization for the parent domain".
The CNAME "must be the only resource record" at its name. UNVERIFIED but
standard: Google-managed certificates with load-balancer authorisation cannot
be wildcards, so DNS authorisation is the only route; and Certificate Manager
certificates can only be attached to Google's proxy load balancers, not
exported and not used by a passthrough load balancer.

**Timeouts on an ALB.** VERIFIED
([Configure Gateway resources using Policies](https://docs.cloud.google.com/kubernetes-engine/docs/how-to/configure-gateway-resources)):
`GCPBackendPolicy` (`networking.gke.io/v1`), `spec.default.timeoutSec`, default
30 seconds; `connectionDraining.drainingTimeoutSec` 0 to 3600. UNVERIFIED (from
memory of the load balancing docs): for a websocket the backend timeout is the
maximum lifetime of the connection, idle or not, and the ceiling is 86,400
seconds; for the Envoy-based ALBs it also bounds a streamed HTTP response.

**What Pomerium wants.** VERIFIED (Pomerium docs):

- [Kubernetes configure](https://www.pomerium.com/docs/deploy/k8s/configure.md):
  "The authenticate endpoint DNS address should resolve to an external IP
  address assigned by your Kubernetes Load Balancer to the `pomerium-proxy`
  service." Its stock Service is `type: LoadBalancer` on 443 and 80, and the
  `config/http3-gke` variant sets
  `loadBalancerClass: networking.gke.io/l4-regional-external`
  ([ingress-controller, branch 0-33-0](https://github.com/pomerium/ingress-controller/tree/0-33-0/config)).
- [Insecure Server](https://www.pomerium.com/docs/reference/insecure-server.md):
  running without TLS "can be useful in a situation where you have Pomerium
  behind a TLS terminating ingress or proxy. However, even in that case, it is
  highly recommended to use TLS…". Possible in Core, discouraged.
- Certificates: "Use cert-manager or other Kubernetes-native certificate
  solution"; `autocert` is not available in Kubernetes.
- Websocket and MCP routes have their Pomerium timeouts disabled
  automatically; other routes default to 30 s (the route config in `deploy/`
  already raises them).

`deploy/base` already runs Pomerium Core with `certificate_file`/
`certificate_key_file` from a `pomerium-tls` secret, i.e. terminating TLS
itself.

**Recommendation: `edge_mode = "pomerium_nlb"`.** A `Service` of
`type: LoadBalancer` gives Pomerium a regional external passthrough Network
Load Balancer on a reserved regional address; Pomerium terminates TLS with one
cert-manager certificate (Let's Encrypt, DNS-01 through Cloud DNS) covering the
three hosts and `*.sessions.browserjs.com`. Why:

1. It is the topology Pomerium documents and ships for GKE.
2. Pomerium is the OAuth authorization server for MCP clients and the
   websocket/SSE proxy. A second L7 proxy in front adds a second set of
   timeouts (30 s by default, a hard cap on websocket lifetime) and a second
   place for streaming to break, and buys nothing Pomerium does not already do.
3. TLS ends in one place, at the component that makes the access decision;
   HTTP/2 and client IPs reach it untouched.
4. One forwarding rule, no proxy-only subnet, no per-GB ALB data charge.

What it gives up: Cloud Armor / Google's L7 DDoS filtering, Google-managed
certificate renewal (cert-manager does it instead, and needs a Google service
account that can edit DNS records), and a global anycast address.

To reserve the address for the Service, VERIFIED
([LoadBalancer Service parameters](https://docs.cloud.google.com/kubernetes-engine/docs/concepts/service-load-balancer-parameters)):
annotation `networking.gke.io/load-balancer-ip-addresses: <address resource
name>` (GKE 1.29+) with `spec.loadBalancerClass:
networking.gke.io/l4-regional-external` (GKE 1.33.1-gke.1779000+), or the old
`spec.loadBalancerIP: <address>`. The address tier must match (Premium).

cert-manager with Workload Identity, VERIFIED
([cert-manager: Google CloudDNS](https://cert-manager.io/docs/configuration/acme/dns01/google/)):
a Google service account with `dns.resourceRecordSets.*`, `dns.changes.*`,
`dns.managedZones.list` (or `roles/dns.admin`), bound to
`serviceAccount:PROJECT.svc.id.goog[cert-manager/cert-manager]` with
`roles/iam.workloadIdentityUser`, and the annotation
`iam.gke.io/gcp-service-account` on cert-manager's ServiceAccount. The code
creates exactly that with a custom role.

**The alternative, `edge_mode = "gateway_alb"`,** is implemented for when Cloud
Armor or Google-managed certificates matter more: a global address, four DNS
authorisations with their CNAMEs, one certificate, a certificate map. `deploy/`
would then need a `Gateway` (`gke-l7-global-external-managed`), an `HTTPRoute`
to Pomerium, a `GCPBackendPolicy` with `timeoutSec: 86400`, a
`HealthCheckPolicy`, and HTTPS from the load balancer to Pomerium
(`appProtocol`/backend TLS) or `insecure_server`. The two modes exclude each
other: both need `_acme-challenge.sessions.browserjs.com`, one as a CNAME and
one as a TXT record.

## 6. Identity, registry, network policy

**Workload Identity Federation for GKE.** `workload_identity_config.workload_pool
= "PROJECT.svc.id.goog"` on the cluster and `workload_metadata_config.mode =
"GKE_METADATA"` on each pool: the same two settings as Google's
`--workload-pool` and `--workload-metadata=GKE_METADATA` (VERIFIED, PREP). Which
pods need Google access:

| Pod | Access | How |
|---|---|---|
| session pods | write snapshots | direct `principal://` grant on the bucket to the `session` ServiceAccount (or `federatedP4SA`) |
| cert-manager (pomerium_nlb) | edit DNS records | Google service account + `workloadIdentityUser` |
| backend, Pomerium, Dex | none | the backend creates `PodSnapshotManualTrigger` objects through the Kubernetes API only |

GKE Sandbox pods cannot reach the metadata server unless Workload Identity is
on (VERIFIED, GVISOR).

**Artifact Registry.** One Docker repository,
`us-west1-docker.pkg.dev/PROJECT/browserjs`, for `backend`, `browser` and
`mcp-js`. Nodes pull as the node service account, which gets
`roles/artifactregistry.reader` on that repository only, plus
`roles/container.defaultNodeServiceAccount` on the project (logs and metrics).
UNVERIFIED but standard GKE behaviour: no `imagePullSecrets` are needed for
Artifact Registry in the same project. Pushing is done by a person or CI with
`roles/artifactregistry.writer`; not managed here.

**NetworkPolicy.** `datapath_provider = "ADVANCED_DATAPATH"` is Dataplane V2,
which enforces Kubernetes NetworkPolicy without the Calico add-on (UNVERIFIED
here beyond the provider docs; it is long-standing GKE behaviour). It must be
chosen at cluster creation. `deploy/base/networkpolicy.yaml` then takes effect
as written.

## 7. Cost estimate

**An estimate, not a quote.** Prices change, taxes are excluded, and several
unit prices below are from memory. Sources: machine prices from
[gcloud-compute.com](https://gcloud-compute.com/n2-standard-4.html) (a
third-party mirror of Google's SKU list, "Last Update: Sun Sep 27 2026"; region
us-west1), VERIFIED against that page only. Google's own pricing pages render
their tables with JavaScript and could not be read by the tools used:
[GKE](https://cloud.google.com/kubernetes-engine/pricing),
[Compute](https://cloud.google.com/compute/all-pricing),
[network](https://cloud.google.com/vpc/network-pricing),
[DNS](https://cloud.google.com/dns/pricing),
[Storage](https://cloud.google.com/storage/pricing),
[Artifact Registry](https://cloud.google.com/artifact-registry/pricing). Every
figure not from gcloud-compute.com is UNVERIFIED.

At idle (no sessions running), per month of 730 hours, `pomerium_nlb`:

| Item | Unit price | Monthly |
|---|---|---|
| GKE management fee | $0.10/h | $73.00 |
| less the free tier (one zonal or Autopilot cluster per billing account) | $74.40 credit | −$73.00 |
| System node, 1 × e2-standard-2 | $0.067/h (verified) | $48.92 |
| its boot disk, 50 GB pd-balanced | $0.10/GB-month | $5.00 |
| Load balancer forwarding rule | $0.025/h | $18.25 |
| Cloud NAT: gateway for 1 VM + its address | $0.0014/h + $0.005/h | $4.67 |
| Cloud DNS zone | $0.20/zone | $0.20 |
| Pomerium's 1 GiB disk, registry beyond the free 0.5 GB, logs within the free 50 GiB | | about $1 |
| **Total** | | **about $78** |

Without the free-tier credit (a regional control plane, or the credit already
used by another cluster on the billing account): about **$151**. With
`gateway_alb` the forwarding rule costs the same and traffic adds about
$0.008/GB.

Kept per session, whether awake or asleep: 5 GiB disk, about $0.50 a month;
one snapshot of 2 to 3 GiB in Cloud Storage at $0.020/GB-month, about $0.05.

Per hour of use:

| Item | On-demand | Spot |
|---|---|---|
| n2-standard-4 node (verified) | $0.1942 | $0.1165 |
| 100 GB pd-balanced boot disk | $0.0137 | $0.0137 |
| Cloud NAT, per VM | $0.0014 | $0.0014 |
| **Per session-node-hour** | **about $0.21** | **about $0.13** |
| Per session-hour, 3 to 4 sessions on the node | $0.05 to $0.07 | $0.03 to $0.04 |

A lone session pays for the whole node, and a node lingers about 10 minutes
after its last session sleeps. Traffic: Cloud NAT charges about $0.045 per GB
in either direction for what the browsers fetch, and internet egress (the VNC
stream to the user, about $0.12/GB) applies on top. One person using one
session four hours a day comes to roughly $25 a month on top of idle.

The ceiling is `session_max_nodes` (default 3): three nodes running all month
are about $460.

## 8. Delivery: GitHub Actions with keyless authentication

OpenTofu runs only in GitHub Actions. `infra/bootstrap/bootstrap.sh` (run once
by a person) creates the project, the state bucket, a Workload Identity pool
and provider restricted to this repository, and two service accounts:
`tofu-plan` (Viewer, Security Reviewer, object admin on the state bucket; any
ref) and `tofu-apply` (Owner; `refs/heads/main` only). No key exists.

**Is the plan account's access enough?** For what `infra/main` defines, plan
refreshes: the project, enabled services, network, router, NAT, addresses,
cluster and node pools, service accounts and their IAM policies, the project
IAM policy, a custom role, the Artifact Registry repository and its IAM policy,
the DNS zone and records, Certificate Manager resources (one mode), and a
bucket with its IAM policy. Viewer covers the resource reads and Security
Reviewer the `getIamPolicy` calls (UNVERIFIED permission by permission; it is
what those roles are for). No secret is read. The one gap: VERIFIED
([IAM roles for Cloud Storage](https://docs.cloud.google.com/storage/docs/access-control/iam-roles)),
project Viewer's intrinsic Cloud Storage permissions are only
`storage.buckets.getIpFilter`, `storage.buckets.list` and two HMAC-key ones,
not `storage.buckets.get`. A bucket created with default settings also gets
the legacy "project viewers" binding, which includes it, so the plan may work
anyway; `roles/storage.bucketViewer` on the project for `tofu-plan` removes the
doubt, and `bootstrap.sh` grants it.

That same legacy binding has a side effect worth knowing: anyone with project
Viewer, `tofu-plan` included, can read the snapshot objects, which are browser
memory. ANALYSIS; check the bucket's IAM policy after the first apply.

**Limits of the gate.** ANALYSIS. The repository's GitHub plan has no
environment protection rules, so there is no required-reviewer gate. Apply is
started by hand (`workflow_dispatch` on `main`, with a typed confirmation) and
plans and applies in one job with no pause; the review happens on the pull
request's plan. Nothing technical stops a workflow on `main` from using
`tofu-apply`: Google hands it to any workflow on that ref. Who can write to
`main` is therefore the real control. `tofu-plan` can read and write the state bucket
from any branch of the repository, so everyone with push access can read
state.

## Unverified list

Things that cannot be checked without a real project, in the order they
matter:

1. A `Sandbox` suspended under the **managed** add-on is restored from a Pod
   Snapshot (Google's tutorial uses the open-source controller for this).
2. `agent_sandbox_config.enabled = true` is accepted when a Standard cluster is
   created, before any gVisor pool exists.
3. The `REGULAR` channel's default version is at least 1.36.3-gke.1767000.
4. Agent Sandbox and Pod Snapshots are available in `us-west1`, and their
   launch stage.
5. `podKSA` snapshot uploads work with `automountServiceAccountToken: false`.
6. The `gcp-sa-gkenode` service agent exists before the first node pool is
   created (only matters for `federatedP4SA`; an IAM grant to a principal that
   does not exist yet fails).
7. Headed Chromium under gVisor with all capabilities dropped and non-root;
   snapshot and restore of Chromium + Xvfb; snapshot size and restore time.
8. Scale-up from zero of the gVisor pool, and its latency.
9. `Intel Ice Lake` as minimum CPU platform is available for N2 in
   `us-west1-a`; quota for N2 CPUs and in-use addresses in a new project.
10. Whether an HNS bucket with `force_destroy` is emptied cleanly by the
    provider on destroy (folders).
11. `cluster_autoscaling { autoscaling_profile }` without node
    auto-provisioning is accepted as written (it validates; the API has not
    seen it).
12. All unit prices not marked verified in section 7.
13. That `tofu-plan`'s roles suffice for every refresh (section 8), and that
    newly enabled APIs are usable by the time the resources that follow them
    are created in the same apply.
