# Metering and billing: deployment

What track B of the billing plan deploys, how it is switched on in stages,
and what has and has not been checked on a cluster. Design:
[plans/2026-10-02-metering-billing-design.md](plans/2026-10-02-metering-billing-design.md).
Contract: [contracts/billing/deploy.md](contracts/billing/deploy.md).

**On `main` billing is off in `deploy/gke`.** Deploying `main` installs
everything and runs nothing: the backend has no `BILLING`, the operator has
no pods, the export is suspended, no Stripe secret is needed. Sessions behave
exactly as before. `deploy/local` meters.

## What is deployed

Everything is in the namespace `browserjs-sessions`.

| Object | File | Notes |
|---|---|---|
| CRDs `accounts`, `grants`, `usageperiods` (`browserjs.dev/v1alpha1`) | `deploy/base/crd-*.yaml` | byte for byte the contract's; CI compares |
| ConfigMap `billing-catalogue` | `deploy/base/catalogue.yaml`, by `configMapGenerator` with no name hash | byte for byte the contract's; mounted whole (no `subPath`) in both pods, so a change arrives without a restart |
| Billing operator: ServiceAccount, Role, RoleBinding, ClusterRole and binding `browserjs-billing-operator`, Deployment (`Recreate`), no Service | `deploy/base/billing-operator.yaml` | **0 replicas and no `BILLING`** in the base; image `browserjs/billing-operator` (track A) |
| Backend: Role rules for `accounts`, `grants`, `usageperiods`; the billing and Stripe variables; the catalogue beside the blueprint in `/etc/browserjs` | `deploy/base/backend.yaml` | `BILLING` is absent; no rule names `secrets` or `configmaps` |
| NetworkPolicy `billing-operator` | `deploy/base/networkpolicy.yaml` | no ingress; egress DNS, and TCP 443 and 6443 (the API server) |
| Route `api-stripe-webhook` (`/stripe/webhook` on the API host, public) | every `pomerium-config.yaml` | the backend serves it only while `STRIPE_MODE` is set |
| CronJob `billing-export`, its ServiceAccount, Role, RoleBinding, NetworkPolicy | `deploy/gke/billing-export.yaml` | **suspended** until the stage `enforce` |
| Bucket `<project>-billing-export`, and the right of that ServiceAccount to add objects to it | `infra/main/billing.tf` | versioned, 90 days; applied by "infra apply", before `enforce` |
| Secret `stripe` (`STRIPE_API_KEY`, `STRIPE_WEBHOOK_SECRET`), ConfigMap `billing-mode` (`STRIPE_MODE`) | not in the repository | written together by the deploy workflow (`hack/stripe-secret.sh`), or absent; placeholders in `secrets.example.yaml` |

The backend's `/etc/browserjs` is now one projected volume of two
ConfigMaps (`session-blueprint`, `billing-catalogue`), which keeps
`BILLING_CATALOGUE` at the contract's default path.

The backend's variables are the contract's table with its defaults written
out, except three: `BILLING` (set only by the stage), and
`BILLING_EXEMPT_EMAILS` and `TERMS_VERSION`, which are left unnamed so that
the backend's own default applies (the value of `ADMIN_EMAILS`; no terms
asked).

### Who may do what

| ServiceAccount | Rules |
|---|---|
| `backend` (added) | `accounts`: get, list, watch, create, update, patch. `grants`: get, list, watch, create, patch. `usageperiods`: get, list. Never `accounts/status` |
| `billing-operator` | `accounts`: get, list, watch. `accounts/status`: get, patch, update. `grants`: get, list, watch, delete. `grants/status`: patch. `usageperiods`: get, list, create, delete. `sandboxes`: get, list, watch. `events`: create. ClusterRole: `customresourcedefinitions` list, watch |
| `billing-export` | `accounts`, `grants`, `usageperiods`: get, list |

### NetworkPolicy

| Pods | Ingress | Egress |
|---|---|---|
| `app: billing-operator` | none | DNS; TCP 443 and 6443 to any address |
| `app: billing-export` | none | the same, plus the node's metadata server (`169.254.169.254:80`, `169.254.169.252:988`) |

The 443 and 6443 rule is for the API server, which a NetworkPolicy cannot
name (as for the policy operator, `docs/policy-deployment.md`); for the
export it is also Cloud Storage. It is wider than "the API server only":
the internet on 443 is not closed to the operator. Everything else is.

## The stages

Two switches, because they are two kinds of thing.

