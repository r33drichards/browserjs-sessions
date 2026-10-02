# Metering and billing: deployment

What track B of the billing plan deploys, how it is switched on in stages,
and what has and has not been checked on a cluster. Design:
[plans/2026-10-02-metering-billing-design.md](plans/2026-10-02-metering-billing-design.md)
and, for where the meter and the credit live,
[plans/2026-10-02-metronome-integration.md](plans/2026-10-02-metronome-integration.md).
Contract: [contracts/billing/deploy.md](contracts/billing/deploy.md),
[contracts/billing/metronome.md](contracts/billing/metronome.md).

**On `main` billing is off**, in `deploy/gke` and in `deploy/local`.
Deploying `main` installs everything and runs nothing: the backend has no
`BILLING`, the operator has no pods, the export is suspended, and no Stripe
or Metronome secret is needed. Sessions behave exactly as before.

Usage and credit are kept by Metronome, not in the cluster. The cluster
keeps one resource, the `Account`; the billing operator is an observer that
sends seconds to Metronome.

## What is deployed

Everything is in the namespace `browserjs-sessions`.

| Object | File | Notes |
|---|---|---|
| CRD `accounts.browserjs.dev` (`v1alpha1`, no status subresource) | `deploy/base/crd-account.yaml` | byte for byte the contract's; CI compares. The superseded `Grant` and `UsagePeriod` CRDs are not deployed |
| ConfigMap `billing-catalogue` | `deploy/base/catalogue.yaml`, by `configMapGenerator` with no name hash | byte for byte the contract's; mounted whole (no `subPath`) in both pods, so a change arrives without a restart |
| Billing operator: ServiceAccount, Role, RoleBinding, ClusterRole and binding `browserjs-billing-operator`, Deployment (`Recreate`), no Service | `deploy/base/billing-operator.yaml` | **0 replicas and no `BILLING`** in the base; image `browserjs/billing-operator` (track A) |
| Backend: Role rules for `accounts` and the Lease; the billing, Stripe and Metronome variables; the catalogue beside the blueprint in `/etc/browserjs` | `deploy/base/backend.yaml` | `BILLING` is absent; no rule names `secrets` or `configmaps` |
| NetworkPolicy `billing-operator` | `deploy/base/networkpolicy.yaml` | no ingress; egress DNS, and TCP 443 and 6443 (the API server, Metronome) |
| Routes `api-stripe-webhook` (`/stripe/webhook`) and `api-metronome-webhook` (`/metronome/webhook`), on the API host, public | every `pomerium-config.yaml` | the request's signature is the credential; the backend serves each only while its feature is on |
| CronJob `billing-export`, its ServiceAccount, Role, RoleBinding, NetworkPolicy | `deploy/gke/billing-export.yaml` | **suspended** until the stage `enforce`; Accounts only |
| Bucket `<project>-billing-export`, and the right of that ServiceAccount to add objects to it | `infra/main/billing.tf` | versioned, 90 days; see "The export" for why it is here already |
| Secret `stripe` (`STRIPE_API_KEY`, `STRIPE_WEBHOOK_SECRET`), ConfigMap `billing-mode` (`STRIPE_MODE`), Secret `metronome` (`METRONOME_API_TOKEN`, `METRONOME_WEBHOOK_SECRET`) | not in the repository | written by the deploy workflow (`hack/billing-secrets.sh`), or absent; placeholders in `secrets.example.yaml` |

The backend's `/etc/browserjs` is now one projected volume of two
ConfigMaps (`session-blueprint`, `billing-catalogue`), which keeps
`BILLING_CATALOGUE` at the contract's default path.

The backend's variables are the contract's table with its defaults written
out, except three: `BILLING` (set only by the stage), and
`BILLING_EXEMPT_EMAILS` and `TERMS_VERSION`, which are left unnamed so that
the backend's own default applies (the value of `ADMIN_EMAILS`; no terms
asked). The backend takes both keys of the Secret `metronome`; the operator
takes the token only.

### Who may do what

