# terraform-provider-browserjs

A Terraform and OpenTofu provider for browserjs sessions and their policies.
Provider type `browserjs`, source address `r33drichards/browserjs`.

| | |
|---|---|
| `browserjs_session` (resource) | a session: one persistent browser, driven over MCP |
| `browserjs_session_policy` (resource) | a session's policy, managed as code, in JSON or Rego |
| `browserjs_session`, `browserjs_sessions` (data sources) | sessions that already exist |
| `browserjs_policy_document` (data source) | builds a JSON policy from HCL blocks |

Reference for every argument: [`docs/`](docs/index.md), generated from the
schema. A complete configuration: [`examples/session-policies/`](examples/session-policies/main.tf).
How it behaves and why: [`../docs/terraform-provider.md`](../docs/terraform-provider.md).
The contract it is built to: [`../docs/contracts/policy/terraform-provider.md`](../docs/contracts/policy/terraform-provider.md).

It is its own Go module, so none of its dependencies reach `backend/`.

## Install locally

The provider is in no registry yet. Until it is, build it here and tell
OpenTofu or Terraform where it is. Go comes from the repository's Nix dev
shell (`nix develop`, from the repository root or this directory).

### For development: `dev_overrides`

```sh
nix develop -c make build      # ./terraform-provider-browserjs
```

In `~/.tofurc` for OpenTofu, `~/.terraformrc` for Terraform (or any file named
by `TF_CLI_CONFIG_FILE`, which both read):

```hcl
provider_installation {
  dev_overrides {
    "r33drichards/browserjs" = "/path/to/browserjs-sessions/terraform-provider-browserjs"
  }
  direct {}
}
```

The path is the directory that holds the binary. With `dev_overrides` there
is **no `init`** for this provider: `tofu init` fails looking for it in a
registry, and is not needed. Go straight to `tofu plan`. Every command
prints a warning that overrides are in effect. Rebuild, and the next command
uses the new binary.

### To use it like a released provider: a local mirror

```sh
nix develop -c make install    # version 0.1.0 into ~/.terraform.d/plugins
```

That directory is the one both tools search without being told. The binary
lands at

```
~/.terraform.d/plugins/<host>/r33drichards/browserjs/0.1.0/<os>_<arch>/terraform-provider-browserjs_v0.1.0
```

once with `<host>` `registry.opentofu.org` (what OpenTofu expands
`r33drichards/browserjs` to) and once with `registry.terraform.io`
(Terraform's). Then `tofu init` or `terraform init` installs it from there
and writes a lock file, as for any provider.

To keep the mirror somewhere else, `make install MIRROR=/some/dir` and name it:

```hcl
provider_installation {
  filesystem_mirror {
    path    = "/some/dir"
    include = ["r33drichards/browserjs"]
  }
  direct {
    exclude = ["r33drichards/browserjs"]
  }
}
```

### Use it

```sh
export BROWSERJS_ENDPOINT=https://api.browserjs.com   # the default
export BROWSERJS_TOKEN=bjs_...                        # Tokens page of the UI
cd examples/session-policies
tofu plan                                             # or terraform plan
```

The token needs the scopes `sessions:read`, `sessions:write`, `policies:read`
and `policies:write`. Keep it in the environment, not in a `.tf` file; the
provider marks it sensitive and never logs it.

## Try it without the API

`cmd/fakeapi` is an in-memory fake of the API, written from
`docs/contracts/policy/backend-api.yaml`:

```sh
nix develop -c make fakeapi      # http://127.0.0.1:18080, token bjs_fake_token
export BROWSERJS_ENDPOINT=http://127.0.0.1:18080 BROWSERJS_TOKEN=bjs_fake_token
```

Its policy check is rough (JSON syntax, top-level keys, operation names; a
Rego module's package line and brace balance). The real check is the policy
operator's.

## Develop

```sh
nix develop -c make test                                # go vet, unit tests
nix shell nixpkgs#opentofu -c nix develop -c make testacc   # real plans and applies
nix shell nixpkgs#opentofu -c nix develop -c make e2e       # the example, end to end
nix shell nixpkgs#opentofu -c nix develop -c make docs      # regenerate docs/
```

- **Unit tests** drive the provider over protocol 6 in process, as Terraform
  does, against the fake API. No `tofu` or `terraform` binary is involved.
- **Acceptance tests** (`TestAcc…`, skipped unless `TF_ACC=1`) run real
  plans, applies and imports. Against the fake by default; with
  `BROWSERJS_ENDPOINT` and `BROWSERJS_TOKEN` set, against that API. They
  create and delete sessions, so point them at a local deployment, never at
  production. For Terraform instead of OpenTofu:
  `TF_ACC=1 TF_ACC_TERRAFORM_PATH="$(command -v terraform)" go test ./internal/provider -run TestAcc -v`.
- **`hack/e2e.sh`** takes `examples/session-policies` through plan, apply, a
  policy edit, a policy that does not validate, and destroy, with
  `dev_overrides` and the fake.
- CI (`.github/workflows/terraform-provider.yml`) runs all three on pull
  requests that touch this directory, and fails if `docs/` is stale.
