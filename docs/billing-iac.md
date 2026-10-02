# Billing as code: Stripe and Metronome in OpenTofu

What is sold in Stripe and what is metered in Metronome is defined in
`infra/billing`, planned on pull requests and applied from GitHub Actions.
Nothing of it is made by a setup command, and nothing by hand except what
the two services give no API for.

Contracts: [`contracts/billing/stripe.md`](contracts/billing/stripe.md),
[`contracts/billing/metronome.md`](contracts/billing/metronome.md),
[`contracts/billing/deploy.md`](contracts/billing/deploy.md). The numbers:
[`contracts/billing/catalogue.yaml`](contracts/billing/catalogue.yaml), which
`infra/billing` reads; no price or rate is written a second time.

**Nothing here has been applied.** `tofu validate` and the offline tests
pass; no plan has run against either service, because no agent has or may
have a key. Read the first plan closely, and see "Not verified" at the end.

```
infra/billing/                 one root module, a state for each mode
  catalogue.tf                 the catalogue, as locals
  stripe.tf                    products, prices, the webhook endpoint, the portal
  metronome.tf                 metrics, products, the rate card, rates, the alert, custom field keys
  test.tfvars, live.tfvars     the two modes
  provider.sh                  builds terraform-provider-metronome into a mirror
  keys.sh                      checks a mode's keys are there and are that mode's
  tests/offline.tftest.hcl     both providers mocked; contacts nothing
terraform-provider-metronome/  the Metronome provider (ours)
.github/workflows/
  billing-plan.yml             on pull requests: checks, and a plan for each mode whose keys are set
  billing-apply.yml            by hand, on main: plans, then applies one mode
  terraform-provider-metronome.yml   the provider's own tests
```

| Mode | Stripe | Metronome | State |
|---|---|---|---|
| `test` | the sandbox | the sandbox environment | `gs://<state bucket>/billing/test` |
| `live` | live mode | the production environment | `gs://<state bucket>/billing/live` |

The bucket is `infra/main`'s; the prefixes are separate from it and from
each other, so no apply of the cluster can touch billing and no apply of
one mode can touch the other.

## Providers

Looked at on 2026-10-02.

### Stripe

