# Infrastructure (OpenTofu)

The Google Cloud side of browserjs sessions: a project, a GKE cluster with the
managed Agent Sandbox and Pod Snapshots add-ons, a gVisor node pool that scales
to zero, a registry, a snapshots bucket, a public address, DNS and what is
needed for certificates.

Why it is shaped this way, with sources: `docs/infrastructure.md`.

**Nothing here has been applied.** The code passes `tofu validate` and its
offline tests; it has never met a real project. Read the first plan closely,
and see "Not verified" at the end.

```
infra/
  bootstrap/bootstrap.sh   run once by a person: the project, the state
                           bucket, and keyless access for GitHub Actions
  main/                    everything else, OpenTofu, run by GitHub Actions
.github/workflows/
  infra-plan.yml           on pull requests: shows the plan
  infra-apply.yml          started by hand on main: plans, then applies
```

`infra/billing` is a separate configuration with its own states and
workflows: what is sold in Stripe and what is metered in Metronome. It is
described in [`docs/billing-iac.md`](../docs/billing-iac.md), not here.

Kubernetes objects (Pomerium, Dex, the backend, the Sandbox template, the
snapshot policy, cert-manager) are not managed here. They live in `deploy/`
and consume this configuration's outputs.

OpenTofu is never run from a laptop against the real project. There are no
service account keys: GitHub Actions exchanges its OIDC token for a
short-lived Google credential (Workload Identity Federation).

| Service account | Rights | Usable from |
|---|---|---|
| `tofu-plan` | project Viewer and Security Reviewer; object admin on the state bucket (for the state lock) | any branch or pull request of this repository |
| `tofu-apply` | project Owner | `refs/heads/main` of this repository only |

## 1. Bootstrap (once, by hand)

**Already done** for the real deployment: project `browserjs-sessions`, state
bucket `browserjs-sessions-tofu-state`, and the six repository variables are
set on `r33drichards/browserjs-sessions`. This section is for a rebuild or a
second project.