| ServiceAccount | Rules |
|---|---|
| `backend` (added) | `accounts`: get, list, create, update, patch (no `watch`: there is no informer). Lease `billing-observer`: get |
| `billing-operator` | `sandboxes`: get, list, watch. Lease `billing-observer`: get, update; `leases`: create. `events`: create. ClusterRole: `customresourcedefinitions` list, watch. **Nothing on Accounts** |
| `billing-export` | `accounts`: get, list |

RBAC cannot hold a `create` to a name, so the operator may create a Lease
of any name in the namespace; it can read and update only its own.

### NetworkPolicy

| Pods | Ingress | Egress |
|---|---|---|
| `app: billing-operator` | none | DNS; TCP 443 and 6443 to any address |
| `app: billing-export` | none | the same, plus the node's metadata server (`169.254.169.254:80`, `169.254.169.252:988`) |

443 and 6443 to any address is the API server (which a NetworkPolicy cannot
name, as for the policy operator) and, on 443, Metronome's API for the
operator and Cloud Storage for the export.

## The stages

Two switches, because they are two kinds of thing.

**`hack/billing-stage.sh`** shows and sets the stage of an overlay (`gke`,
`local`). It only edits files; the change is reviewed, merged and deployed
like any other.

| Stage | Operator | Backend | Secret `metronome` | Export (GKE) | What a user notices |
|---|---|---|---|---|---|
| `off` (now) | 0 replicas, no `BILLING` | no `BILLING`: as before the feature | absent | suspended | nothing |
| `meter` | 1 replica, `BILLING=meter` | `BILLING=meter` | required | suspended | usage and charges are shown; nothing is refused, nothing is stopped |
| `enforce` | `BILLING=enforce` | `BILLING=enforce` | required | daily | no card, no session; at zero credit the sessions are drained and put to sleep |

What the script changes, and nothing else: two lines of the overlay's
`kustomization.yaml`.

- `off`: `- path: patch-billing-meter.yaml` and `- path:
  patch-billing-enforce.yaml` are both commented out.
- `meter`: the first is listed. It gives the operator its replica and sets
  `BILLING=meter` on the operator and the backend.
- `enforce`: both are listed. The second changes both values to `enforce`
  and un-suspends the export.

`BILLING` is set by those two patches and nowhere else, so the backend and
the operator cannot disagree; `--check` refuses a `BILLING` in the base or
in an overlay's `patch-backend.yaml`, `enforce` listed without `meter`, and
a missing patch file.

**The repository variable `STRIPE_MODE`** (`test`, `live`, or not set) is
payments, and it also chooses Metronome's environment: the sandbox while it
is not set or `test`, production when it is `live`. It is not in the files.

The deploy workflow's step "Billing secrets" (`hack/billing-secrets.sh`):

- **Stripe.** With `STRIPE_MODE` set it writes the Secret `stripe` and the
  ConfigMap `billing-mode` together from the secrets of that mode; not set,
  it deletes both.
- **Metronome.** With the stage of `deploy/gke` at `meter` or `enforce` it
  writes the Secret `metronome` from the secrets of the environment; at
  `off` it deletes it.
- The pods take every value with `optional: true` and read them at start,
  so the deploy restarts the backend when either changed and the operator
  when Metronome's did.

**A secret that is not needed is not looked at**: with billing off and no
`STRIPE_MODE`, none is, and a repository with no Stripe account and no
Metronome account deploys. A secret that is needed and empty stops the run
before it touches the cluster, naming the secret; so does a Stripe key of
the other mode (by its prefix: the backend would refuse to start) and a
webhook secret that is not a `whsec_`. No value is printed, by the script
or in an error.

What the deploy refuses (step "Billing stage and secrets", before it signs
in to the cluster):

| Files | `STRIPE_MODE` | |
|---|---|---|
| `off` | set | refused: the backend requires `BILLING` with `STRIPE_MODE`, and would not start |
| `meter` | not set, `test` | deployed if both `METRONOME_SANDBOX_` secrets are set (and with `test`, both `STRIPE_TEST_` secrets) |
| `enforce` | not set | refused: the backend refuses to enforce without Stripe |
| `enforce` | `test` | as `meter` with `test` |
| `meter`, `enforce` | `live` | deployed if both `METRONOME_PRODUCTION_` and both `STRIPE_LIVE_` secrets are set |
| `meter` or `enforce`, operator image not pinned | any | refused by `hack/pin-images.sh --check` |