| Provider | State | Covers of what we need |
|---|---|---|
| [`stripe/stripe`](https://registry.terraform.io/providers/stripe/stripe/latest) 0.3.0, 2026-08-13. Stripe's own ([repository](https://github.com/stripe/terraform-provider-stripe), generated from its API description; "partner" tier; also in [OpenTofu's registry](https://registry.opentofu.org/v1/providers/stripe/stripe/versions)) | maintained; 0.x | products, prices (lookup keys), webhook endpoints (the signing secret is a sensitive attribute, "only returned at creation"), the portal configuration (`stripe_billing_portal_configuration`). Two gaps, below. |
| [`lukasaron/stripe`](https://registry.terraform.io/providers/lukasaron/stripe/latest) 3.4.1, 2025-11-14 | [archived](https://github.com/lukasaron/terraform-provider-stripe) 2026-04-17; its README sends users to Stripe's | products with a chosen ID, prices, webhook endpoints, a portal configuration without `billing_cycle_anchor` or `schedule_at_period_end` (it is built on `stripe-go` v78, API version 2024-04-10) |

**Chosen: `stripe/stripe`, pinned to exactly 0.3.0.** The other one is
archived, speaks a two-year-old API version, and cannot express the portal's
upgrade and downgrade rules either. Both take the key from `STRIPE_API_KEY`
and work with a sandbox's restricted key: a key is a key.

Its two gaps, and what was done about each:

1. **A product's ID cannot be chosen** ([issue 41](https://github.com/stripe/terraform-provider-stripe/issues/41),
   open). The contract named products by fixed IDs (`cu_plan_starter`, ...).
   Stripe now assigns the IDs, and the catalogue's `productId` is carried as
   `metadata.catalogue_product`. Nothing at run time needs a product ID: the
   backend finds prices by lookup key.
2. **The portal's list of switchable products cannot be managed**
   ([issue 53](https://github.com/stripe/terraform-provider-stripe/issues/53),
   open; confirmed in the provider's source, which never asks Stripe to
   expand `features.subscription_update.products`). Any configuration that
   sets it fails on apply and can never converge. So
   `portal_plan_switching` is `false`: the portal lets people update cards,
   see invoices and cancel, and **does not let them switch plan**. When a
   release fixes the issue: raise the version in `versions.tf`, set
   `portal_plan_switching = true` in the mode's `.tfvars`, and read the plan.
   Until then a subscriber changes plan by cancelling and subscribing again,
   or the product owner turns the switch on by hand in the Dashboard's own
   (default) portal configuration.

Raising the version: change `versions.tf`, run `tofu init -upgrade` and
`tofu providers lock -platform=linux_amd64 -platform=darwin_arm64 registry.opentofu.org/stripe/stripe`
in `infra/billing`, commit the lock file without the `r33drichards/metronome`
entry, and read both modes' plans in the pull request. A generated 0.x
provider can change a resource's shape between releases.

### Metronome

| Provider | State | Covers |
|---|---|---|
| [`Metronome-Industries/metronome`](https://registry.terraform.io/providers/Metronome-Industries/metronome/latest) 0.1.0-alpha.3, 2026-06-02. Metronome's own, generated with Stainless | README: "This terraform provider is experimental. DO NOT use in production." Not in OpenTofu's registry. | **nothing**: [`internal/provider.go`](https://github.com/Metronome-Industries/terraform-provider-metronome/blob/main/internal/provider.go) returns an empty list of resources and an empty list of data sources |
| [`buildwithdeck/terraform-provider-metronome`](https://github.com/buildwithdeck/terraform-provider-metronome), 2026-09 | one month old, no release, in no registry | one resource, a custom field key |

**So the provider was written: `terraform-provider-metronome/`**, source
address `r33drichards/metronome`. What it has, and how it treats what
Metronome will not change or delete:
[`../terraform-provider-metronome/README.md`](../terraform-provider-metronome/README.md).
It is in no registry; the workflows build it from this repository into a
filesystem mirror before `tofu init` (`provider.sh`). What publishing would
take is at the end of its README.

## What is code and what is by hand

| | Made by | Where |
|---|---|---|
| Stripe products and prices, from the catalogue | OpenTofu | `stripe.tf` |
| The Stripe webhook endpoint, its 23 events and its API version | OpenTofu | `stripe.tf` |
| The Customer Portal configuration | OpenTofu (plan switching off, above) | `stripe.tf` |
| Metronome billable metrics, products, the rate card `cu-standard-v1`, its two rates, the alert `cu-zero-balance`, the three custom field keys of a credit | OpenTofu | `metronome.tf` |
| The Stripe account and its sandbox; the two restricted keys of each mode; Radar, emails and failed-payment settings | the product owner, in the Dashboard | the design's to-do list |
| The Metronome sandbox and production environments; their API tokens | the product owner, in Metronome's app | the Metronome design's to-do list |
| **Metronome's webhook destination and its secret** | the product owner, in Metronome's app (Developer, Notifications, Webhooks): Metronome's API has no call that creates one | secret `METRONOME_<ENV>_WEBHOOK_SECRET` |
| Stripe restricted keys | the product owner; never OpenTofu (a key in a state file is a key leaked) | |
| Customers, Checkout Sessions, subscriptions, payments; Metronome customers, contracts, credits, usage | the backend, the observer, and the two services, at run time | |

## The product owner's steps

No key is pasted anywhere but a GitHub secret field (repository Settings,
Secrets and variables, Actions, Secrets). No agent sees one.

**Test mode**

1. In the Stripe Dashboard, in the sandbox: Developers, API keys, "Create
   restricted key", named `opentofu`. Permissions, everything else **None**:

   | Resource, as the Dashboard names it | Permission | For |
   |---|---|---|
   | Products | Write | `/v1/products` |
   | Prices | Write | `/v1/prices` |
   | Webhook Endpoints | Write | `/v1/webhook_endpoints` |
   | Customer portal | Write | `/v1/billing_portal/configurations` |

   Put the key in the secret **`STRIPE_TEST_SETUP_KEY`**. (Write includes
   read. The names in the left column are **not verified** against the
   Dashboard: Stripe documents no list of them. If an apply fails with a
   403, Stripe's message names the permission to add; edit the key, do not
   replace it.)
2. In Metronome's sandbox: Developer, API tokens, create a token. Put it in
   **`METRONOME_SANDBOX_API_TOKEN`**. (A Metronome token has no
   permissions to choose: it can do everything in its environment. To
   apply Stripe before the Metronome sandbox exists, set
   `metronome_enabled = false` in `infra/billing/test.tfvars`.)
3. Open any pull request that touches `infra/billing` (or run **billing
   plan** by hand) and read the plan for `test`. The first one creates
   everything: 4 products, 6 prices, 1 webhook endpoint and 1 portal
   configuration in Stripe; 2 metrics, 3 products, 1 rate card, 2 rates,
   1 alert and 3 custom field keys in Metronome; and one bookkeeping
   resource (`terraform_data.webhook_generation`). 25 to add, none to
   change or destroy.
4. Set the repository **variable** `STRIPE_MODE` to `test` if it is not
   already, and make sure the deploy workflow has run once (the namespace
   must exist for step 5's last part).
5. Actions, **billing apply**, Run workflow, branch `main`, mode `test`,
   confirm `apply`. It plans again, prints that plan, applies it, and
   copies the webhook endpoint's signing secret into the cluster's Secret
   `stripe-webhook`.
6. A second **billing plan** shows no changes (Metronome's check M0).
7. In Metronome's sandbox: Developer, Notifications, Webhooks, Add
   `https://api.computeruse.site/metronome/webhook`; put its secret in
   **`METRONOME_SANDBOX_WEBHOOK_SECRET`**. This is the one object with no
   API.

**Live mode**, later: the same with a restricted key of live mode in
**`STRIPE_LIVE_SETUP_KEY`**, a production token in
**`METRONOME_PRODUCTION_API_TOKEN`**, mode `live`, confirm **`apply live`**,
and `STRIPE_MODE` = `live` before the apply so that the signing secret is
copied.

| Secret | What | Read by |
|---|---|---|
| `STRIPE_TEST_SETUP_KEY`, `STRIPE_LIVE_SETUP_KEY` | restricted key, the four permissions above | `billing-plan.yml`, `billing-apply.yml` |
| `METRONOME_SANDBOX_API_TOKEN`, `METRONOME_PRODUCTION_API_TOKEN` | API token of that environment | the same two, and `deploy.yml` |

The workflows refuse a key of the other mode (by its prefix, `rk_test_` or
`rk_live_`; the key itself is never printed), and Stripe's own answer is
checked again in the plan (`livemode`). A Metronome token says nothing
about its environment: putting the right one under the right name is on
the person who enters it.

`STRIPE_<MODE>_WEBHOOK_SECRET` is **no longer entered by anyone**; see the
next section.

## The webhook's signing secret

Stripe returns an endpoint's signing secret once, in the answer to the
call that creates it. OpenTofu makes that call, so the secret is:

- **in the state**, `gs://<state bucket>/billing/<mode>`, as an attribute of
  `stripe_webhook_endpoint.backend`, and
- **in the cluster**, in the Secret `stripe-webhook` (key
  `STRIPE_WEBHOOK_SECRET`), copied there by the last steps of **billing
  apply**: read from the state into a file only that step can read, given
  to `kubectl`, deleted; never in a log, a command line, an output, a
  GitHub secret or a person's hands. The copy happens when the repository
  variable `STRIPE_MODE` equals the mode applied, and the backend is
  restarted if it takes the Secret and the value changed.

Nothing else holds it. `tofu output` and the plan print it as
`(sensitive value)`.

**Who can read the state.** The bucket is private (uniform access, no
public access). It is readable by: the project's owners; the `tofu-apply`
service account (workflows on `main`); and the `tofu-plan` service account,
which **any branch or pull request of this repository** can use. So anyone
who can push a branch here can, with a workflow of their own, read the
signing secret (and could already read `infra/main`'s state). That is the
same set of people who can change `main`'s workflows. Pull requests from
forks get no credentials. If that set is ever wider than "people trusted
with the webhook secret", move the billing states to a bucket of their own
that only a `main`-bound service account can read; the plan on pull
requests then has to go.

The apply itself runs as `tofu-plan`, not `tofu-apply`: the only thing it
needs from Google Cloud is to write its state, which `tofu-plan` may, and
`tofu-apply` is project Owner.

**Rotating it**: Stripe's Dashboard can roll an endpoint's secret, but the
new one would be known only to the Dashboard. Instead raise
`webhook_generation` by one in the mode's `.tfvars`: the plan shows the
endpoint **replaced** (a new endpoint, a new secret, the old endpoint
deleted), and the apply copies the new secret to the cluster. Events
delivered between the two moments fail their signature check, are retried
by Stripe, and are repaired in any case by the backend's reconcile.

**If the state has no secret** (an endpoint that was imported rather than
created has none, because Stripe will not show it again), the apply stops
with an error naming this section: rotate, as above.

**What track B has to change** (it was written when a person entered the
secret): the backend's `STRIPE_WEBHOOK_SECRET` comes from the Secret
`stripe-webhook`, key `STRIPE_WEBHOOK_SECRET`, not from the Secret
`stripe`; `deploy.yml` stops requiring `STRIPE_<MODE>_WEBHOOK_SECRET` and
stops writing that key. `deploy.md` says so.

## Changing things later

### A price (Stripe)

Stripe prices are immutable: an amount, a currency and an interval never
change. The provider would handle an edited amount by **replacing** the
price, which deactivates the old one under the subscribers who are on it
and moves its lookup key. So an entry of the catalogue is never edited:

1. In `catalogue.yaml`, add an entry with a new `lookupKey`
   (`cu_pro_monthly_v2`), the same `productId`, the new `amount`.
2. Set `enabled: false` on the old entry. Keep it: existing subscribers
   stay on that price, and their credit is looked up by it.
3. The plan shows **one price to add and one to change** (the old one
   becomes inactive: no new purchase can use it; subscriptions on it go
   on). If it shows a price **to replace**, an old entry was edited: undo
   that.
4. Merge, **billing apply**, then deploy the catalogue as usual.

What a plan *gives* (credit, limits) is not in Stripe: it changes with the
catalogue's ConfigMap alone, with no apply here.

Removing an entry altogether deactivates its price (Stripe has no delete
for prices or products); do it only when nobody is subscribed to it.

### A rate (Metronome)

A rate card's rates are a schedule that is only ever added to. When
`rates` in the catalogue changes, change `metronome_rates_starting_at` in
the mode's `.tfvars` **in the same pull request**, to the hour the new
price starts (in the future, or the top of the current hour). The plan
shows each changed rate **replaced**: the old one leaves the state (with a
warning that it stays in Metronome, which is right: it is the price of the
past) and the new one is added from its start. Usage before that moment
keeps the old price. Changing a rate without moving the start is not
refused by the plan; what Metronome does with two rates for one moment is
not documented.

### A metric (Metronome)

A metric's definition is fixed for good, and `prevent_destroy` makes the
plan fail rather than replace one. To change what is measured: add an
entry to `metronome_metrics` with the next suffix (`cu_awake_seconds_v2`),
point the product at it (`metric = ...`), and apply: one metric to add, one
product to change in place. The product meters with the new metric from
the top of that hour. Remove the old entry (and, deliberately, its
`prevent_destroy`) only when nothing sends events for it. The observer and
`metronome.md` change with it.

### Never

`tofu destroy`, or `metronome_enabled = false` after an apply, against
production: archiving a metric, a product or the rate card stops rating
for every customer and cannot be undone. `prevent_destroy` refuses both.

## If the objects already exist

A sandbox that an earlier setup command ran against already has prices
with these lookup keys, and Stripe refuses a second price with a lookup
key that is in use. Adopt them instead of creating them: add `import`
blocks to a pull request (IDs are not secrets), read the plan, apply,
remove the blocks.

```hcl
import {
  to = stripe_price.plan["cu_starter_monthly_v1"]
  id = "price_..."
}
```

The same for Metronome objects made by hand (`metronome_billable_metric`,
`metronome_product` and `metronome_rate_card` import by ID; a rate by
`<rate card>/<product>/<starting_at>`; an alert cannot be imported:
archive it in Metronome's app, releasing its uniqueness key).

## Checking the code without a key

```sh
cd infra/billing
export TF_CLI_CONFIG_FILE="$(nix develop -c ./provider.sh)"   # builds the Metronome provider
nix shell nixpkgs#opentofu -c tofu fmt -check -recursive
nix shell nixpkgs#opentofu -c tofu init -backend=false
nix shell nixpkgs#opentofu -c tofu validate
nix shell nixpkgs#opentofu -c tofu test      # mocked providers; contacts nothing
```

`init` adds an entry for `r33drichards/metronome` to
`.terraform.lock.hcl`; do not commit it (the binary is built for each run,
so its checksum is not stable). The workflows pin OpenTofu 1.10.7.

OpenTofu is never run from a laptop against the real accounts: that would
need a key on the laptop.

## Not verified

No plan has run. In the order they are likely to matter on the first
apply:

1. The four permission names of the Stripe restricted key (above).
2. Whether Stripe accepts `2026-08-26.dahlia` as an endpoint's
   `api_version` (it is the version of the backend's `stripe-go` v86.4.2).
   If not: set `webhook_api_version = null` in the `.tfvars`; the backend
   reads events of any version.
3. Whether a portal configuration with `subscription_update` disabled is
   accepted as written, with its other fields null.
4. Whether `stripe/stripe` 0.3.0 has other read-back faults like issue 53
   on the attributes used here (a second plan that is not empty would show
   one).
5. Everything about the Metronome provider against the real API: it was
   written from Metronome's API description and tested against a fake.
   The list is in its README ("What was read, and what was not tried").
   Check M0 of `metronome.md` is exactly this.
6. That a customer-level credit's custom fields use the entity
   `contract_credit` (the API's list of entities has `contract_credit` and
   `commit`, and no other credit).
7. That the alert's `threshold: 0` is accepted (check M4).
8. The last steps of **billing apply** (reading the state as `tofu-plan`
   while `kubectl` runs as the deployer) have not run.