**`hack/billing-stage.sh`** shows and sets the stage of an overlay (`gke`,
`local`). It only edits files; the change is reviewed, merged and deployed
like any other.

| Stage | Operator | Backend | Export (GKE) | What a user notices |
|---|---|---|---|---|
| `off` (now) | 0 replicas, no `BILLING` | no `BILLING`: as before the feature | suspended | nothing |
| `meter` | 1 replica, `BILLING=meter` | `BILLING=meter` | suspended | usage and charges are shown; nothing is refused, nothing is stopped |
| `enforce` | `BILLING=enforce` | `BILLING=enforce` | daily | no card, no session; at zero credit the sessions are drained and put to sleep |

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
payments. It is not in the files: the deploy workflow reads it, and writes
the Secret `stripe` and the ConfigMap `billing-mode` together from the
secrets of that mode, or deletes both when it is not set
(`hack/stripe-secret.sh`). The backend takes all three values with
`optional: true` and is restarted by the deploy when they changed.

Stripe secrets are optional while `STRIPE_MODE` is not set: none is looked
at. When it is `test` or `live`, the run stops before touching the cluster,
naming the secret, if one of that mode's two is empty, if the API key is
not of that mode (by its prefix: the backend would refuse to start), or if
the webhook secret is not a `whsec_`. No value is printed, by the script or
in an error.

What the deploy refuses (step "Billing stage and Stripe", before it signs
in to the cluster):

| Files | `STRIPE_MODE` | |
|---|---|---|
| `off` | set | refused: the backend requires `BILLING` with `STRIPE_MODE`, and would not start |
| `meter` | not set, `test`, `live` | deployed |
| `enforce` | not set | refused: the backend refuses to enforce without Stripe |
| `enforce` | `test`, `live` | deployed |
| `meter` or `enforce`, operator image not pinned | any | refused by `hack/pin-images.sh --check` |

The stages of the contract, in these terms: 1 is `meter`; 2 is `meter` with
`STRIPE_MODE=test`; 3 is `enforce`; 4 is `STRIPE_MODE=live`; 5 (open
sign-up) is track G's and is not switched here.

## Secrets and variables to create

Entered by the product owner in the repository's settings and nowhere else.
All are optional until `STRIPE_MODE` is set.

