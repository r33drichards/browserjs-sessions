# Deployment contract

Names, flags, secrets, routes and rights. Everything is in the namespace
`browserjs-sessions`.

> **Changed 2026-10-02 by [`metronome.md`](metronome.md)**: one more
> Secret (`metronome`) and one more public route; the operator is the
> observer, sends usage to Metronome and writes nothing to an Account; of
> the three CRDs only `Account` is deployed.

## Flags and stages

| Stage | What is on | Set by |
|---|---|---|
| 0 | nothing: `BILLING` unset. The CRD may be installed; nothing reads it. | default |
| 1, shadow | `BILLING=meter` and the Metronome **sandbox** token, no Stripe key: usage is metered in Metronome and shown; nothing is refused; no purchase is offered | `patch-backend.yaml`, the operator's Deployment, the secrets below |
| 2, test payments | stage 1 + `STRIPE_MODE=test` and the test secrets: Checkout and the portal work with test cards | the repository variable `STRIPE_MODE`, the secrets below |
| 3, enforce | `BILLING=enforce`: the card gate, the stop at zero (for the two allow-listed users this is a rehearsal, with test cards). Requires stage 2. | `patch-backend.yaml` |
| 4, live payments | `STRIPE_MODE=live` and the live secrets, of Stripe **and of Metronome's production environment** (the setup workflow run against it first). Test-mode cards, sandbox customers and credit do not carry over: every Account's `metronomeCustomerId` and `credit` are cleared by the documented command, and the reconcile makes production customers. | the repository variable, the secrets |
| 5, open sign-up | `OPEN_SIGNUP=true` and Pomerium's policy changed in the same pull request; needs live payments and the other prerequisites of the design's section 8.5 | `patch-backend.yaml`, `pomerium-config.yaml` |

Each stage is one small pull request (or one variable) that can be reverted.

## Backend configuration

