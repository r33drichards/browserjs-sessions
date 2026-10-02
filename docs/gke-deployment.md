# Deploying to GKE

How `deploy/gke` gets onto the production cluster, in order, and what to look
at when a step fails. Written 2026-10-01 without access to the cluster or the
cloud account: nothing here has run. Claims are marked **VERIFIED** (read in
the cited source that day) or **UNVERIFIED**; the unverified ones are
collected at the end.

| | |
|---|---|
| App | `https://app.computeruse.site` |
| A session | `https://sessions.computeruse.site/<id>/mcp` ([session-urls.md](session-urls.md); the older `https://<id>.sessions.computeruse.site/mcp` still answers) |
| Pomerium's sign-in host | `https://authenticate.computeruse.site` |
| Dex | `https://dex.computeruse.site/dex` |
| Address | `8.231.155.139` (the address resource `browserjs-edge`) |
| Cluster | `browserjs`, `us-west1-a`, project `browserjs-sessions` |
| Namespace | `browserjs-sessions` (cert-manager in `cert-manager`) |

## What is deployed

- **`deploy/gke/cert-manager`**: cert-manager v1.21.2 (supports Kubernetes
  1.33 to 1.36, VERIFIED <https://cert-manager.io/docs/releases/>), the
  release's static manifest kept in the repository, with one patch: its
  ServiceAccount is annotated for Workload Identity as
  `browserjs-cert-manager@browserjs-sessions.iam.gserviceaccount.com`.
  Vendored rather than applied from the release URL so that what reaches the
  cluster is what was reviewed, a deploy does not depend on a download, and
  the annotation is part of the manifest instead of a second step.
- **`deploy/gke/issuers.yaml`**: two `ClusterIssuer`s for Let's Encrypt,
  production and staging, both solving DNS-01 in the Cloud DNS zone
  `computeruse-site` with ambient credentials (on by default for a
  ClusterIssuer; VERIFIED
  <https://cert-manager.io/docs/configuration/acme/dns01/google/>).
- **`deploy/gke`** (kustomize, on `deploy/base`):
  - The public site on `https://computeruse.site` (`site.yaml`): one nginx pod
    serving the static build of `site/`, a Service, and a NetworkPolicy that
    lets only Pomerium in. Pomerium's `site` route needs no sign-in and
    passes no identity. The name is on the certificate, and its A record is
    in `infra/main/edge.tf`. `www.computeruse.site` redirects to it (301, same path and
    query): Pomerium's `www` route, its own A record and certificate name.
  - Pomerium's Service as a regional external passthrough Network Load
    Balancer on the reserved address: `type: LoadBalancer`,
    `loadBalancerClass: networking.gke.io/l4-regional-external`, annotation
    `networking.gke.io/load-balancer-ip-addresses: browserjs-edge` (the
    address resource's name). VERIFIED
    <https://docs.cloud.google.com/kubernetes-engine/docs/concepts/service-load-balancer-parameters>:
    the class is what selects the backend-service-based load balancer on GKE
    1.33.1 to 1.36, the annotation needs GKE 1.29 and that class, and the
    class cannot be changed on an existing Service.
  - One `Certificate` for `app`, `authenticate`, `dex`, `sessions` and
    `*.sessions.computeruse.site` (the old session hosts, deprecated) into the
    Secret `pomerium-tls`.
  - Pomerium's and Dex's production configuration: the three session routes
    on `sessions.computeruse.site`, the four deprecated ones on the old session
    hosts, and the app route as locally, MCP settings as locally, the allow-list
    `rwendt1337@gmail.com` and `browserjs06@gmail.com`; Dex with the issuer
    `https://dex.computeruse.site/dex`, the Google and GitHub connectors and no
    passwords. Pomerium's databroker is on a 1 GiB Persistent Disk.
  - The backend with the production URLs and `ADMIN_EMAILS=rwendt1337@gmail.com`.
  - The session blueprint for GKE: `runtimeClassName: gvisor`, the gVisor
    node selector and toleration, `serviceAccountName: session`, no service
    account token, all capabilities dropped, CPU and memory limits, disks
    from the StorageClass below.
  - The warm pool (`warmpool.yaml`, [warm-pool.md](warm-pool.md)): a
    `SandboxWarmPool` of one node's worth of sessions (seven) started ahead of time, over a
    `SandboxTemplate` that repeats the blueprint, and `WARM_POOL=s` on the
    backend.
  - The `session` ServiceAccount, and the StorageClass `browserjs-zonal`
    (`pd.csi.storage.gke.io`, `pd-balanced`, `WaitForFirstConsumer`).
  - The NetworkPolicies of `deploy/base`, unchanged (see below).
- **`deploy/gke-staging-issuer`**: the same with the certificate asked of the
  staging issuer.

Not in the repository: the Secrets. The deploy workflow writes `dex-oauth`
from four repository secrets on every run, and generates `pomerium` (shared
secret, cookie secret, signing key, Dex client secret) once, the first time,
and never replaces it.

**Pod Snapshots** (`snapshots.yaml`): an idle session sleeps to a snapshot of
its pod and wakes from it, browser as it was. Without a usable snapshot, and
after a stop, a session keeps its disk and Chromium restores its tabs from
it. See "Sleep and wake from Pod Snapshots" below.

### Network policy

The add-on's "default deny" is not applied to these sessions. It is a
feature of `SandboxTemplate` (`networkPolicyManagement: Managed`): the
controller makes one NetworkPolicy per template, selecting pods by a
template label (VERIFIED
<https://docs.cloud.google.com/kubernetes-engine/docs/how-to/agent-sandbox>,
"Network Policy restrictions", and the upstream description it links to,
<https://github.com/kubernetes-sigs/agent-sandbox/blob/v1.0.4/examples/policy/network-policy-management/README.md>).
The backend creates `Sandbox` objects directly, with no template, so the
only policy on a session pod is `session-pods` from `deploy/base`: ingress
from the backend on 6080 and 8080, egress to cluster DNS and to public
addresses. Should a managed policy ever select these pods as well,
NetworkPolicies add up, so both of ours keep working.

## The two findings that shape the procedure

### 1. The cluster must be on GKE 1.36.3 or later

The backend uses `agents.x-k8s.io/v1beta1` and suspends a session by writing
`spec.operatingMode`. The cluster is on 1.35.8-gke.1225000.

- VERIFIED
  <https://docs.cloud.google.com/kubernetes-engine/docs/how-to/how-install-agent-sandbox>:
  "Ensure that your cluster is running GKE version 1.36.3-gke.1767000 or
  later (supports the v1beta1 API)", and its migration section: before that
  version the managed add-on stores and serves `v1alpha1` with no conversion
  webhook; "Sandbox operating mode: inferred from replicas or state fields"
  in v1alpha1, "explicit value for the spec.operatingMode field" in v1beta1.
- VERIFIED in the upstream types (`api/v1alpha1/sandbox_types.go` at v0.4.6
  and v0.5.6, `api/v1beta1/sandbox_types.go` at v1.0.4, read with `gh api`):
  v1alpha1 has `podTemplate`, `volumeClaimTemplates`, `status.conditions` and
  `status.podIPs` like v1beta1, but no `operatingMode`: a sandbox is
  suspended with `spec.replicas: 0`. At v0.4.6 it has no `Suspended`
  condition either (only `Ready` and `Finished`).

So a `SANDBOX_API_VERSION` setting is **not** enough and was not written.
Against v1alpha1 the backend's `operatingMode` would be dropped as an unknown
field: sessions would start, and stop, sleep and wake would silently do
nothing. Supporting v1alpha1 would mean a second code path for five writes
and for reading the state back, for an API Google is migrating away from.

The change made instead: `infra/main/terraform.tfvars` sets
`kubernetes_version = "1.36"` (the variables already existed). Consequences:

- The control plane is upgraded in place. It is a zonal cluster with one
  control plane, so the Kubernetes API is unreachable for the duration
  (UNVERIFIED: typically some minutes); workloads keep running. There are no
  workloads yet, which makes now the cheapest moment.
- A control plane cannot be taken back to 1.35 afterwards.
- The node pools follow by auto-upgrade in the maintenance window (Tuesday to
  Thursday, 10:00 to 14:00 UTC), one surge node at a time. Nodes one minor
  version behind the control plane are supported, so nothing waits for them.
- UNVERIFIED: that the REGULAR channel offers a 1.36 at or above
  1.36.3-gke.1767000 today. Check before merging:

  ```sh
  gcloud container get-server-config --location us-west1-a --format='yaml(channels)'
  ```

  If REGULAR's `validVersions` has none, set `release_channel = "RAPID"` in
  the same file.

1.36 is needed for a second reason: only from 1.36.0-gke.2459000 is the
add-on's admission policy split into a fixed part and an editable part (next
section). The deploy workflow checks which versions the cluster serves and
fails, at the end, if `v1beta1` is not among them.

### 2. Sessions run as a non-root user, because the add-on's policy requires it

VERIFIED
<https://docs.cloud.google.com/kubernetes-engine/docs/how-to/agent-sandbox>,
"Sandbox security policies": the add-on enforces two
ValidatingAdmissionPolicies on `Sandbox` and `SandboxTemplate` objects.

| Policy | What it enforces | Can it be changed? |
|---|---|---|
| `sandbox-core-policy` | "require the use of gVisor, network isolation such as disabling hostNetwork, and file system isolation such as blocking hostPath" | No: `addonmanager.kubernetes.io/mode: Reconcile` |
| `sandbox-hardening-policy` (binding `sandbox-hardening-binding`) | "dropping all capabilities, preventing the addition of new capabilities, and requiring containers to run as non-root with resource limits" | Yes: `EnsureExists`. "you can modify the policy to remove specific constraints or delete the policy binding entirely", with the example "to allow containers to run as root" |

The split exists "in cluster version 1.36.0-gke.2459000 or later". Google's
sample template also marks as required: `runtimeClassName: gvisor`,
`automountServiceAccountToken: false`, `securityContext.runAsNonRoot: true`,
the gVisor node selector and toleration, `capabilities.drop: ["ALL"]` and a
memory limit. The exact expressions are not published; `cluster info` prints
both policies in full.

`deploy/gke/blueprint.yaml` meets all of it: the gVisor runtime class, node
selector and toleration; no service account token; no host network, host
path, privileged mode, added capabilities, host ports or sysctls; all
capabilities dropped and CPU and memory limits on both containers; and the
whole pod as uid and gid 1000 (`runAsNonRoot`, `runAsUser`, `runAsGroup`). No
policy is edited and no namespace is exempted. There is no `seccompProfile`:
GKE Sandbox does not support seccomp and the policies do not ask for one.

The browser image used to have no user but root. It now has the user
`browser` (uid 1000, home `/home/browser`) and an entrypoint that needs
nothing of root:

- `images/browser/Dockerfile`: a passwd and a group entry for uid 1000
  (openbox crashes for a uid that is not in `/etc/passwd`), and
  `/home/browser` owned by it.
- `images/browser/browser/entrypoint.sh`: `HOME` is `/root` for root and the
  user's home otherwise (with `/tmp/home` as the fallback for a uid the image
  does not know), instead of always `/root`; the `chmod` of `/tmp`, which
  only its owner may do, is allowed to fail; and an unwritable profile
  directory is reported as such instead of as a Chromium crash loop.
- The image still **defaults to root** (no `USER`): the standalone
  deployment on Railway mounts a root-owned volume at `/data`, and keeps
  working unchanged. A session pod asks for uid 1000 itself.

Checked on the local image with the new entrypoint and passwd files layered
on top (Docker, arm64; not gVisor, and not a rebuilt image), as
`--user 1000:1000 --cap-drop ALL --security-opt no-new-privileges` with a
fresh volume at `/data/chrome` owned `root:1000`, mode 2775, which is what
`fsGroup: 1000` leaves:

| Check | Result |
|---|---|
| starts | `/healthz` 200 after 1.1 s; every process is uid 1000; all capability sets zero, `NoNewPrivs: 1` |
| VNC | websocket upgrade 101, banner `RFB 003.008` |
| two named tabs | `default` at example.com, `two` at example.org |
| window fills the screen | outer 1279x799 on a 1280x800 screen, device pixel ratio 1 |
| `docker stop` | 0.6 s, exit 143; tab file and a session file written, owned by 1000 |
| restart | both pages restored, both names rebound, no extra tab |
| `docker kill`, restart | `exit_type` was `Crashed`; three pages restored and rebound; `exit_type` reset to `Normal` |
| volume not writable (root-owned, no group write) | exits with the new message naming the uid and its groups |
| as root, not in session mode, root-owned `/data` | Caddy `/healthz` 200; 401 without the password; `/vnc.html` 200 with it; stop under 1 s |

Xvfb prints "Owner of /tmp/.X11-unix should be set to root" as a non-root
user; it is a warning. Chromium keeps `--no-sandbox`; gVisor is the sandbox.

`deploy/base` and `deploy/local` are unchanged and still run the browser as
root: the local image predates this change (rebuilding it needs about 14 GB),
and the new image runs as root as well, so local keeps working either way.
Move the three `runAs*` lines to base when the local image is rebuilt.

On 1.35.8 the policy cannot be edited at all (the split is from
1.36.0-gke.2459000), which no longer matters for this deployment.

## Runbook

Do these in order. Steps 1 to 4 can be done in any order among themselves.

### 1. OAuth callbacks

Dex's callback in production is `https://dex.computeruse.site/dex/callback`.

- **Google** (console, the OAuth client, Authorized redirect URIs): add it
  beside `http://localhost:5556/dex/callback`. The app is in testing mode:
  `browserjs06@gmail.com` must be a listed test user to get through Google.
- **GitHub**: an OAuth App has exactly one "Authorization callback URL", and
  a `redirect_uri` on another host is refused (UNVERIFIED here, from GitHub's
  documented behaviour). So either register a **second OAuth App** for
  production (recommended: local sign-in keeps working, and the production
  secret is not on a laptop), or change the one app's callback and lose
  GitHub sign-in locally. A GitHub account is known to Dex by its primary
  verified e-mail, which has to be on the allow-list.

### 2. GitHub secrets

Never in a file or a command's arguments. From the Keychain items the local
setup uses (`gh secret set` reads the value from stdin):

```sh
cd ~/browserjs-sessions
for pair in google-client-id:DEX_GOOGLE_CLIENT_ID google-client-secret:DEX_GOOGLE_CLIENT_SECRET \
            github-client-id:DEX_GITHUB_CLIENT_ID github-client-secret:DEX_GITHUB_CLIENT_SECRET; do
  security find-generic-password -s "browserjs-sessions-${pair%%:*}" -w | gh secret set "${pair##*:}"
done
gh secret list
```

With a second GitHub OAuth App, set the two `DEX_GITHUB_*` secrets from that
app instead.

### 3. Images and their digests

After PR #3 is on `main` and the `images` workflow has pushed the three
images, pin them on this branch:

```sh
nix develop -c hack/pin-images.sh --registry main     # asks Artifact Registry; needs gcloud
# or, with the digests from the images run's summary:
nix develop -c hack/pin-images.sh backend=sha256:… browser=sha256:… mcp-js=sha256:…
git commit -am "deploy: pin the images"
```

The digests live in the `images:` block of `deploy/gke/kustomization.yaml`;
the script copies the two session images into `deploy/gke/blueprint.yaml`
and `deploy/gke/warmpool.yaml`.
`hack/pin-images.sh --check` is the deploy's first step.

The browser digest pinned must be of an image built after the non-root
change (finding 2): an older one has no uid 1000 and its sessions crash at
start. After that change merges and `images` has run, pin `browser` again
before deploying.

### 4. Infrastructure: pull request, apply, one variable

1. Check that the channel offers 1.36.3-gke.1767000 or later (command in
   finding 1).
2. Open the pull request for this branch. `infra plan` should show: three
   resources to add (`google_service_account.deployer`,
   `google_project_iam_member.deployer_cluster`,
   `google_service_account_iam_member.deployer_github`), one output, and
   `google_container_cluster.this` updated in place (`min_master_version`).
   Anything else, stop.
3. Merge. `infra apply` starts by itself on `main` (a merge that changes
   `infra/main` is the approval). Expect it to sit in
   the cluster update for the length of the control plane upgrade.
4. Set the variable, and check the version:

   ```sh
   gh variable set DEPLOY_SA --body "deployer@browserjs-sessions.iam.gserviceaccount.com"
   gcloud container clusters describe browserjs --location us-west1-a \
     --format='value(currentMasterVersion,currentNodeVersion)'
   ```

Nothing is needed in `infra/bootstrap/bootstrap.sh`: `tofu-plan`'s roles
already read service accounts and IAM policies, and `tofu-apply` is Owner.

### 5. Look before deploying

Run `cluster info` (Actions, or `gh workflow run cluster-info.yml --ref main`,
then `gh run watch`). In its summary check:

- "Agent Sandbox API": `sandboxes.agents.x-k8s.io  served=…v1beta1…`.
- "Agent Sandbox admission policies": `sandbox-core-policy` and
  `sandbox-hardening-policy` with their bindings.
  (Before the first deploy the account is not yet `cluster-admin` in the
  cluster; if this section shows "forbidden", the deploy's own summary has it.)
- "Nodes": the `system` node Ready; a `kube-dns` pod list that is not empty
  (the session NetworkPolicy allows DNS to pods labelled `k8s-app=kube-dns`).

### 6. Deploy with the staging issuer

```sh
gh workflow run deploy.yml --ref main -f confirm=deploy -f issuer=staging
gh run watch
```

This proves the parts that are expensive to get wrong against production's
rate limits: Workload Identity for cert-manager, DNS-01, the load balancer on
the reserved address. Expect a green run with a warning that the certificate
is a staging one. With it, browsers warn, Pomerium cannot reach Dex and the
backend cannot fetch Pomerium's keys (both go through the public names and
refuse the certificate): sign-in does not work yet. `INSECURE=1 test/smoke.sh`
should pass everything but the four certificate checks.

### 7. Deploy with the production issuer

```sh
gh workflow run deploy.yml --ref main -f confirm=deploy -f issuer=production
gh run watch
```

The Certificate is
issued again, the workflow sees that Pomerium still serves the old one and
restarts it, then the backend.

### 8. Smoke test

```sh
test/smoke.sh
```

30 checks, no sign-in: valid certificates for the five names (a random
`s-….sessions` host proves the wildcard), the redirect to
`authenticate.computeruse.site`, Pomerium's keys, Dex's issuer and its two
connectors and no password login; that on `sessions.computeruse.site` Pomerium
answers the OAuth metadata for a session, `/<id>/mcp` is 401 and points a
client at that metadata, an upload to a session that does not exist is 404
(no redirect to sign-in), `/<id>/vnc` is 426 and nothing else has a route;
and that an old session host still answers its metadata, 401 and 426
(`LEGACY=0 test/smoke.sh` once those hosts are retired).

### 9. Sign in

Open <https://app.computeruse.site>, sign in with Google as
`rwendt1337@gmail.com`, create a session. The first one waits for the
`sessions` node pool to grow from zero (UNVERIFIED: a few minutes; the UI
shows "starting"). If it does not become running:
`gh workflow run cluster-info.yml --ref main -f sandbox=<session id>`.

Then: stop and resume it (tabs come back), let it idle 15 minutes (asleep,
wakes on an MCP call; the test plan under "Sleep and wake from Pod
Snapshots"), connect an MCP client to
`https://sessions.computeruse.site/<id>/mcp` (the test plan in
[session-urls.md](session-urls.md)), delete it and see its disk go.

## Sleep and wake from Pod Snapshots

`deploy/gke/snapshots.yaml` and `SNAPSHOTS=true` on the backend. Sources and
what is verified: `docs/infrastructure.md`, section 3.

**Sleep** (a session idle for 15 minutes). The backend reads the node the pod
is on (`status.nodeName` of the Sandbox) and that node's `browserjs.com/pool`
label, creates a `PodSnapshotManualTrigger` for the pod, and waits for the
trigger to complete and for the `PodSnapshot` it names to be `Ready`. Then, in
one write, it sets `operatingMode: Suspended`, records the snapshot
(`browserjs.dev/snapshot`, `browserjs.dev/snapshot-pool`) and adds
`browserjs.com/pool: <pool>` to the pod template's `nodeSelector`. Any older
snapshot of the session is deleted. If the session was used while the
snapshot was taken, it stays up and the snapshot is deleted.

**A snapshot that fails or takes longer than `SNAPSHOT_TIMEOUT` (2m)** does
not stop the sleep: the session is suspended without a snapshot, as before
this feature, and every snapshot of it is deleted.

**Wake** (an MCP call, or the UI opening the session). If the recorded
snapshot is still there and `Ready`, the Sandbox is set to `Running` as it
is: the new pod lands on the snapshot's pool and GKE restores it, because it
is in the same snapshot group (the policy groups by the controller's
per-Sandbox pod label, so no session can wake from another's snapshot).
Otherwise the record and the pin are removed in the same write and the pod
cold starts on any pool, Chromium restoring its tabs from disk.

**A restore that does not work.** If the woken session is not running
`SNAPSHOT_RESTORE_TIMEOUT` (2m) after its pod got a node (or after the wake,
while it has none: no capacity in the pinned pool), or its pod fails, the
backend deletes the snapshot, removes the pin, suspends the Sandbox to remove
the pod, and runs it again: a cold start. `READY_TIMEOUT` is 5m so that one
request can outlast both attempts.

**Stop by the user** takes no snapshot and deletes the session's snapshots:
resume after a stop is a cold start, as before. **Delete** deletes the
session's snapshots first, then the Sandbox; GKE removes a deleted
PodSnapshot's objects from the bucket.

**Turning it off.** Set `SNAPSHOTS` to `"false"` and deploy: sessions sleep
and wake cold. Pins and snapshots already made stay as they are (a pinned
session still wakes from its snapshot, GKE does that on its own); to be rid
of them, also remove `snapshots.yaml` from the kustomization, delete the
PodSnapshots (`kubectl -n browserjs-sessions delete podsnapshots --all`) and
the policy.

### What is not known until it runs on the cluster

- **The disk is not in the snapshot.** The pod runs on for a moment after
  the checkpoint and Chromium exits cleanly, writing its profile. The
  restored browser's memory is from the checkpoint and its disk from a few
  seconds later. Chromium's databases may or may not take that well
  (UNVERIFIED). If they do not, the alternative is `postCheckpoint: stop` in
  the policy, whose effect on a Sandbox pod is itself UNVERIFIED.
- **A pod replaced while the session is awake** (node upgrade, eviction) is
  made again by the controller and would be restored from the snapshot of
  the last sleep, over a disk that has moved on. The snapshot is not deleted
  after a wake because it is UNVERIFIED that a restored pod no longer reads
  from it (memory is loaded in the background).
- `podKSA` uploads with `automountServiceAccountToken: false`; whether the
  managed Sandbox controller restores at all (Google's tutorial installs the
  open-source one); snapshot and restore times for a 1 to 3 GiB browser.
- Any change to the pod spec in `blueprint.yaml` (an image digest too) only
  reaches new sessions; an existing session keeps its own spec, so its
  snapshots stay valid. A node upgrade that changes the gVisor version makes
  them unrestorable; the fallback above then applies.

### Test plan on the cluster

After the deploy, with `gh workflow run cluster-info.yml --ref main` (add
`-f sandbox=<id>` for one session) and its "Pod Snapshots" section:

1. **Resources.** The storage config and policy are listed and `Ready`. In
   the backend's log: `idle sessions sleep to Pod Snapshots`.
2. **State to recognise.** Create a session. In its browser open a page with
   state that a reload loses: type in a text field without submitting, start
   a video and pause it mid-way, and run in the console
   `window.t0 = Date.now()`. Also `localStorage.setItem("k", "v")` on some
   site. Note the node and pool of the pod.
3. **Sleep.** Close the viewer and wait 15 to 17 minutes. Expect in the
   status: the Sandbox `Suspended`, `STOPPED-BY idle`, a `SNAPSHOT`, `POOL`
   equal to `PIN` equal to the pool noted; one PodSnapshot for the pod,
   `READY True`; no trigger left. Backend log: `session snapshotted`. A
   `GKEPodSnapshotting` event "Successfully checkpointed the pod". If the log
   says `snapshot failed; the session will wake cold`, the error after it is
   the finding (IAM on the bucket, the admission policy, the timeout).
4. **Wake.** Open the session in the UI and time it until the screen shows.
   Expect: the text field as typed, the video at its position, `window.t0`
   still defined (a cold start loses all three and only brings the tabs
   back), `localStorage.getItem("k")` is `"v"`. In the status: the pod on a
   node of the same pool, listed with `PodRestored=True` naming the
   snapshot. Compare the time with a cold start (about 45 s once scheduled).
5. **Twice.** Let it sleep and wake a second time: still one PodSnapshot,
   with a new name. Browse for a while after the wake and check that the
   sites' storage (step 2) is intact: this is the disk question above.
6. **Wake by MCP.** With the session asleep, make an MCP call to
   `https://sessions.computeruse.site/<id>/mcp`; it answers after the restore.
7. **Fallback.** With a session asleep, delete its snapshot by hand
   (`kubectl -n browserjs-sessions delete podsnapshot <name>`), then open
   it: it cold starts, tabs restored by Chromium, and the status shows no
   `SNAPSHOT` and no `PIN`.
8. **Stop and delete.** Stop a session that has a snapshot: the PodSnapshot
   goes. Let another sleep, then delete it: its Sandbox, disk and
   PodSnapshot go, and `gcloud storage ls gs://browserjs-sessions-pod-snapshots/sessions/`
   no longer lists it.
9. **Isolation.** With one session asleep (so a snapshot exists), create a
   new session: it must start with an empty browser, not the other's pages.

## When a step fails

Every failure ends with the status in the run's summary; `cluster info` gives
events, logs and the description of every pod that is not ready.

| Where | Symptom | Likely cause and what to do |
|---|---|---|
| deploy: sign-in to Google Cloud | `Unable to acquire impersonated credentials` or a 403 | `DEPLOY_SA` not set or wrong; step 4 not applied; the run is not on `main` |
| deploy: get credentials / first `kubectl` | forbidden or cannot connect | the DNS endpoint needs `container.clusters.connect` (in Kubernetes Engine Admin); `infra apply` still running the upgrade: wait |
| deploy: "Images are pinned" | `not pinned` or `disagree` | step 3 |
| deploy: "The deployer is cluster-admin" | forbidden | the IAM grant has not propagated (wait a minute, run again) |
| deploy: cert-manager | webhook does not answer | `cluster info`: are the three cert-manager pods running on the system node? On a private cluster the control plane reaches webhooks on port 10250, which this manifest uses (`--secure-port=10250`) and GKE's default firewall rule allows (UNVERIFIED) |
| deploy: Issuers | ClusterIssuer not Ready | cert-manager cannot reach Let's Encrypt (Cloud NAT), see cert-manager's log |
| deploy: Certificate, after 15 min | `Challenge` pending, log says 403 from `dns.googleapis.com` | Workload Identity: the annotation on `cert-manager/cert-manager`, the binding `browserjs-sessions.svc.id.goog[cert-manager/cert-manager]`, the custom DNS role |
| | `Challenge` says the TXT record is not found | propagation; or the domain is not delegated to Cloud DNS: `dig +short NS computeruse.site`, `dig +short TXT _acme-challenge.sessions.computeruse.site` |
| | `rateLimited` | production only: wait, and use staging to debug |
| deploy: Apply | `spec.loadBalancerClass` is immutable | the Service was once applied without it: delete the Service by hand, run again |
| | `ValidatingAdmissionPolicy 'sandbox-…' denied` | only when a session is created, not at deploy: see below |
| deploy: Dex and Pomerium | Pomerium stays `ContainerCreating` | waiting for the Secret `pomerium-tls`: the certificate |
| | the Service's address is not the reserved one, or none | `describe service` in the output: the address resource's name, region or tier; `loadBalancerClass` missing; quota |
| | Dex `CrashLoopBackOff` | a secret missing in `dex-oauth` (step 2), or it cannot create its CRDs |
| deploy: Verdict | `does not serve agents.x-k8s.io/v1beta1` | finding 1 |
| smoke: certificate checks | `unable to get local issuer` | a staging certificate: step 7 |
| smoke: timeouts | nothing answers on 443 | forwarding rule or firewall: `describe service pomerium`; with `externalTrafficPolicy: Local` only Pomerium's node is healthy, which is intended |
| sign-in | Pomerium shows an error after Dex | Pomerium cannot fetch `https://dex.computeruse.site/dex` from inside the cluster (its log says so): see "in-cluster access to the public address" below |
| sign-in | Google or GitHub says the redirect URI is wrong | step 1 |
| sign-in | Pomerium's 403 page | the e-mail is not on the allow-list in `deploy/gke/pomerium-config.yaml` |
| the app | every API call 401 after sign-in | the backend has not got Pomerium's keys (its log: "Failed to refresh HTTP JWK Set"): same cause as two rows up; it retries every few minutes and on a restart |
| a session | creating fails with `sandbox-hardening-policy … denied` | the blueprint or an image breaks a hardening rule (finding 2): the message names it; the browser image pinned must be one built after the non-root change |
| | `sandbox-core-policy … denied` | the blueprint breaks a fixed rule: read the message, compare with the policy `cluster info` prints |
| | stays "starting" | `cluster info -f sandbox=<id>`: no node (the pool is scaling, or quota for N2), image pull (digest or the node account's reader role), a probe failing under gVisor |
| | runs, but the browser cannot reach sites | DNS: the `kube-dns` label in "Nodes"; the NetworkPolicy |

**In-cluster access to the public address.** Pomerium (to Dex) and the
backend (to Pomerium's keys) call `https://dex.computeruse.site` and
`https://app.computeruse.site`, which resolve to the load balancer's address.
From inside a GKE cluster that address is served by the Service directly
(UNVERIFIED for this cluster, in particular with `externalTrafficPolicy:
Local` and a pod calling itself). If it does not work: first try
`externalTrafficPolicy: Cluster` in `deploy/gke/patch-pomerium.yaml`; then
give the two pods `hostAliases` for the three names pointing at a fixed
`clusterIP` set on Pomerium's Service (from `10.30.0.0/20`).

## Rollback

- **A bad deploy:** revert the commit on `main` (the image digests are part
  of it) and run `deploy` again. `kubectl apply` does not delete objects that
  left the manifests; nothing in this deployment relies on that yet.
- **Pomerium's secrets** are never touched by a deploy, so a rollback signs
  nobody out. Pomerium's disk and every session disk stay.
- **A bad certificate:** run with the other issuer; the previous Secret is
  replaced only when the new certificate is issued.
- **The cluster version** cannot be rolled back. Agent Sandbox objects did
  not exist before the upgrade, so no migration is involved.
- **Taking it offline:** there is no workflow for that. With a kubeconfig
  (`gcloud container clusters get-credentials browserjs --location us-west1-a
  --project browserjs-sessions --dns-endpoint`),
  `kubectl -n browserjs-sessions delete service pomerium` closes the edge and
  keeps everything else; `kubectl delete -k deploy/gke` deletes the namespace,
  with every session and its disk.

## Access

`infra/main` makes the Google service account `deployer`
(`deployer@browserjs-sessions.iam.gserviceaccount.com`, repository variable
`DEPLOY_SA`), usable only by this repository's workflows on `refs/heads/main`.
Its one role is `roles/container.admin`. VERIFIED
<https://docs.cloud.google.com/iam/docs/roles-permissions/container>:
`roles/container.developer` can create CustomResourceDefinitions, namespaces
and StorageClasses, but for `clusterRoles`, `clusterRoleBindings`, `roles`,
and validating and mutating webhook configurations it has `get` and `list`
only. The deploy creates all of those (Dex's ClusterRole, cert-manager's
RBAC and webhooks) and binds roles holding more than it has (`bind`,
`escalate`). The alternative, Developer plus a `cluster-admin`
ClusterRoleBinding, needs someone with Admin to create that binding first
and ends in the same power inside the cluster; what it would save is Admin's
project-level `container.clusters.update/delete`, against which the cluster
has deletion protection.

The workflow also binds the account to `cluster-admin` in the cluster's own
RBAC, so nothing depends on how GKE maps the IAM role onto kinds it has no
named permission for.

`cluster info` uses the same account because it is the only one GitHub
Actions has on the cluster. It runs `get`, `describe` and `logs` only.

## UNVERIFIED

1. REGULAR offers a GKE 1.36 at or above 1.36.3-gke.1767000; `"1.36"` as
   `min_master_version` is accepted and upgrades in place; how long it takes.
2. The managed add-on on 1.36 serves `agents.x-k8s.io/v1beta1` with the
   fields the backend uses, as upstream v1.0.x does (tested locally against
   upstream v1.0.4 only), and deletes a Sandbox's disk with it.
3. The admission policies' exact rules, and that the blueprint passes them
   (written from Google's description and sample, not from the policies'
   text; `cluster info` prints them).
4. Chromium, Xvfb, openbox and x11vnc under gVisor as uid 1000 with
   capabilities dropped (tested under Docker only, on files layered over the
   old image, not on the image CI builds); the session disk's `chrome`
   subPath being group-writable for gid 1000 on a Persistent Disk. The x11vnc open-file-limit fix is in the image now but
   was never run from a rebuilt image.
5. Pods reaching the load balancer's own address from inside the cluster
   (above).
6. cert-manager on this private cluster: the webhook reachable from the
   control plane; Workload Identity for the DNS-01 solver; the custom DNS
   role being enough.
7. Pomerium noticing a renewed certificate file without a restart
   (`config_hot_reload`). Renewal is 30 days before expiry; if it does not,
   run `deploy` (it compares the served certificate with the Secret and
   restarts Pomerium) at least every 60 days.
8. `google-github-actions/get-gke-credentials` v3.0.0 with
   `use_dns_based_endpoint` (the input exists, VERIFIED in its `action.yml`)
   working with the token `auth` produces, for the hour the job may take.
9. The session NetworkPolicy's DNS rule on this cluster (kube-dns pods
   labelled `k8s-app=kube-dns`; `cluster info` shows them).
10. The `sessions` pool scaling from zero for a Sandbox pod, and how long it
    takes; `browserjs-zonal` disks attaching under gVisor.
11. GitHub OAuth Apps allowing only one callback URL (step 1).
12. No port 80: `http://app.computeruse.site` does not answer. Browsers try
    HTTPS first; add a second Service port and Pomerium's
    `http_redirect_addr` if that matters.

## Decisions needed

1. (Decided: the cluster is upgraded to 1.36; finding 1.)
2. (Decided: the browser image is non-root; finding 2.)
3. **GitHub OAuth App**: a second app for production, or move the one app.
4. **Kubernetes Engine Admin for the deployer** (above), or the narrower
   split.
5. **Let's Encrypt account e-mail**: `rwendt1337@gmail.com` in
   `deploy/gke/issuers.yaml`.
6. **`externalTrafficPolicy: Local`** (callers' addresses in Pomerium's log)
   or `Cluster` (one unknown fewer).
