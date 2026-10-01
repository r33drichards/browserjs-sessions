# Infrastructure (OpenTofu)

The Google Cloud side of browserjs sessions: a project, a GKE cluster with the
managed Agent Sandbox and Pod Snapshots add-ons, a gVisor node pool that scales
to zero, a registry, a snapshots bucket, a public address, DNS and what is
needed for certificates.

Why it is shaped this way, with sources: `docs/infrastructure.md`.

**Nothing here has been applied.** The code passes `tofu validate` and its
offline tests; it has never met a real project. Read the plan before applying,
and see "Not verified" at the end.

```
infra/
  bootstrap/   run once, local state: the project (optionally), its APIs,
               the state bucket, an optional budget
  main/        everything else, state in that bucket
```

Kubernetes objects (Pomerium, Dex, the backend, the Sandbox template, the
snapshot policy, cert-manager) are not managed here. They live in `deploy/`
and consume this configuration's outputs.

## Prerequisites

- OpenTofu 1.8 or later. In this repository: `nix shell nixpkgs#opentofu`.
- The Google Cloud CLI, signed in for OpenTofu to use:
  `gcloud auth application-default login`.
- A billing account you can attach projects to.
- Either an existing empty project with billing attached, or the right to
  create one (see `create_project` below).
- On the project: Owner, or the sum of Service Usage Admin, Kubernetes Engine
  Admin, Compute Network Admin, Storage Admin, DNS Admin, Artifact Registry
  Admin, Service Account Admin, Project IAM Admin, Role Administrator and
  Certificate Manager Editor.
- Quota in `us-west1` for N2 CPUs (4 per session node) and a few external
  addresses. New projects sometimes start with low limits.

## 1. Bootstrap

```sh
cd infra/bootstrap
cp terraform.tfvars.example terraform.tfvars
$EDITOR terraform.tfvars
```

Choose the project ID. IDs are global and permanent, so
`browserjs-sessions` itself is likely taken: use a suffix, for example
`browserjs-sessions-$(openssl rand -hex 2)`.

Then one of:

- **Use a project you made by hand** (`create_project = false`, the default).
  Create it in the console or with
  `gcloud projects create <id>` and
  `gcloud billing projects link <id> --billing-account <account>`.
- **Let OpenTofu create it** (`create_project = true`, with `billing_account`
  and, in an organisation, `org_id` or `folder_id`). You need
  `roles/resourcemanager.projectCreator` on the organisation or folder and
  `roles/billing.user` on the billing account. A created project is protected:
  `tofu destroy` will not delete it.

Optional: `budget_amount` adds e-mail alerts at 50 %, 90 % and 100 % of a
monthly amount. It needs `roles/billing.costsManager` on the billing account,
which project Owner does not include. A budget only notifies; it does not cap
spending. The real ceiling is `session_max_nodes` in `main`.

```sh
tofu init
tofu plan -out bootstrap.tfplan    # read it
tofu apply bootstrap.tfplan
tofu output                        # project_id, state_bucket
```

Bootstrap state is a local `terraform.tfstate`, which git ignores. Keep the
file, or move it into the bucket it just created:

```sh
cat > backend.tf <<'EOF'
terraform {
  backend "gcs" {
    prefix = "bootstrap"
  }
}
EOF
tofu init -migrate-state -backend-config="bucket=$(tofu output -raw state_bucket)"
```

(`backend.tf` is then worth committing.)

## 2. Main

```sh
cd ../main
cp terraform.tfvars.example terraform.tfvars
$EDITOR terraform.tfvars            # project_id from bootstrap
tofu init -backend-config="bucket=<state_bucket from bootstrap>"
tofu plan -out main.tfplan          # read it
tofu apply main.tfplan
tofu output
```

Expect about 15 minutes, most of it the cluster. If creating the cluster fails
on the Agent Sandbox add-on (Google's own procedure enables it after a gVisor
node pool exists), set `enable_agent_sandbox = false`, apply, set it back to
`true`, apply again.

Decisions to make in `terraform.tfvars`:

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

## 3. Point the domain at Cloud DNS (manual, once)

```sh
tofu output dns_name_servers
```

In Namecheap: Domain List, Manage `browserjs.com`, Nameservers, choose
**Custom DNS**, enter the four `ns-cloud-…googledomains.com` names (without
the trailing dot), save. Propagation takes minutes to a day. Check with:

```sh
dig +short NS browserjs.com
dig +short app.browserjs.com        # should print the edge_ip_address output
```

This replaces every record Namecheap served for the domain: mail (MX), any
existing site. Recreate what you still need in the Cloud DNS zone first.
Certificates cannot be issued until the delegation is live.

## 4. Hand over to `deploy/`

```sh
$(tofu output -raw get_credentials_command)
$(tofu output -raw docker_login_command)
```

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
cd infra/bootstrap   # and again in infra/main
tofu fmt -check -recursive
tofu init -backend=false
tofu validate
tofu test            # mocked provider; contacts nothing
```

## Destroying

`main` first, then (if ever) `bootstrap`.

```sh
cd infra/main
# 1. Remove what Kubernetes created in the project: delete the LoadBalancer
#    Service or Gateway, and the session Sandboxes with their disks
#    (kubectl delete -k deploy/…). Load balancers and disks made by the
#    cluster are not in OpenTofu's state and would be left behind, billing.
# 2. In terraform.tfvars: deletion_protection = false, and
#    snapshot_bucket_force_destroy = true if the bucket still holds snapshots.
tofu apply
tofu destroy
```

Then set Namecheap's nameservers back to "Namecheap BasicDNS" if the domain
should keep resolving.

`bootstrap` refuses to destroy the state bucket (`prevent_destroy`) and never
deletes a project it created. The simple way to remove everything is to delete
the project: `gcloud projects delete <id>` (recoverable for 30 days).

## Not covered

- Any Kubernetes object, including cert-manager, the Gateway or Service, the
  StorageClass for session disks, NetworkPolicies, and the PodSnapshot
  resources.
- OAuth client secrets (Google and GitHub for Dex) and Pomerium's secrets:
  created by hand, never in OpenTofu state.
- Registering the domain, and the Namecheap nameserver change.
- Mail or any other record for `browserjs.com` (the apex has no record).
- Pushing images, CI, and who may push (`roles/artifactregistry.writer`).
- Who may use `kubectl` (`roles/container.developer` or similar).
- Backups of session disks, alerting, uptime checks, Cloud Armor.
- More than one environment. A second one is a second project and a second
  pair of state prefixes.

## Not verified

`tofu validate` checks names and types against the provider, nothing more. The
full list is at the end of `docs/infrastructure.md`. The ones most likely to
need a change on the first real apply:

1. Whether the add-on is accepted at cluster creation (workaround above).
2. Whether `REGULAR` already ships a new enough GKE version.
3. Whether a Sandbox under the managed add-on restores from a Pod Snapshot;
   Google's tutorial for that still installs the open-source controller.
4. Quota for N2 in a new project, and `Intel Ice Lake` in the chosen zone.

The provider lock files (`.terraform.lock.hcl`) were generated on
`darwin_arm64`. On another platform run
`tofu providers lock -platform=linux_amd64 -platform=darwin_arm64`.