| Env var | Default | Meaning |
|---|---|---|
| `BILLING` | `off` | `off`, `meter`, `enforce` (`enforcement.md`) |
| `BILLING_GRACE` | `5m` | from `exhaustedAt` to the start of the stop sequence |
| `BILLING_DRAIN_TIMEOUT` | `10m` | the longest the stop sequence waits for calls in flight (the proxy's own `mcpResponseTimeout`) |
| `BILLING_BALANCE_PASS` | `5m` | how often the balance pass reads Metronome (`metronome.md`). Replaces `BILLING_STALE_AFTER`. |
| `METRONOME_API_TOKEN`, `METRONOME_WEBHOOK_SECRET` | | from the Secret `metronome`. Required when `BILLING` is not `off`; the backend refuses to start without them and never logs them. |
| `METRONOME_URL` | `https://api.metronome.com` | |
| `BILLING_EXEMPT_EMAILS` | the value of `ADMIN_EMAILS` | never refused or stopped |
| `MAX_AWAKE_SESSIONS` | `10` | cluster-wide places for awake sessions (today's quota gives 11) |
| `WAKES_PER_HOUR` | `30` | starts per account per hour |
| `ZERO_BALANCE_DELETE` | `off` | delete the sessions of an account that has been at zero for `ZERO_BALANCE_DELETE_AFTER` (`336h`, 14 days) |
| `SIGNUP_CREDIT` | `on` | `off`: cards are still saved, no sign-up credit is granted; the kill switch for abuse of the bonus |
| `AUTO_RECHARGE` | `off` | `on`: accounts may turn auto-recharge on |
| `BILLING_CATALOGUE` | `/etc/browserjs/catalogue.yaml` | the catalogue file (ConfigMap `billing-catalogue`), re-read when it changes; a file that does not parse keeps the last good one and is logged |
| `STRIPE_MODE` | unset | `test` or `live`. Unset: no checkout routes, no webhook route. `BILLING=enforce` refuses to start without it. |
| `STRIPE_API_KEY` | | from the Secret. Required with `STRIPE_MODE`. The backend refuses to start if the key is a live key in `test` mode or the reverse (it looks at the prefix; the value is never logged or put in an error). |
| `STRIPE_WEBHOOK_SECRET` | | from the Secret. Required with `STRIPE_MODE`. |
| `OPEN_SIGNUP` | `false` | `true`: any signed-in user gets an Account (within the sign-up limits), and API tokens are allowed for any account that is not blocked, in place of `ALLOWED_EMAILS` |
| `SIGNUPS_PER_DAY`, `SIGNUPS_PER_IP_PER_DAY` | `200`, `5` | with `OPEN_SIGNUP` |
| `TERMS_VERSION` | unset | the version of the terms users must have accepted; unset, none is asked |

`BILLING` other than `off` requires the `Account` CRD to be served and the
Metronome rate card `cu-standard-v1` to exist; the backend checks both at
start and fails with a message naming what is missing.
`STRIPE_MODE` requires `BILLING` other than `off` and `API_URL`.

## Secrets

GitHub Actions **secrets**, entered by the product owner and nobody else,
never printed, never in the repository, a log or a chat:

| Secret | What it is | Used by |
|---|---|---|
| `STRIPE_TEST_API_KEY` | restricted key (`rk_test_...`) of the sandbox, permissions below | `deploy.yml` |
| `STRIPE_TEST_WEBHOOK_SECRET` | signing secret (`whsec_...`) of the sandbox's webhook endpoint | `deploy.yml` |
| `STRIPE_TEST_SETUP_KEY` | restricted key of the sandbox for the setup command | `stripe-setup.yml` |
| `STRIPE_LIVE_API_KEY`, `STRIPE_LIVE_WEBHOOK_SECRET`, `STRIPE_LIVE_SETUP_KEY` | the same three for live mode, later | the same |

| `METRONOME_SANDBOX_API_TOKEN` | an API token of Metronome's sandbox (Developer, API tokens) | `deploy.yml`, `metronome-setup.yml` |
| `METRONOME_SANDBOX_WEBHOOK_SECRET` | the secret of the sandbox's webhook destination | `deploy.yml` |
| `METRONOME_PRODUCTION_API_TOKEN`, `METRONOME_PRODUCTION_WEBHOOK_SECRET` | the same two for production, later | the same |

GitHub Actions **variable** (not secret): `STRIPE_MODE` = `test` (later
`live`); unset or empty means no Stripe. Metronome's environment follows
it: the sandbox secrets while it is unset or `test`, the production ones
when it is `live`.

`deploy.yml`, in the same step: when `BILLING` is to be on, it fails if
either Metronome secret of that environment is empty, and applies the
Secret `metronome` with the keys `METRONOME_API_TOKEN` and
`METRONOME_WEBHOOK_SECRET`. The backend takes both, the observer takes the
token only, with `secretKeyRef`. A changed Secret restarts both.

`deploy.yml`, in its "Secrets" step and with its `secret_from_stdin`
helper: when `STRIPE_MODE` is set, it fails if either secret of that mode
is empty, and applies the Secret `stripe` with the keys `STRIPE_API_KEY`
and `STRIPE_WEBHOOK_SECRET` from the secrets of that mode; when it is
unset it deletes the Secret `stripe` if present. The backend's Deployment
takes both keys with `secretKeyRef` and `optional: true`, and `STRIPE_MODE`
from a ConfigMap `billing-mode` the same step writes (so that the mode and
the keys can only change together). A changed Secret restarts the backend
(the step compares `resourceVersion`, as it does for Dex).

Restricted key permissions (resource names as the Dashboard shows them are
**not verified**; Stripe's advice is to start broad in the sandbox and
prune with the key's request log):

| Key | Write | Read |
|---|---|---|
| run time | Checkout Sessions, Customers, Customer portal sessions, PaymentIntents (auto-recharge), PaymentMethods (detach on account deletion), Subscriptions (cancel on account deletion) | Prices, Products, SetupIntents, Invoices, Invoice payments, Charges, Refunds, Disputes |
| setup | Products, Prices, Customer portal configurations | the same |

## Workflows

| Workflow | Trigger | Does |
|---|---|---|
| `stripe-setup.yml` (new) | by hand; inputs `mode` (`test`, `live`), `apply` (`false` prints the plan, `true` makes the changes), and for live `confirm` typed as `live` | runs `go run ./backend/cmd/stripe-setup --catalogue docs/contracts/billing/catalogue.yaml` with the setup key of that mode; prints object IDs and what changed, never a key |
| `metronome-setup.yml` (new) | by hand; inputs `environment` (`sandbox`, `production`), `apply`, and for production `confirm` typed as `production` | runs `go run ./backend/cmd/metronome-setup --catalogue docs/contracts/billing/catalogue.yaml` with the token of that environment; prints object IDs and what changed, never a token |
| `deploy.yml` (edited) | as today | the Secrets and ConfigMap above |
| `images.yml`, `hack/pin-images.sh` (edited) | as today | the `billing-operator` image beside the others |

## Workloads

| | Billing operator |
|---|---|
| Deployment, pod label | `billing-operator`, `app: billing-operator` |
| Replicas | 1, `strategy: Recreate` |
| Image | `browserjs/billing-operator`, pinned by `hack/pin-images.sh`; source `images/billing-operator/` (the layout of `images/policy-operator/`) |
| Command | `kopf run --standalone --namespace=browserjs-sessions --liveness=http://0.0.0.0:8081/healthz -m billing_operator` |
| Service | none: nothing calls it |
| Scheduling (GKE) | the system pool |
| Resources (requests) | 50m CPU, 128Mi |
| Config | `BILLING`, `TICK` (60s), `MAX_GAP` (150s), `BILLING_CATALOGUE` (for `sessionDiskGB` only), `METRONOME_API_TOKEN`, `METRONOME_URL` |
| Egress | the API server, DNS, and TCP 443 to the internet for Metronome's API (NetworkPolicy `billing-operator`; a NetworkPolicy cannot name a host); no ingress |

It is the **observer**: it lists Sandboxes each tick, computes seconds
(`metering.md`), sends events to Metronome and renews the Lease
`billing-observer` (`metronome.md`, "Usage events"). It reads and writes no
Account.

A separate operator from the policy operator: different rights, and a
restart of one must not stop the other.

A CronJob `billing-export` (from stage 3 on GKE): daily, `kubectl get
accounts -o yaml` to a versioned Cloud Storage bucket
through Workload Identity (bucket and binding in `infra/main`, object
versioning on, 90 day lifecycle). The restore is `kubectl apply` of the
latest export followed by the Stripe reconcile (`stripe.md`). Usage and
credit are in Metronome and are not lost with the cluster; which cards have
had the sign-up credit is Metronome's uniqueness keys. What the export
saves is the Accounts' card state, flags and customer IDs.

## RBAC

| ServiceAccount | Rules |
|---|---|
| `billing-operator` (Role) | `agents.x-k8s.io` `sandboxes`: get, list, watch; `coordination.k8s.io` `leases`: get, create, update (the one named `billing-observer`); `events`: create |
| `billing-operator` (ClusterRole) | `apiextensions.k8s.io` `customresourcedefinitions`: list, watch (kopf) |
| `backend` (added to its Role) | `accounts`: get, list, create, update, patch (no `watch`: there is no informer); `leases`: get (`billing-observer`) |
| `billing-export` | `accounts`: get, list |

The operator has no right on Accounts. No ServiceAccount gains anything on
`secrets` or `configmaps`; the two pods read the Secret `metronome` through
`secretKeyRef` only.

## The catalogue

`deploy/base/catalogue.yaml` is this directory's `catalogue.yaml`, made into
the ConfigMap `billing-catalogue` by `configMapGenerator` **without** a name
suffix hash, and mounted in the backend and the operator. A change of a
credit amount or a limit is a pull request to that file and a
manifest apply: no image is built and no pod restarts. A change of a
**price** also needs the Stripe setup workflow (a new lookup key), and a
change of a **rate** the Metronome setup workflow (a new rate on the rate
card, from a date). Track B
keeps the two copies identical with a test.

## Labels

Every Account carries `browserjs.dev/owner` = the owner hash, as Sandboxes
do, and once it has them `browserjs.dev/stripe-customer` and
`browserjs.dev/metronome-customer` (the IDs in lower case), so that
`Accounts.ByCustomer` and the Metronome webhook find it with a label
selector and not a cache. A pack's credit carries the PaymentIntent's ID
as the custom field `payment_intent` in Metronome.

## Pomerium routes

Added to every `pomerium-config.yaml`, beside the API host's three routes:

```yaml
  # Stripe's webhook. Nobody signs in: the request's signature is the
  # credential, checked by the backend (docs/contracts/billing/stripe.md).
  - name: api-stripe-webhook
    from: https://api.computeruse.site
    path: /stripe/webhook
    to: http://backend
    allow_public_unauthenticated_access: true
    preserve_host_header: true
  # Metronome's webhook, the same way (docs/contracts/billing/metronome.md).
  - name: api-metronome-webhook
    from: https://api.computeruse.site
    path: /metronome/webhook
    to: http://backend
    allow_public_unauthenticated_access: true
    preserve_host_header: true
```

On the API host, not the app's: that host already takes requests that
carry no sign-in and is told nothing about the caller, and the app's host
keeps having no public path at all. Whether Pomerium passes the body
byte for byte and the `Stripe-Signature` header through is **not
verified**; it is the first thing track C checks, with `stripe trigger`.

Stage 5 changes the policy shared by the `app`, `session-mcp` and
`legacy-session-mcp` routes from the two addresses to:

```yaml
    policy: &allowed
      - allow:
          and:
            - authenticated_user: true
```

(not the route setting `allow_any_authenticated_user`, which Pomerium's
documentation says bypasses centrally managed policy).

## Local development

`deploy/local` runs with `BILLING` off by default; `meter` needs a
developer's own Metronome sandbox token in the untracked `.env` (a second
sandbox customer set is made under an alias prefix `local-`, so that a
laptop and production's rehearsal do not share customers). With
Stripe: a developer's own sandbox key in an untracked `.env`,
`stripe listen --forward-to http://localhost:8080/stripe/webhook` (the
command prints its own `whsec_...`, stable across restarts, which goes in
the same `.env`), `stripe trigger checkout.session.completed` for canned
events, test card `4242 4242 4242 4242` (and `4000 0000 0000 0341`, which
saves and then declines, for auto-recharge failures), and test clocks ("simulations",
sandbox only: three customers per clock, advance at most two billing
intervals per call) for renewals, failed payments and cancellations. The
procedure is written by track C in `docs/billing-development.md`.