| Kind | Name | When | Used by |
|---|---|---|---|
| variable | `STRIPE_MODE` | `test` at stage 2, `live` at stage 4; not set before | `deploy.yml` |
| secret | `STRIPE_TEST_API_KEY` | stage 2: the sandbox's restricted key `backend` (`rk_test_...`) | `deploy.yml` |
| secret | `STRIPE_TEST_WEBHOOK_SECRET` | stage 2: the signing secret of the sandbox's webhook endpoint `https://api.computeruse.site/stripe/webhook` (`whsec_...`) | `deploy.yml` |
| secret | `STRIPE_LIVE_API_KEY`, `STRIPE_LIVE_WEBHOOK_SECRET` | stage 4: the same two for live mode | `deploy.yml` |
| secret | `STRIPE_TEST_SETUP_KEY`, `STRIPE_LIVE_SETUP_KEY` | with the others | `stripe-setup.yml` (track C's workflow, not part of this change) |

The deploy job runs in the environment `production`; a repository-level
variable and repository-level secrets reach it.

## Rolling out on production, from workflows only

Each step is a pull request made with the commands shown, merged, and then
the `deploy` workflow on `main` (confirm: `deploy`). `cluster info` is the
read-only workflow; its summary has a "Billing" section.

| Step | Before | Pull request | After `deploy`, in `cluster info` |
|---|---|---|---|
| 0. Install | nothing | this one | `billing-operator` WANTED 0. The three CRDs `established=True`. No `BILLING` on either Deployment. No ConfigMap `billing-mode`, no Secret `stripe`. CronJob `billing-export` SUSPENDED true. Pomerium and the backend restarted once (below). Sessions and the warm pool untouched |
| 1. Meter | tracks A and D merged; the `images` run on `main` has published `billing-operator`, and the pinned backend is one that knows `BILLING` | `hack/pin-images.sh billing-operator=sha256:… backend=sha256:…` and `hack/billing-stage.sh gke meter` | `billing-operator` READY 1 on the system node, no restarts. `BILLING=meter` on both. After a session has been awake a few minutes: an Account a user, `OBSERVED` under two minutes old, `BURN-MICROS-PER-HOUR` not zero |
| 2. Test payments | track C merged and pinned; the product owner's steps 1 to 6 of the design's section 12 (the sandbox, the two keys, "stripe setup", the webhook endpoint) | none: set the variable `STRIPE_MODE` to `test`, run `deploy` | ConfigMap `billing-mode` says `test`; Secret `stripe` has two keys; the backend restarted and is ready. In Stripe's Dashboard the webhook endpoint's deliveries are 200 |
| 3. Enforce | stage 2 checked by hand; "infra apply" has made the export bucket (`billing_export_bucket` in its outputs) | `hack/billing-stage.sh gke enforce` | `BILLING=enforce` on both. CronJob SUSPENDED false; the day after, LAST-SUCCESS set and a job `Complete` |
| 4. Live payments | the product owner's steps 11 to 14 | none: the variable `STRIPE_MODE` to `live`, run `deploy` | `billing-mode` says `live`; the backend restarted and is ready |

Rollback, each one `deploy` run:

- From 1: `hack/billing-stage.sh gke off`. The Accounts and their ledgers
  stay, unread.
- From 2 or 4: delete the variable `STRIPE_MODE` (or set it back to
  `test`). The Secret and the ConfigMap are removed or replaced together and
  the backend restarts. Not while the stage is `enforce` (refused): go back
  to `meter` in the same run.
- From 3: `hack/billing-stage.sh gke meter`. Nothing is refused any more;
  sessions put to sleep for credit stay asleep until their users wake them.
  The export is suspended again; what it wrote stays in the bucket.
- If the step "Billing operator" fails in the `deploy` run (it prints the
  Deployment and its log): in `meter` nothing depends on the pod, so revert
  at leisure. In `enforce` the ledger goes stale after
  `BILLING_STALE_AFTER`: accounts that had credit carry on, the others are
  refused with `metering_unavailable` (`enforcement.md`); revert to `meter`.

**What step 0 does to the running product.** Merging changes nothing. The
next `deploy` restarts Pomerium once (its configuration gained a route, so
its ConfigMap has a new name) and the backend once (new variables, and the
catalogue in its volume), as any deploy that changes either does: a few
seconds in which requests fail and screens reconnect. No session pod, disk,
snapshot or warm pod is touched, and the backend behaves as before.

## The catalogue

`deploy/base/catalogue.yaml` is a copy of `docs/contracts/billing/catalogue.yaml`
(kustomize cannot read a file outside its directory). A change of a credit
amount, a limit or a rate is a pull request to **both** files and a
`deploy`: CI fails when they differ. No image is built and no pod restarts;
on kind the change was in both pods within the time the summary of the
`billing kind` run gives (the kubelet's sync period, about a minute). A
change of a price also needs the Stripe setup workflow.

## The export, and restoring from it

From the stage `enforce`, daily at 03:17 UTC, the CronJob writes
`kubectl get accounts,grants,usageperiods -o yaml` to
`gs://browserjs-sessions-billing-export/export/<year>/<month>/<time>.yaml`.
It authenticates as its own Kubernetes ServiceAccount (Workload Identity
Federation, no key), and may create objects there and nothing else: it can
neither read, replace nor delete an export. The token goes from the
metadata server to `curl` on stdin.

Restoring into a recreated cluster, by someone with read access to the
bucket and a kubeconfig:

1. Deploy `main` with billing **off** (`hack/billing-stage.sh gke off`), so
   that the CRDs exist and the operator has no pods.
2. `gcloud storage ls gs://browserjs-sessions-billing-export/export/**`,
   copy the latest file.
3. `hack/billing-restore.sh <file>`: applies every object, then writes each
   one's status (a subresource, which `apply` does not write; for an
   Account it is the ledger itself). It refuses to run while the operator
   has pods. Running it twice changes nothing.
4. The Stripe reconcile of `docs/contracts/billing/stripe.md`.
5. Billing back on.

This is the one procedure here that is not runnable from a workflow. What
was used between the last export and the loss is not charged.

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

`deploy/local` is at `meter`. `hack/local-up.sh` builds and loads
`browserjs/billing-operator:dev` when `images/billing-operator/Dockerfile`
is in the tree; until then it leaves the operator without pods and says so.
With Stripe: track C's `docs/billing-development.md`; the Secret and
ConfigMap of `deploy/base/secrets.example.yaml`, with a developer's own
sandbox key.

## Checked on kind

`.github/workflows/billing-kind.yml`, on every pull request that touches
`deploy/`, the billing contracts or these scripts; its job summary lists
each refusal with the message, each RBAC answer and each connection.

Without a cluster:

- the four copies in `deploy/base` are the contract's files, byte for byte;
- from whatever stage is committed, every stage (`off`, `meter`,
  `enforce`) of `gke` and `local` renders, with `BILLING`, the operator's
  replicas and the export's `suspend` as the table above says, and going
  back leaves the files as they were;
- the refusals of the table under "The stages", each one; an unpinned
  operator accepted with billing off and refused with it on;
- `hack/stripe-secret.sh --check`: nothing needed without the mode; a
  missing secret, the other mode's secrets, a key of the other mode each
  refused, with no value in the output;
- the Go test of the API host's routes (`backend/internal/auth/deploy_test.go`).

On kind (`test/billing/run.sh`), with stand-ins for the backend and for the
operator (the placeholder image) that carry the real ServiceAccounts,
labels, volumes and variables:

1. The three CRDs are accepted. Each rule refuses what its message says: an
   Account not named by its hash, a changed owner, a changed and a removed
   `stripeCustomerId`, a changed `signupCredit`, a changed Grant, an
   un-revoked Grant, a non-admin Grant with no expiry, a changed
   UsagePeriod; and the allowed changes beside them are accepted.
2. RBAC, asked (`kubectl auth can-i`) and tried (requests as each
   ServiceAccount): the backend cannot patch `accounts/status`; the operator
   cannot patch an Account's spec, cannot create a Grant, cannot read
   Secrets or ConfigMaps; the operator can patch the status, and that leaves
   the spec alone.
3. NetworkPolicy: the operator reaches the API server and DNS; not the
   backend (by Service or by pod address), not the internet on port 80 or
   53. The backend does not reach the operator's listener.
4. The catalogue in both pods is the contract's; a changed ConfigMap reaches
   both without a restart (same pods, no container restarted).
   `hack/stripe-secret.sh` with made-up values: a missing secret, a
   live key in test mode and an unknown mode stop it and make nothing; with
   both, the Secret has exactly the two keys and the ConfigMap the mode,
   and a restarted backend has all three in its environment (the operator
   none); the change is reported once and not again; with the mode empty
   both are removed. Its output is searched for the values.
5. The export job, from `deploy/gke`'s own CronJob with no bucket, completes
   and prints every object. `hack/billing-restore.sh` puts that file into a
   second, empty kind cluster: names, labels, spec and status are equal in
   the two, and a second restore changes nothing.

## Not yet checked: GKE

Look for these at the step named:

- Step 1: that the operator reaches the API server under Dataplane V2
  through its NetworkPolicy (the policy operator does, with the same rule).
- Step 2: that Pomerium passes Stripe's request body byte for byte and the
  `Stripe-Signature` header through (the contract's open question; track C
  checks it with `stripe trigger`).
- Step 3: **the upload**. On kind the export is printed, not uploaded: the
  token from GKE's metadata server, the NetworkPolicy rules for that server
  under Dataplane V2, and the Cloud Storage request have run nowhere. After
  the first deploy at `enforce`, either wait for 03:17 UTC or look the next
  day: `cluster info` shows the CronJob's LAST-SUCCESS and, with `--full`,
  the job's log (`exported N bytes to gs://…`). If it fails, the log says
  at which of the three; billing is not affected, the backup is missing.
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
3. **`deploy.yml`**: the Stripe Secret and ConfigMap are made by a step of
   their own, "Stripe", directly after "Secrets", which runs
   `hack/stripe-secret.sh` (its own copy of the stdin helper), so that the
   kind job can run the very same code. Beyond the contract it checks the
   key's prefix against the mode, and the stage against the mode, before
   anything is changed.
4. **The export bucket's role is `roles/storage.objectCreator`**, and the
   export authenticates by the ServiceAccount's federated identity, with no
   Google service account. The contract says only "through Workload
   Identity".
5. **Egress "the API server and DNS only"** is ports 443 and 6443 to any
   address (above).
6. **The operator's pod**: a read-only root filesystem with an `emptyDir`
   at `/tmp`, uid 65532 and `USER` in its environment, as the policy
   operator needed (kopf asks who it runs as). Track A's image has to run
   as a non-root uid with no entry in `/etc/passwd`.
7. **`backend/internal/auth/deploy_test.go`** (a test, not backend code)
   now expects four routes on the API host: the fourth is
   `api-stripe-webhook`, path `/stripe/webhook`.
8. **`ZERO_BALANCE_DELETE_AFTER`** (`336h`) is written out beside
   `ZERO_BALANCE_DELETE`; the contract names it in that row's text.
9. `docs/build-pipeline.md` still says four images; it is not this track's.