The stages of the contract, in these terms: 1 is `meter`; 2 is `meter` with
`STRIPE_MODE=test`; 3 is `enforce`; 4 is `STRIPE_MODE=live`; 5 (open
sign-up) is track G's and is not switched here.

## Secrets and variables to create

Entered by the product owner in the repository's settings and nowhere else.
None is needed while billing is off and `STRIPE_MODE` is not set.

| Kind | Name | Needed from | Used by |
|---|---|---|---|
| secret | `METRONOME_SANDBOX_API_TOKEN` | stage 1 (`meter`): an API token of Metronome's sandbox | `deploy.yml` |
| secret | `METRONOME_SANDBOX_WEBHOOK_SECRET` | stage 1: the secret of the sandbox's webhook destination `https://api.computeruse.site/metronome/webhook` | `deploy.yml` |
| variable | `STRIPE_MODE` | `test` at stage 2, `live` at stage 4; not set before | `deploy.yml` |
| secret | `STRIPE_TEST_API_KEY` | stage 2: the sandbox's restricted key `backend` (`rk_test_...`) | `deploy.yml` |
| secret | `STRIPE_TEST_WEBHOOK_SECRET` | stage 2: the signing secret of the sandbox's webhook endpoint `https://api.computeruse.site/stripe/webhook` (`whsec_...`) | `deploy.yml` |
| secret | `STRIPE_LIVE_API_KEY`, `STRIPE_LIVE_WEBHOOK_SECRET` | stage 4 | `deploy.yml` |
| secret | `METRONOME_PRODUCTION_API_TOKEN`, `METRONOME_PRODUCTION_WEBHOOK_SECRET` | stage 4 | `deploy.yml` |

The deploy job runs in the environment `production`; a repository-level
variable and repository-level secrets reach it.

The objects in Stripe (products, prices, the portal's configuration) and in
Metronome (billable metrics, products, the rate card, the alert) are made
with OpenTofu from `infra/billing/`, by its own plan and apply workflows,
which have their own credentials. They are not part of this change, and
there is no `stripe-setup` or `metronome-setup` workflow here.

## Rolling out on production, from workflows only

Each step is a pull request made with the commands shown, merged, and then
the `deploy` workflow on `main` (confirm: `deploy`). `cluster info` is the
read-only workflow; its summary has a "Billing" section.

| Step | Before | Pull request | After `deploy`, in `cluster info` |
|---|---|---|---|
| 0. Install | nothing | this one | `billing-operator` WANTED 0. The CRD `established=True`. No `BILLING` on either Deployment. No ConfigMap `billing-mode`, no Secret `stripe` or `metronome`. CronJob `billing-export` SUSPENDED true. Pomerium and the backend restarted once (below). Sessions and the warm pool untouched |
| 1. Meter | tracks A and D merged; the `images` run on `main` has published `billing-operator`, and the pinned backend is one that knows `BILLING`; `infra/billing` applied to Metronome's sandbox (the rate card exists: the backend checks at start); the two `METRONOME_SANDBOX_` secrets | `hack/pin-images.sh billing-operator=sha256:… backend=sha256:…` and `hack/billing-stage.sh gke meter` | `billing-operator` READY 1 on the system node, no restarts. `BILLING=meter` on both. Secret `metronome` with two keys. Lease `billing-observer` RENEWED under two minutes ago. After a sign-in: an Account with a METRONOME customer |
| 2. Test payments | track C merged and pinned; `infra/billing` applied to Stripe's sandbox; the webhook endpoint and the two `STRIPE_TEST_` secrets | none: set the variable `STRIPE_MODE` to `test`, run `deploy` | ConfigMap `billing-mode` says `test`; Secret `stripe` has two keys; the backend restarted and is ready. In Stripe's Dashboard the webhook endpoint's deliveries are 200 |
| 3. Enforce | stage 2 checked by hand | `hack/billing-stage.sh gke enforce` | `BILLING=enforce` on both. CronJob SUSPENDED false; the day after, LAST-SUCCESS set and a job `Complete` |
| 4. Live payments | the product owner's steps for live mode; `infra/billing` applied to production of both; the `STRIPE_LIVE_` and `METRONOME_PRODUCTION_` secrets; the sandbox customer IDs cleared from the Accounts (below) | none: the variable `STRIPE_MODE` to `live`, run `deploy` | `billing-mode` says `live`; both Secrets changed; the backend and the operator restarted and are ready |

Rollback, each one `deploy` run:

- From 1: `hack/billing-stage.sh gke off`. The Secret `metronome` is
  removed; the Accounts stay, unread; what was sent to Metronome's sandbox
  stays there.
- From 2 or 4: delete the variable `STRIPE_MODE` (or set it back to
  `test`). The Secrets and the ConfigMap are removed or replaced and the
  pods restart. Not while the stage is `enforce` (refused): go back to
  `meter` in the same run.
- From 3: `hack/billing-stage.sh gke meter`. Nothing is refused any more;
  sessions put to sleep for credit stay asleep until their users wake them.
  The export is suspended again; what it wrote stays in the bucket.
- If the step "Billing operator" fails in the `deploy` run (it prints the
  Deployment and its log): nothing is refused because of it, in either
  stage. Usage is not sent while it is down, which is free time for the
  users; the backend reports metering as stale once the Lease is ten
  minutes old. Revert at leisure.

**What step 0 does to the running product.** Merging changes nothing in
the cluster. The next `deploy` restarts Pomerium once (its configuration
gained two routes, so its ConfigMap has a new name) and the backend once
(new variables, and the catalogue in its volume), as any deploy that
changes either does: a few seconds in which requests fail and screens
reconnect. No session pod, disk, snapshot or warm pod is touched, and the
backend behaves as before.

**Stage 4 and `metronomeCustomerId`: an open contract question.** The
contract's stage 4 says every Account's `metronomeCustomerId` and `credit`
are "cleared by the documented command" when the environment changes from
sandbox to production. The Account CRD's rule "metronomeCustomerId cannot
be changed once set" refuses exactly that (the kind job shows the refusal
of a removal). As the contracts stand the only way is to delete the
Accounts and let the backend make them again, which also loses the
recorded outcome of the sign-up credit. This needs a contract pull request
before stage 4; nothing here works around it.

