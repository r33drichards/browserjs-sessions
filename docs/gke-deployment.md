# Deploying to GKE

How `deploy/gke` gets onto the production cluster, in order, and what to look
at when a step fails. Written 2026-10-01 without access to the cluster or the
cloud account: nothing here has run. Claims are marked **VERIFIED** (read in
the cited source that day) or **UNVERIFIED**; the unverified ones are
collected at the end.

| | |
|---|---|
| App | `https://app.browserjs.com` |
| A session | `https://<id>.sessions.browserjs.com/mcp` |
| Pomerium's sign-in host | `https://authenticate.browserjs.com` |
| Dex | `https://dex.browserjs.com/dex` |
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
  `browserjs-com` with ambient credentials (on by default for a
  ClusterIssuer; VERIFIED
  <https://cert-manager.io/docs/configuration/acme/dns01/google/>).
- **`deploy/gke`** (kustomize, on `deploy/base`):
  - Pomerium's Service as a regional external passthrough Network Load
    Balancer on the reserved address: `type: LoadBalancer`,
    `loadBalancerClass: networking.gke.io/l4-regional-external`, annotation
    `networking.gke.io/load-balancer-ip-addresses: browserjs-edge` (the
    address resource's name). VERIFIED
    <https://docs.cloud.google.com/kubernetes-engine/docs/concepts/service-load-balancer-parameters>:
    the class is what selects the backend-service-based load balancer on GKE
    1.33.1 to 1.36, the annotation needs GKE 1.29 and that class, and the
    class cannot be changed on an existing Service.
  - One `Certificate` for `app`, `authenticate`, `dex` and
    `*.sessions.browserjs.com` into the Secret `pomerium-tls`.
  - Pomerium's and Dex's production configuration: the four session routes
    and the app route as locally, MCP settings as locally, the allow-list
    `rwendt1337@gmail.com` and `browserjs06@gmail.com`; Dex with the issuer
    `https://dex.browserjs.com/dex`, the Google and GitHub connectors and no
    passwords. Pomerium's databroker is on a 1 GiB Persistent Disk.
  - The backend with the production URLs and `ADMIN_EMAILS=rwendt1337@gmail.com`.
  - The session blueprint for GKE: `runtimeClassName: gvisor`, the gVisor
    node selector and toleration, `serviceAccountName: session`, no service
    account token, all capabilities dropped, CPU and memory limits, disks
    from the StorageClass below.
  - The `session` ServiceAccount, and the StorageClass `browserjs-zonal`
    (`pd.csi.storage.gke.io`, `pd-balanced`, `WaitForFirstConsumer`).
  - The NetworkPolicies of `deploy/base`, unchanged (see below).
- **`deploy/gke-staging-issuer`**: the same with the certificate asked of the
  staging issuer.

Not in the repository: the Secrets. The deploy workflow writes `dex-oauth`
from four repository secrets on every run, and generates `pomerium` (shared
secret, cookie secret, signing key, Dex client secret) once, the first time,
and never replaces it.

**Pod Snapshots are not used.** The add-on is enabled on the cluster and the
bucket exists, but nothing in `deploy/` creates a snapshot resource. A
stopped or sleeping session keeps its disk; Chromium restores its tabs from
it.

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

### 2. The browser runs as root, and the add-on's policy forbids that

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

Against that, `deploy/gke/blueprint.yaml`:

| Requirement | Blueprint |
|---|---|
| gVisor runtime class, node selector, toleration | yes |
| no service account token | yes |
| no host network, host path, privileged, added capabilities, host ports, sysctls | none used |
| drop all capabilities | yes, both containers (new for the browser) |
| CPU and memory limits | yes, both containers |
| run as non-root | **mcp-js yes (uid 1000); browser no** |

The browser image has no user but root, and its entrypoint needs one. Checked
locally on the current image (Docker, arm64, not gVisor):

- root with every capability dropped and `no-new-privileges`: starts, health
  check 200 in 3 s, VNC answers. So dropping capabilities costs nothing.
- uid 1000 with the image unchanged: the entrypoint dies at `chmod 1777 /tmp`.
- uid 1000 with that line made tolerant and `HOME` writable: openbox crashes
  (signal 11), because uid 1000 has no entry in `/etc/passwd`.
- uid 1000 with a passwd entry as well: starts, health check 200 in 2 s, VNC
  answers, all 21 processes run as uid 1000, clean stop.

Chromium keeps `--no-sandbox` either way; gVisor is the sandbox.

**The two ways forward** (a decision; the images were not changed here):

A. **Make the image non-root** (recommended; small). Exactly:
   - `images/browser/Dockerfile`: add `browser:x:1000:1000:browser:/home/browser:/bin/sh`
     to `/rootfs/etc/passwd` and `browser:x:1000:` to `/rootfs/etc/group`;
     `mkdir -p /rootfs/home/browser && chown 1000:1000 /rootfs/home/browser`;
     in the final stage `USER 1000:1000` and `ENV HOME=/home/browser`.
   - `images/browser/browser/entrypoint.sh`: `export HOME=/root` becomes
     `export HOME="${HOME:-/root}"`; `chmod 1777 /tmp /tmp/.X11-unix` gets
     `2>/dev/null || true` (the image's `/tmp` is already 1777).
   - `deploy/gke/blueprint.yaml`: uncomment `runAsNonRoot: true`,
     `runAsUser: 1000`, `runAsGroup: 1000` in the pod's `securityContext`
     (they are there, commented). The session disk already has `fsGroup: 1000`.
   - Railway's standalone use of the same image mounts a volume at `/data`
     that root owned so far; it would need its ownership changed once
     (UNVERIFIED, not part of this system).
   Then no policy is touched and the deploy runs with
   `hardening_exemption: leave`.
B. **Exempt the namespace from the hardening policy** (what the workflow
   offers for the first deployment with today's image): the input
   `hardening_exemption: exempt-namespace` adds a `namespaceSelector` to
   `sandbox-hardening-binding` so that it no longer matches Sandboxes in
   `browserjs-sessions`. The core policy still applies, and the blueprint
   still meets every hardening rule except non-root by its own choice.
   `restore` removes the selector again. This is narrower than Google's
   documented "delete the binding", which would lift the policy for the
   whole cluster. UNVERIFIED: the binding's current `matchResources` (the
   run prints it before and after), and that GKE leaves the edit in place.

On the current 1.35.8 cluster neither applies: there is no editable policy
before the split (UNVERIFIED which single policy exists there), so a
root pod would simply be refused. Another reason for finding 1.

## Runbook

Do these in order. Steps 1 to 4 can be done in any order among themselves.

### 1. OAuth callbacks

Dex's callback in production is `https://dex.browserjs.com/dex/callback`.

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
the script copies the two session images into `deploy/gke/blueprint.yaml`.
`hack/pin-images.sh --check` is the deploy's first step.

### 4. Infrastructure: pull request, apply, one variable

1. Check that the channel offers 1.36.3-gke.1767000 or later (command in
   finding 1).
2. Open the pull request for this branch. `infra plan` should show: three
   resources to add (`google_service_account.deployer`,
   `google_project_iam_member.deployer_cluster`,
   `google_service_account_iam_member.deployer_github`), one output, and
   `google_container_cluster.this` updated in place (`min_master_version`).
   Anything else, stop.
3. Merge. Start `infra apply` on `main`, confirm `apply`. Expect it to sit in
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
gh workflow run deploy.yml --ref main -f confirm=deploy -f issuer=staging \
  -f hardening_exemption=exempt-namespace     # "leave" once the browser image is non-root
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
gh workflow run deploy.yml --ref main -f confirm=deploy -f issuer=production -f hardening_exemption=leave
gh run watch
```

(`leave` does not undo the exemption made in step 6.) The Certificate is
issued again, the workflow sees that Pomerium still serves the old one and
restarts it, then the backend.

### 8. Smoke test

```sh
test/smoke.sh
```

19 checks, no sign-in: valid certificates for the four names (a random
`s-….sessions` host proves the wildcard), the redirect to
`authenticate.browserjs.com`, Pomerium's keys, Dex's issuer and its two
connectors and no password login, and that a session host answers the OAuth
metadata, 401 on `/mcp`, 404 on an upload to a session that does not exist
(no redirect to sign-in) and 426 on `/vnc`.

### 9. Sign in

Open <https://app.browserjs.com>, sign in with Google as
`rwendt1337@gmail.com`, create a session. The first one waits for the
`sessions` node pool to grow from zero (UNVERIFIED: a few minutes; the UI
shows "starting"). If it does not become running:
`gh workflow run cluster-info.yml --ref main -f sandbox=<session id>`.

Then: stop and resume it (tabs come back), let it idle 15 minutes (asleep,
wakes on an MCP call), connect an MCP client to
`https://<id>.sessions.browserjs.com/mcp`, delete it and see its disk go.

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
| | `Challenge` says the TXT record is not found | propagation; or the domain is not delegated to Cloud DNS: `dig +short NS browserjs.com`, `dig +short TXT _acme-challenge.sessions.browserjs.com` |
| | `rateLimited` | production only: wait, and use staging to debug |
| deploy: Apply | `spec.loadBalancerClass` is immutable | the Service was once applied without it: delete the Service by hand, run again |
| | `ValidatingAdmissionPolicy 'sandbox-…' denied` | only when a session is created, not at deploy: see below |
| deploy: Dex and Pomerium | Pomerium stays `ContainerCreating` | waiting for the Secret `pomerium-tls`: the certificate |
| | the Service's address is not the reserved one, or none | `describe service` in the output: the address resource's name, region or tier; `loadBalancerClass` missing; quota |
| | Dex `CrashLoopBackOff` | a secret missing in `dex-oauth` (step 2), or it cannot create its CRDs |
| deploy: Verdict | `does not serve agents.x-k8s.io/v1beta1` | finding 1 |
| smoke: certificate checks | `unable to get local issuer` | a staging certificate: step 7 |
| smoke: timeouts | nothing answers on 443 | forwarding rule or firewall: `describe service pomerium`; with `externalTrafficPolicy: Local` only Pomerium's node is healthy, which is intended |
| sign-in | Pomerium shows an error after Dex | Pomerium cannot fetch `https://dex.browserjs.com/dex` from inside the cluster (its log says so): see "in-cluster access to the public address" below |
| sign-in | Google or GitHub says the redirect URI is wrong | step 1 |
| sign-in | Pomerium's 403 page | the e-mail is not on the allow-list in `deploy/gke/pomerium-config.yaml` |
| the app | every API call 401 after sign-in | the backend has not got Pomerium's keys (its log: "Failed to refresh HTTP JWK Set"): same cause as two rows up; it retries every few minutes and on a restart |
| a session | creating fails with `sandbox-hardening-policy … denied` | the browser is root: finding 2, way A or B |
| | `sandbox-core-policy … denied` | the blueprint breaks a fixed rule: read the message, compare with the policy `cluster info` prints |
| | stays "starting" | `cluster info -f sandbox=<id>`: no node (the pool is scaling, or quota for N2), image pull (digest or the node account's reader role), a probe failing under gVisor |
| | runs, but the browser cannot reach sites | DNS: the `kube-dns` label in "Nodes"; the NetworkPolicy |

**In-cluster access to the public address.** Pomerium (to Dex) and the
backend (to Pomerium's keys) call `https://dex.browserjs.com` and
`https://app.browserjs.com`, which resolve to the load balancer's address.
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
- **The hardening exemption:** `-f hardening_exemption=restore`.
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
3. The admission policies' exact rules, names of the bindings, and that a
   `namespaceSelector` merged into `sandbox-hardening-binding` exempts the
   namespace and stays. Whether the policies accept a container-level
   `runAsNonRoot` (mcp-js) with a root container beside it does not matter
   under way B and is moot under way A.
4. Chromium, Xvfb and x11vnc under gVisor, with capabilities dropped (tested
   under Docker only). The x11vnc open-file-limit fix is in the image now but
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
12. No port 80: `http://app.browserjs.com` does not answer. Browsers try
    HTTPS first; add a second Service port and Pomerium's
    `http_redirect_addr` if that matters.

## Decisions needed

1. **Cluster version** (finding 1): upgrade to 1.36 now (recommended, and in
   this branch as its own commit), or keep 1.35 and have the backend support
   v1alpha1 (a real code change, not a setting).
2. **Root browser** (finding 2): change the image (A, recommended, folded
   into the rebuild from `main`) or exempt the namespace (B). B also works
   as a stopgap for the first deployment.
3. **GitHub OAuth App**: a second app for production, or move the one app.
4. **Kubernetes Engine Admin for the deployer** (above), or the narrower
   split.
5. **Let's Encrypt account e-mail**: `rwendt1337@gmail.com` in
   `deploy/gke/issuers.yaml`.
6. **`externalTrafficPolicy: Local`** (callers' addresses in Pomerium's log)
   or `Cluster` (one unknown fewer).
