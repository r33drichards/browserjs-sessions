# Deployment contract

Names, flags, secrets, routes and rights. Everything is in the namespace
`browserjs-sessions`.

## Flags and stages

| Stage | What is on | Set by |
|---|---|---|
| 0 | nothing: `BILLING` unset. The CRDs may be installed; nothing reads them. | default |
| 1, shadow | `BILLING=meter`, no Stripe key: usage is metered and shown; nothing is refused; no purchase is offered | `patch-backend.yaml`, the operator's Deployment |
| 2, test payments | stage 1 + `STRIPE_MODE=test` and the test secrets: Checkout and the portal work with test cards | the repository variable `STRIPE_MODE`, the secrets below |
| 3, enforce | `BILLING=enforce` (for the two allow-listed users this is a rehearsal) | `patch-backend.yaml` |
| 4, open sign-up | `OPEN_SIGNUP=true` and Pomerium's policy changed in the same pull request; needs the prerequisites of the tracks document | `patch-backend.yaml`, `pomerium-config.yaml` |
| 5, live payments | `STRIPE_MODE=live` and the live secrets | the repository variable, the secrets |

Stages 4 and 5 are independent of each other, and each is one small pull
request that can be reverted.

## Backend configuration

| Env var | Default | Meaning |
|---|---|---|
| `BILLING` | `off` | `off`, `meter`, `enforce` (`enforcement.md`) |
| `BILLING_GRACE` | `5m` | from `exhaustedAt` to the sleep |
| `BILLING_STALE_AFTER` | `10m` | age of `observedAt` beyond which the ledger is stale |
| `BILLING_EXEMPT_EMAILS` | the value of `ADMIN_EMAILS` | never refused or stopped |
| `MAX_AWAKE_SESSIONS` | `10` | cluster-wide places for awake sessions (today's quota gives 11) |
| `FREE_AWAKE_CEILING` | `5` | places a non-paying account may take |
| `WAKES_PER_HOUR` | `30` | starts per account per hour |
| `IDLE_DELETE` | `off` | delete long-unused sessions of free and pay-as-you-go accounts |
| `FREE_TIER` | `on` | (operator) `off`: no new free Grants are made; the kill switch for abuse |
| `STRIPE_MODE` | unset | `test` or `live`. Unset: no purchase routes, no webhook route, the UI offers nothing to buy. |
| `STRIPE_API_KEY` | | from the Secret. Required with `STRIPE_MODE`. The backend refuses to start if the key is a live key in `test` mode or the reverse (it looks at the prefix; the value is never logged or put in an error). |
| `STRIPE_WEBHOOK_SECRET` | | from the Secret. Required with `STRIPE_MODE`. |
| `OPEN_SIGNUP` | `false` | `true`: any signed-in user gets an Account (within the sign-up limits), and API tokens are allowed for any account that is not blocked, in place of `ALLOWED_EMAILS` |
| `SIGNUPS_PER_DAY`, `SIGNUPS_PER_IP_PER_DAY` | `50`, `3` | with `OPEN_SIGNUP` |
| `TERMS_VERSION` | unset | with `OPEN_SIGNUP`: the version users must have accepted |

`BILLING` other than `off` requires the three CRDs to be served; the
backend checks at start and fails with a message naming the missing one.
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

GitHub Actions **variable** (not secret): `STRIPE_MODE` = `test` (later
`live`); unset or empty means no Stripe.

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
| run time | Checkout Sessions, Customers, Customer portal sessions, Subscriptions (cancel on account deletion) | Prices, Products, Invoices, Invoice payments, PaymentIntents, Charges, Refunds, Disputes |
| setup | Products, Prices, Customer portal configurations | the same |

## Workflows

| Workflow | Trigger | Does |
|---|---|---|
| `stripe-setup.yml` (new) | by hand; inputs `mode` (`test`, `live`), `apply` (`false` prints the plan, `true` makes the changes), and for live `confirm` typed as `live` | runs `go run ./backend/cmd/stripe-setup --catalogue docs/contracts/billing/catalogue.yaml` with the setup key of that mode; prints object IDs and what changed, never a key |
| `deploy.yml` (edited) | as today | the Secret and ConfigMap above |
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
| Config | `BILLING`, `FREE_TIER`, `TICK` (60s), `MAX_GAP` (150s); `catalogue.yaml` copied into the image at build time from this directory |
| Egress | the API server and DNS only (NetworkPolicy `billing-operator`); no ingress |

A separate operator from the policy operator: different rights, and a
restart of one must not stop the other.

A CronJob `billing-export` (stage 3 on GKE): daily, `kubectl get
accounts,grants,usageperiods -o yaml` to a versioned Cloud Storage bucket
through Workload Identity (bucket and binding in `infra/main`, object
versioning on, 90 day lifecycle). The restore is `kubectl apply` of the
latest export followed by the Stripe reconcile (`stripe.md`). Until this
exists, losing the cluster loses the usage counted in the current period
and the free grants (in the users' favour); purchases come back from Stripe.

## RBAC

| ServiceAccount | Rules |
|---|---|
| `billing-operator` (Role) | `browserjs.dev` `accounts`: get, list, watch; `accounts/status`: get, patch, update; `grants`: get, list, watch, create, delete; `grants/status`: patch; `usageperiods`: get, list, create, delete; `agents.x-k8s.io` `sandboxes`: get, list, watch; `events`: create |
| `billing-operator` (ClusterRole) | `apiextensions.k8s.io` `customresourcedefinitions`: list, watch (kopf) |
| `backend` (added to its Role) | `accounts`: get, list, watch, create, update, patch; `grants`: get, list, watch, create, patch; `usageperiods`: get, list |
| `billing-export` | `accounts`, `grants`, `usageperiods`: get, list |

The backend never writes `accounts/status`. The operator never writes an
Account's `spec`. No ServiceAccount gains anything on `secrets` or
`configmaps`.

## Labels

Every Account, Grant and UsagePeriod carries `browserjs.dev/owner` = the
owner hash, as Sandboxes do. A pack's Grant also carries
`browserjs.dev/payment-intent` = the PaymentIntent's ID in lower case.

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
```

On the API host, not the app's: that host already takes requests that
carry no sign-in and is told nothing about the caller, and the app's host
keeps having no public path at all. Whether Pomerium passes the body
byte for byte and the `Stripe-Signature` header through is **not
verified**; it is the first thing track C checks, with `stripe trigger`.

Stage 4 changes the policy shared by the `app`, `session-mcp` and
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

`deploy/local` runs with `BILLING=meter` and no Stripe by default. With
Stripe: a developer's own sandbox key in an untracked `.env`,
`stripe listen --forward-to http://localhost:8080/stripe/webhook` (the
command prints its own `whsec_...`, stable across restarts, which goes in
the same `.env`), `stripe trigger checkout.session.completed` for canned
events, test card `4242 4242 4242 4242`, and test clocks ("simulations",
sandbox only: three customers per clock, advance at most two billing
intervals per call) for renewals, failed payments and cancellations. The
procedure is written by track C in `docs/billing-development.md`.
