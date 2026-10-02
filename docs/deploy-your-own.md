# Deploy your own

`deploy/gke`, `infra/main/terraform.tfvars` and the workflows are the hosted
service (computeruse.site) as it runs. They are not a template: a handful of
values in them are ours, and are written out rather than templated because
several sit inside files kustomize cannot edit (a ConfigMap's file, a custom
resource). This page lists every one, so that a fork can replace them in one
commit. Try the system first on a local cluster
([local-development.md](local-development.md)): it needs none of this.

## What you need

A Google Cloud billing account, a registered domain, a fork of this
repository (the workflows are how OpenTofu and `kubectl` reach your project;
nothing is run from a laptop), and OAuth apps at Google and GitHub for
sign-in.

## The values that are ours

| Value | Ours | Where |
|---|---|---|
| Google Cloud project ID | `browserjs-sessions` | repository variable `GCP_PROJECT_ID`; written out in `deploy/gke/` (`kustomization.yaml` image names, `issuers.yaml`, `snapshots.yaml`, `blueprint.yaml`, `warmpool.yaml`, `cert-manager/kustomization.yaml`) and `hack/policy-stage.sh` |
| Image registry | `us-west1-docker.pkg.dev/browserjs-sessions/browserjs` | repository variable `IMAGE_REGISTRY`; the same files |
| Domain | `computeruse.site` | `infra/main/terraform.tfvars` (`domain`; delete `previous_domain`); every file in `deploy/gke/`; `.github/workflows/deploy.yml`, `session-public-url.yml`, `hack/session-public-url.sh`; the default in `web/src/billing/site.ts` |
| Cloud DNS zone name | `computeruse-site` | `deploy/gke/issuers.yaml` (the `dns_zone_name` output of `infra/main`) |
| Public address | `8.231.155.139` | `EDGE_IP` in `.github/workflows/deploy.yml` (the `edge_ip_address` output); documentation only elsewhere |
| Cluster name and location | `browserjs`, `us-west1-a` | `env:` of `deploy.yml`, `cluster-info.yml`, `session-public-url.yml`; `infra/main/terraform.tfvars` |
| Who may sign in | two e-mail addresses | `deploy/gke/pomerium-config.yaml` (the `&allowed` policy) and `ALLOWED_EMAILS` in `deploy/gke/patch-backend.yaml`: the two lists must be the same |
| Administrators | one e-mail address | `ADMIN_EMAILS` in `deploy/gke/patch-backend.yaml` |
| Let's Encrypt account e-mail | one e-mail address | `deploy/gke/issuers.yaml`, twice |
| Support address shown in the app | one e-mail address | the default in `web/src/billing/site.ts` |
| GitHub repository allowed into the project | repository ID `1400826306` | `REPO`, `REPO_ID` of `infra/bootstrap/bootstrap.sh`; `github_repository_id` in `infra/main/terraform.tfvars` |

To find what is left after editing:

```sh
git grep -n -e 'computeruse\.site' -e 'browserjs-sessions\.iam' -e 'pkg\.dev/browserjs-sessions' \
  -e 'project: browserjs-sessions' -e '8\.231\.155\.139' -e '@gmail\.com' -- deploy infra .github hack web/src
```

The Kubernetes namespace is also called `browserjs-sessions`, and so are
labels and the Terraform provider. Those are names inside the product, not
ours: leave them.

## Order

1. **Project and CI access.** [`infra/README.md`](../infra/README.md),
   sections 1 and 2: run `bootstrap.sh` once with your `PROJECT_ID` and
   `REPO`, set the repository variables it prints, create the `production`
   environment and give it a required reviewer.
2. **Infrastructure.** Edit `infra/main/terraform.tfvars`, open a pull request
   in your fork, read the plan, merge. Point your registrar at the nameservers
   in the `dns_name_servers` output. The outputs name everything step 4 needs.
3. **Images.** Set the variables `IMAGE_REGISTRY` and `IMAGE_PUSH_SA`
   ([build-pipeline.md](build-pipeline.md)); a push to `main` builds and
   pushes, and prints each digest.
4. **The deployment.** Replace the values above in `deploy/gke`, pin the
   digests (`hack/pin-images.sh`), add the OAuth apps' credentials as secrets,
   and run the `deploy` workflow: [gke-deployment.md](gke-deployment.md).

Billing (Stripe, Metronome) and session policies each have a stage switch and
start off; a deployment works without either
([policy-deployment.md](policy-deployment.md)).

## If your repository is public

Read "Running the workflows in a public repository" in
[`infra/README.md`](../infra/README.md) before the first apply.
