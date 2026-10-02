# terraform-provider-metronome

A Terraform and OpenTofu provider for the pricing configuration of a
[Metronome](https://docs.metronome.com) account. Provider type `metronome`,
source address `r33drichards/metronome`.

It exists because nothing else does the job (looked at on 2026-10-02):

- Metronome's own provider,
  [`Metronome-Industries/metronome`](https://registry.terraform.io/providers/Metronome-Industries/metronome/latest)
  0.1.0-alpha.3, is marked "experimental. DO NOT use in production" and
  registers **no resources and no data sources**
  ([`internal/provider.go`](https://github.com/Metronome-Industries/terraform-provider-metronome/blob/main/internal/provider.go)
  returns two empty lists). It is not in OpenTofu's registry.
- [`buildwithdeck/terraform-provider-metronome`](https://github.com/buildwithdeck/terraform-provider-metronome)
  has one resource, a custom field key, and is in no registry.

| | | Change | Destroy |
|---|---|---|---|
| `metronome_billable_metric` | what is measured: a filter and an aggregation over usage events | the name, in place; anything else replaces | archives |
| `metronome_product` | what an invoice line is for: `USAGE` (on a metric), `FIXED`, `SUBSCRIPTION` | in place, from the start of the current hour; `type` replaces | archives |
| `metronome_rate_card` | the price list contracts refer to, with its aliases | in place; the currency replaces | archives |
| `metronome_rate` | the price of a product on a rate card from a moment on, with an optional commit rate | everything replaces: a new rate is added | leaves the rate in Metronome, with a warning |
| `metronome_alert` | a threshold notification, for one customer or all | everything replaces | archives |
| `metronome_custom_field_key` | permission to set a custom field on one kind of object | everything replaces | deletes |
| `metronome_pricing_unit` (data source) | a currency or custom pricing unit, by name | | |

Metronome deletes almost nothing and lets little change, and the provider
says so instead of pretending: "archives" means the object stays in the
account for good, unusable for anything new, and cannot be restored. An
object archived outside Terraform leaves the state (where the API lets the
provider see it), so the next apply makes a new one.

Not managed, on purpose: customers, contracts, commits and credits (an
application creates those at run time), usage events, and webhook
destinations (Metronome's API has no call for them; they are added in its
app under Developer, Notifications, Webhooks).

Reference for every argument: [`docs/`](docs/index.md), generated from the
schema. A complete configuration:
[`examples/computer-use/`](examples/computer-use/main.tf). Where it is used:
[`../infra/billing/`](../infra/billing/), described in
[`../docs/billing-iac.md`](../docs/billing-iac.md).

It is its own Go module, so none of its dependencies reach `backend/`.

## What was read, and what was not tried

The provider was written from Metronome's API description,
<https://docs.metronome.com/openapi.json>, as it stood on 2026-10-02, and
tested against a fake written from the same description. **It has never
called Metronome.** What the description does not say, and so what only a
real token will settle (the fake's choices are marked `ASSUMED` in
[`internal/fakeapi/fakeapi.go`](internal/fakeapi/fakeapi.go)):

1. Whether a product update may start at the beginning of the current hour
   (the description says only "on an hour boundary").
2. Whether a rate's `starting_at` must be on the hour (validated here as if
   it must).
3. What adding a rate with the same `starting_at` as an existing one does.
4. Whether updating a rate card's `aliases` replaces the list (assumed) or
   adds to it.
5. Whether timestamps and empty lists come back as they were sent. The
   provider keeps what the configuration wrote when the API's answer names
   the same moment or is equally empty, so either way plans stay empty.
6. What an archived rate card answers; the API has no field that says a
   rate card is archived.
7. The rate limit in practice: these endpoints are in the "8 requests a
   second" tier. A 429 is retried five times with a growing wait.

`make testacc` with `METRONOME_ACC_BEARER_TOKEN` set to a **sandbox** token
answers most of these in a minute (below).

## Install locally

The provider is in no registry yet. Until it is, build it here and tell
OpenTofu or Terraform where it is. Go comes from the repository's Nix dev
shell (`nix develop`, from the repository root or this directory).

### For development: `dev_overrides`

```sh
nix develop -c make build      # ./terraform-provider-metronome
```

In `~/.tofurc` for OpenTofu, `~/.terraformrc` for Terraform (or any file named
by `TF_CLI_CONFIG_FILE`, which both read):

```hcl
provider_installation {
  dev_overrides {
    "r33drichards/metronome" = "/path/to/browserjs-sessions/terraform-provider-metronome"
  }
  direct {}
}
```

With `dev_overrides` there is **no `init`** for this provider, and no lock
file entry. Every command prints a warning that overrides are in effect.

### To use it like a released provider: a filesystem mirror

```sh
nix develop -c make install    # version 0.1.0 into ~/.terraform.d/plugins
```

The binary lands at

```
~/.terraform.d/plugins/<host>/r33drichards/metronome/0.1.0/<os>_<arch>/terraform-provider-metronome_v0.1.0
```

once with `<host>` `registry.opentofu.org` and once with
`registry.terraform.io`. `tofu init` installs it from there. This is how
`infra/billing` gets it in GitHub Actions (`make install MIRROR=...`, and a
CLI configuration that names the mirror for this one provider).

### Use it

```sh
export METRONOME_BEARER_TOKEN=...     # Metronome app: Developer, API tokens
cd examples/computer-use
tofu plan
```

Keep the token in the environment, not in a `.tf` file; the provider marks it
sensitive and never logs it. `METRONOME_ENDPOINT` points the provider at
another base URL (the fake).

## Try it without Metronome

`cmd/fakeapi` is an in-memory fake of the endpoints the provider uses:

```sh
nix develop -c make fakeapi      # http://127.0.0.1:18090, token fake-token
export METRONOME_ENDPOINT=http://127.0.0.1:18090 METRONOME_BEARER_TOKEN=fake-token
```

It keeps Metronome's documented rules (a metric's definition is fixed, an
archived object stays readable and cannot be used again, rates accumulate,
a uniqueness key is refused twice) and refuses request fields the API does
not have.

## Develop

```sh
nix develop -c make test                                    # go vet, unit tests
nix shell nixpkgs#opentofu -c nix develop -c make testacc   # real plans and applies
nix shell nixpkgs#opentofu -c nix develop -c make e2e       # the example, end to end
nix shell nixpkgs#opentofu -c nix develop -c make docs      # regenerate docs/
```

- **Unit tests** drive the provider over protocol 6 in process, as Terraform
  does, against the fake. No `tofu` or `terraform` binary is involved.
- **Acceptance tests** (`TestAcc…`, skipped unless `TF_ACC=1`) run real
  plans, applies and imports. `TestAccPricing` runs against the fake,
  whatever the environment holds. **`TestAccLivePricing` runs against
  Metronome, and only when `METRONOME_ACC_BEARER_TOKEN` is set**: a name
  the provider itself does not read, so that a token lying in the
  environment cannot start it. Give it a sandbox token, never a production
  one: it leaves five archived objects behind, which Metronome keeps for
  good.
- **`hack/e2e.sh`** takes `examples/computer-use` through plan, apply, a
  rename, a new price, a refused timestamp and destroy, with
  `dev_overrides` and the fake.
- CI (`.github/workflows/terraform-provider-metronome.yml`) runs all three
  on pull requests that touch this directory, and fails if `docs/` is stale.
  It has no secret and never reaches Metronome.

## Publishing

Not done, and nothing in this repository needs it. It would take:

1. A repository of its own named `terraform-provider-metronome` (both
   registries require that name and one provider per repository), with this
   directory as its root.
2. A GPG key; its public half registered with the Terraform Registry (under
   the publishing account's signing keys) and submitted to the OpenTofu
   registry (a pull request to `opentofu/registry` adding the provider and
   the key).
3. A release workflow with GoReleaser producing, for each tag `vX.Y.Z`, the
   zips for every platform, `terraform-registry-manifest.json` (protocol
   6.0), `SHA256SUMS` and its signature.
4. `infra/billing` then drops the mirror step from its workflows and
   records the registry's checksums in its lock file.