## The catalogue

`deploy/base/catalogue.yaml` is a copy of `docs/contracts/billing/catalogue.yaml`
(kustomize cannot read a file outside its directory). A change of a credit
amount or a limit is a pull request to **both** files and a `deploy`: CI
fails when they differ. No image is built and no pod restarts; on kind the
change was in both pods within the time the summary of the `billing kind`
run gives (the kubelet's sync period, about a minute). A change of a price
or of a rate is also a change in `infra/billing`.

## The export, and restoring from it

From the stage `enforce`, daily at 03:17 UTC, the CronJob writes
`kubectl get accounts -o yaml` to
`gs://browserjs-sessions-billing-export/export/<year>/<month>/<time>.yaml`.
It authenticates as its own Kubernetes ServiceAccount (Workload Identity
Federation, no key), and may create objects there and nothing else: it can
neither read, replace nor delete an export. The token goes from the
metadata server to `curl` on stdin.

**What it is for now.** Usage and credit are in Metronome and payments in
Stripe; neither is lost with the cluster, and the record of which cards
earned the sign-up credit is Metronome's uniqueness keys. What a recreated
cluster would lose without the export is each Account's own state: the
card state as last read, `exempt`, `blocked`, the terms accepted, the
outcome of the sign-up credit, and the two customer IDs.

**Why the bucket is kept, and in this change.** The contract still has the
export from stage 3, and it is what makes a blocked account stay blocked
and an accepted-terms record survive a cluster. `infra/main` applies on
merge, so merging this creates one empty bucket and one IAM binding, and
nothing else: no cost until something is written, nothing reads it, and
the CronJob that writes it is suspended. Having it exist before the
`enforce` pull request means that pull request is a manifest change only.
If the export is later judged not worth having, `infra/main/billing.tf`
and `deploy/gke/billing-export.yaml` go together.

Restoring into a recreated cluster, by someone with read access to the
bucket and a kubeconfig:

1. Deploy `main` with billing **off** (`hack/billing-stage.sh gke off`), so
   that the CRD exists and nothing makes Accounts meanwhile.
2. `gcloud storage ls gs://browserjs-sessions-billing-export/export/**`,
   copy the latest file.
3. `hack/billing-restore.sh <file>`: applies every Account. It refuses to
   run while the operator has pods, and a file with anything but Accounts.
   Running it twice changes nothing.
4. The reconciles of `docs/contracts/billing/stripe.md` and `metronome.md`.
5. Billing back on.

This is the one procedure here that is not runnable from a workflow.

## Images

- **The billing operator** is the sixth image of `.github/workflows/images.yml`
  and of `hack/pin-images.sh`. Until `images/billing-operator/Dockerfile`
  exists the workflow skips it with a notice. It is built with
  `images/billing-operator` as the context, unless that directory has a
  `Dockerfile.dockerignore`, in which case it is built from the repository
  root with that file as `-f` (the policy operator's layout);
  `hack/local-up.sh` decides the same way. In `deploy/gke` it has the
  placeholder digest, which `hack/pin-images.sh --check` accepts only while
  billing is off there.
- **The export** runs `alpine/k8s:1.36.5` (kubectl, curl, jq, bash), named
  in `deploy/gke/billing-export.yaml` with tag and digest, read from Docker
  Hub's API on 2026-10-02 (an index with `linux/amd64` and `linux/arm64`).
  The tag is kubectl's version, the one the deploy workflow uses.

## Local

`deploy/local` is `off`. `hack/local-up.sh` builds and loads
`browserjs/billing-operator:dev` when `images/billing-operator/Dockerfile`
is in the tree. To meter locally: `hack/billing-stage.sh local meter`
(not committed) and the Secret `metronome` of
`deploy/base/secrets.example.yaml` with a developer's own sandbox token.
With Stripe: the Secret `stripe` and the ConfigMap `billing-mode` of the
same file; the procedure is track C's `docs/billing-development.md`.

## Checked on kind

`.github/workflows/billing-kind.yml`, on every pull request that touches
`deploy/`, the billing contracts or these scripts; its job summary lists
each refusal with the message, each RBAC answer and each connection.

Without a cluster:

- the two copies in `deploy/base` are the contract's files, byte for byte,
  and the superseded CRDs are not there;
- from whatever stage is committed, every stage (`off`, `meter`,
  `enforce`) of `gke` and `local` renders, with `BILLING`, the operator's
  replicas and the export's `suspend` as the table above says, and going
  back leaves the files as they were;
- the refusals of the table under "The stages", each one; an unpinned
  operator accepted with billing off and refused with it on;
- `hack/billing-secrets.sh --check`: nothing needed with billing off and no
  mode; Metronome's secrets required at `meter`, the sandbox's or
  production's by the mode; Stripe's by the mode; the other mode's or the
  other environment's secrets, and a key of the other mode, each refused,
  with no value in the output;
- the Go test of the API host's routes (`backend/internal/auth/deploy_test.go`).

On kind (`test/billing/run.sh`), with stand-ins for the backend and for the
operator (the placeholder image) that carry the real ServiceAccounts,
labels, volumes and variables:

1. The Account CRD is accepted, with no status subresource. Each rule
   refuses what its message says: an Account not named by its hash, a
   changed owner, a changed and a removed `stripeCustomerId`, a changed and
   a removed `metronomeCustomerId`, a changed `signupCredit`; and the
   allowed changes beside them (the card state, `credit`) are accepted.
2. RBAC, asked (`kubectl auth can-i`) and tried (requests as each
   ServiceAccount): the backend cannot watch or delete Accounts; the
   operator can do nothing with an Account and cannot read Secrets or
   ConfigMaps; the operator makes and renews the Lease `billing-observer`
   and the backend reads it; neither can get or update another Lease.
3. NetworkPolicy: the operator reaches the API server and DNS; not the
   backend (by Service or by pod address), not the internet on port 80 or
   53. The backend does not reach the operator's listener.
4. The catalogue in both pods is the contract's; a changed ConfigMap reaches
   both without a restart (same pods, no container restarted).
   `hack/billing-secrets.sh` with made-up values, through the stages in
   order: nothing needed and nothing made at `off`; each missing or
   wrong-mode secret stops it and makes nothing; at `meter` only the Secret
   `metronome`; with `test` the Secret `stripe` and the ConfigMap as well;
   restarted pods have the values in their environment (the backend all
   five, the operator the token only); with `live` the Secret `metronome`
   becomes production's; each change is reported once and not again; back
   at `off` all three are removed. Its output is searched for the values.
5. The export job, from `deploy/gke`'s own CronJob with no bucket, completes
   and prints every Account. `hack/billing-restore.sh` puts that file into a
   second, empty kind cluster: names, labels and spec are equal in the two,
   and a second restore changes nothing.

## Not yet checked: GKE

Look for these at the step named:

- Step 1: that the operator reaches the API server and
  `api.metronome.com` under Dataplane V2 through its NetworkPolicy (the
  Lease is renewed only after a tick Metronome accepted, so RENEWED says
  both).
- Steps 1 and 2: that Pomerium passes each webhook's body byte for byte
  and its signature header through (the contracts' open questions M9 and
  Stripe's; tracks D and C check them).
- Step 3: **the upload**. On kind the export is printed, not uploaded: the
  token from GKE's metadata server, the NetworkPolicy rules for that server
  under Dataplane V2, and the Cloud Storage request have run nowhere. After
  the first deploy at `enforce`, look the next day: `cluster info` shows
  the CronJob's LAST-SUCCESS and, with `--full`, the job's log (`exported N
  bytes to gs://…`). If it fails, the log says at which of the three;
  billing is not affected, the backup is missing.
- The export's image is pulled from Docker Hub by the system node.

## Deviations from the contract

1. **The stage is not in `patch-backend.yaml`.** The contract has `BILLING`
   set by "`patch-backend.yaml`, the operator's Deployment". It is set by
   `patch-billing-meter.yaml` and `patch-billing-enforce.yaml` of each
   overlay, listed by `hack/billing-stage.sh`, so that the two workloads
   change together and a script can check it (the pattern of session
   policies).
2. **The operator has 0 replicas in the base.** The contract's table says
   1; that is what `meter` makes it. Off, nothing is to run.
3. **`deploy.yml`**: the Secrets and the ConfigMap are made by a step of
   their own, "Billing secrets", directly after "Secrets", which runs
   `hack/billing-secrets.sh` (its own copy of the stdin helper), so that
   the kind job can run the very same code. Beyond the contract it checks
   the Stripe key's prefix against the mode, and the stage against the
   mode, before anything is changed; and at `off` it deletes the Secret
   `metronome`, as it does Stripe's without a mode.
4. **Every reference to the Secret `metronome` is `optional: true`**, as
   Stripe's are, so that the manifests apply with billing off and no
   Secret. The contract says "with `secretKeyRef`"; the backend itself
   refuses to start without the two once `BILLING` is set.
5. **No `stripe-setup.yml` and no `metronome-setup.yml`.** The product
   owner chose OpenTofu for the objects in Stripe and Metronome
   (`infra/billing/`, another change); the contract's two setup workflows
   are not built.
6. **The operator's Lease**: `create` on `leases` without a name (RBAC
   cannot name a create), `get` and `update` on `billing-observer` only.
7. **The export bucket's role is `roles/storage.objectCreator`**, and the
   export authenticates by the ServiceAccount's federated identity, with no
   Google service account. The contract says only "through Workload
   Identity".
8. **The operator's pod**: a read-only root filesystem with an `emptyDir`
   at `/tmp`, uid 65532 and `USER` in its environment, as the policy
   operator needed (kopf asks who it runs as). Track A's image has to run
   as a non-root uid with no entry in `/etc/passwd`.
9. **`backend/internal/auth/deploy_test.go`** (a test, not backend code)
   now expects five routes on the API host: the two more are
   `api-stripe-webhook` and `api-metronome-webhook`.
10. **`ZERO_BALANCE_DELETE_AFTER`** (`336h`) is written out beside
    `ZERO_BALANCE_DELETE`; the contract names it in that row's text.
11. `docs/build-pipeline.md` still says four images; it is not this track's.