In [Cloud Shell](https://shell.cloud.google.com), signed in as someone who may
create projects and link the billing account:

```sh
git clone https://github.com/r33drichards/browserjs-sessions && cd browserjs-sessions
ORG_ID=<numeric organisation id> ./infra/bootstrap/bootstrap.sh
```

It is safe to re-run. Settings are environment variables:

| Variable | Default | Note |
|---|---|---|
| `PROJECT_ID` | `browserjs-sessions` | Project IDs are global and permanent. If the name is taken the script stops at project creation; run it again with another ID |
| `ORG_ID` | unset | Organisation to create the project under. An account that belongs to an organisation must set it (`gcloud organizations list`); a personal account leaves it unset |
| `REGION` | `us-west1` | |
| `REPO` | `r33drichards/browserjs-sessions` | the only repository allowed to use the two service accounts |
| `BILLING` | the first open billing account | set it explicitly if you have more than one |

It creates the project, links billing, enables the base APIs, creates the
versioned state bucket `<project>-tofu-state`, the Workload Identity pool and
provider `github`, and the two service accounts. It also switches Cloud
Shell's active project. `infra/main` manages none of those.

In a project that is seconds old, IAM can answer `PERMISSION_DENIED` for a
minute; the script retries the Workload Identity steps for that reason. If it
still stops, wait a minute and run it again.

## 2. Repository variables

The script ends by printing six values. Set each as a GitHub Actions
**repository variable** (Settings, Secrets and variables, Actions, Variables;
they are identifiers, not secrets), or:

```sh
gh variable set GCP_PROJECT_ID    --body "<value>"
gh variable set GCP_REGION        --body "<value>"
gh variable set TOFU_STATE_BUCKET --body "<value>"
gh variable set GCP_WIF_PROVIDER  --body "<value>"
gh variable set TOFU_PLAN_SA      --body "<value>"
gh variable set TOFU_APPLY_SA     --body "<value>"
```

The `production` environment must exist (Settings, Environments); the apply
job names it. On this repository's GitHub plan an environment cannot require a
reviewer, so it gates nothing: the gate is the procedure in section 4.

Protect `main` as far as the plan allows, and keep the list of people with
write access short. Google hands `tofu-apply` (project Owner) to any workflow
that runs on `main`, so whoever can change `main` can change the project.

## 3. Plan: open a pull request

Settings that are not the project or region live in
`infra/main/terraform.tfvars`, which is tracked (it holds no secrets):

| Variable | Default | When to change |
|---|---|---|
| `cluster_location` | `us-west1-a` (zonal) | `us-west1` for a regional control plane: more available, about $73 a month more |
| `edge_mode` | `pomerium_nlb` | `gateway_alb` to terminate TLS in a Google load balancer with Certificate Manager |
| `release_channel`, `kubernetes_version` | `REGULAR`, unset | if `REGULAR` is older than 1.36.3-gke.1767000 |
| `session_machine_type` | `n2-standard-4` | bigger nodes; never E2 |
| `session_max_nodes` | 3 | the spending ceiling for sessions |
| `session_spot` | `false` | `true` for about 40 % cheaper nodes that can vanish |
| `snapshot_token_source` | `podKSA` | `federatedP4SA` if per-ServiceAccount access does not work |
| `master_authorized_cidrs` | none | only if you want `kubectl` over the public IP endpoint |

Any pull request that touches `infra/**` runs **infra plan**: format check,
`init`, `validate`, `plan`. The plan is in the run's summary. It changes
nothing. Pull requests from forks cannot authenticate and fail at that step,
by design.

The very first plan creates everything: expect 32 resources to add (default settings) and
none to change or destroy.

## 4. Apply: merge, then start it by hand

Nothing is applied by a merge. The steps, in order:

1. Read the plan in the pull request's **infra plan** run.
2. Merge to `main`.
3. Actions, **infra apply**, Run workflow, branch `main`, type `apply` in the
   confirm box.

The job refuses any other branch and any other confirm text. It then plans
again, prints that plan in the run summary, and applies exactly it, without a
pause. So apply soon after merging, and do not change the project by other
means in between: the plan you read is then the plan that runs. Applies never
overlap (one concurrency group). The outputs are printed in the run summary.

The first apply takes about 15 minutes, most of it the cluster. If creating
the cluster fails on the Agent Sandbox add-on (Google's own procedure enables
it after a gVisor node pool exists), set `enable_agent_sandbox = false` in
`terraform.tfvars`, merge and apply, then set it back to `true`, merge and
apply again.

## 5. Point the domain at Cloud DNS (manual, once)

Take `dns_name_servers` from the apply run's summary.

In Namecheap: Domain List, Manage `computeruse.site`, Nameservers, choose
**Custom DNS**, enter the four `ns-cloud-…googledomains.com` names (without
the trailing dot), save. Propagation takes minutes to a day. Check with:

```sh
dig +short NS computeruse.site
dig +short app.computeruse.site        # should print the edge_ip_address output
```

This replaces every record Namecheap served for the domain: mail (MX), any
existing site. Recreate what you still need in the Cloud DNS zone first.
Certificates cannot be issued until the delegation is live.

A domain listed in `additional_domains` gets a zone of its own with the same
records, and its own nameservers, in the output `additional_dns_name_servers`.
Nothing is served under it: it is how the deployment moves to another domain,
[docs/domain-switch.md](../docs/domain-switch.md). `previous_domain`
(`browserjs.com`) is the domain the deployment moved from: its zone and
records are kept, and removing the variable deletes them.

## 6. Image pushes (once)

After the apply that creates the `images-push` service account, set two more
repository variables from the apply run's outputs:

```sh
gh variable set IMAGE_PUSH_SA  --body "<images_push_service_account_email>"
gh variable set IMAGE_REGISTRY --body "<registry_url>"
```

`.github/workflows/images.yml` then builds and pushes the three images from
`main`. Until both are set, its push job fails at sign-in. See
`docs/build-pipeline.md`.

## 7. Deploys (once)

The same apply creates the `deployer` service account (Kubernetes Engine
Admin, usable only from `main`). Set its e-mail as a repository variable:

```sh
gh variable set DEPLOY_SA --body "<deployer_service_account_email>"
```

`.github/workflows/deploy.yml` and `cluster-info.yml` sign in with it.
Nothing changes in `bootstrap.sh`: `tofu-plan` can already read what this
adds. The deployment itself, and why the role is Admin: `docs/gke-deployment.md`.

## 8. Hand over to `deploy/`

`deploy/gke` already carries the values in the table below for the real
project; the table is what to change if the outputs ever differ.

Run the `get_credentials_command` and `docker_login_command` outputs on your
machine. Using `kubectl` and pushing images needs your own Google account to
hold `roles/container.developer` and `roles/artifactregistry.writer` on the
project (Owner covers both).

| Output | Where it goes |
|---|---|
| `registry_url` | image names: `<registry_url>/backend`, `/browser`, `/mcp-js` |
| `edge_ip_name` | `pomerium_nlb`: Pomerium's Service gets `type: LoadBalancer`, `loadBalancerClass: networking.gke.io/l4-regional-external` and the annotation `networking.gke.io/load-balancer-ip-addresses: <edge_ip_name>`. `gateway_alb`: the Gateway's `spec.addresses` (`type: NamedAddress`) |
| `certificate_dns_names`, `dns_zone_name`, `cert_manager_service_account_email` | `pomerium_nlb`: install cert-manager; annotate its ServiceAccount `iam.gke.io/gcp-service-account: <email>`; a ClusterIssuer with a `dns01.cloudDNS` solver for `project_id`; a Certificate for those names into the `pomerium-tls` secret |
| `certificate_map_name` | `gateway_alb`: the Gateway annotation `networking.gke.io/certmap` |
| `snapshot_bucket`, `snapshot_token_source` | `PodSnapshotStorageConfig` (`podsnapshot.gke.io/v1`) |
| `sessions_namespace`, `session_service_account` | the Sandbox template's `serviceAccountName`; that ServiceAccount must exist in that namespace |
| `session_node_selector` | the Sandbox template's `runtimeClassName`, `nodeSelector` and toleration |
| `hostnames` | Pomerium's routes and `authenticate_service_url`, Dex's issuer |

## Checking the code without an account

```sh
cd infra/main
tofu fmt -check -recursive
tofu init -backend=false
tofu validate
tofu test            # mocked provider; contacts nothing
```

(`nix shell nixpkgs#opentofu` provides `tofu`. The workflows pin 1.10.7.)

## Destroying

There is no destroy workflow, on purpose.

1. Remove what Kubernetes created in the project: delete the LoadBalancer
   Service or Gateway, and the session Sandboxes with their disks
   (`kubectl delete -k deploy/…`). Load balancers and disks made by the
   cluster are not in OpenTofu's state and would be left behind, billing.
2. Set Namecheap's nameservers back to "Namecheap BasicDNS" if the domain
   should keep resolving.
3. Delete the project: `gcloud projects delete <id>` (recoverable for 30
   days). That removes everything, including the state bucket and the
   Workload Identity pool. Then delete the six repository variables.

To remove only part of it, delete the resources from the code; the pull
request's plan shows the destroys before you merge and apply. The cluster needs
`deletion_protection = false` applied first, and the snapshots bucket
`snapshot_bucket_force_destroy = true` if it still holds snapshots.

## Not covered

- Any Kubernetes object, including cert-manager, the Gateway or Service, the
  StorageClass for session disks, NetworkPolicies, and the PodSnapshot
  resources.
- OAuth client secrets (Google and GitHub for Dex) and Pomerium's secrets:
  created by hand, never in OpenTofu state.
- Registering the domain, and the Namecheap nameserver change.
- Mail or any other record for `computeruse.site` (the apex has no record).
- Pushing images, CI, and who may push (`roles/artifactregistry.writer`).
- Who may use `kubectl` (`roles/container.developer` or similar).
- Backups of session disks, alerting, uptime checks, Cloud Armor.
- A billing budget or alert. Set one by hand in the console (Billing, Budgets
  & alerts); the ceiling this code enforces is `session_max_nodes`.
- More than one environment. A second one is a second project, bootstrapped
  the same way, and a second set of repository variables.

## Not verified

`tofu validate` checks names and types against the provider, nothing more. The
full list is at the end of `docs/infrastructure.md`. The ones most likely to
need a change on the first real apply:

1. Whether the add-on is accepted at cluster creation (workaround above).
2. Whether `REGULAR` already ships a new enough GKE version. It did not by
   default: the cluster came up on 1.35.8, where the add-on serves only the
   `v1alpha1` API. `terraform.tfvars` now asks for `"1.36"`; whether the
   channel offers 1.36.3-gke.1767000 or later is still to be checked
   (`gcloud container get-server-config`).
3. Whether a Sandbox under the managed add-on restores from a Pod Snapshot;
   Google's tutorial for that still installs the open-source controller.
4. Quota for N2 in a new project, and `Intel Ice Lake` in the chosen zone.

5. Whether `tofu-plan`'s read-only roles are enough for every refresh. Reading
   a bucket needs `storage.buckets.get`, which project Viewer does not
   include; `bootstrap.sh` now grants `roles/storage.bucketViewer` for that.
   A project bootstrapped before that line was added needs the script re-run
   (or that one grant made by hand).

The provider lock file (`infra/main/.terraform.lock.hcl`) was written on
macOS. It carries the registry's checksums for every platform, so `tofu init`
on the Linux runners verifies the provider against it.
