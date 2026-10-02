# terraform-provider-browserjs

A Terraform and OpenTofu provider for browserjs sessions and their policies.
Provider type `browserjs`, source address `r33drichards/browserjs`.

| | |
|---|---|
| `browserjs_session` (resource) | a session: one persistent browser, driven over MCP |
| `browserjs_session_policy` (resource) | a session's policy, managed as code: a Rego module |
| `browserjs_session`, `browserjs_sessions` (data sources) | sessions that already exist |

Reference for every argument: [`docs/`](docs/index.md), generated from the
schema. A complete configuration: [`examples/session-policies/`](examples/session-policies/main.tf).
How it behaves and why: [`../docs/terraform-provider.md`](../docs/terraform-provider.md).
The contract it is built to: [`../docs/contracts/policy/terraform-provider.md`](../docs/contracts/policy/terraform-provider.md).

A policy is Rego, and decides every tool call an agent makes: the browser
(`browser_execute`), desktop control (`desktop_execute`) and the shell (the
`exec` server). What a policy is asked is in
[`../docs/contracts/policy/rego-contract.md`](../docs/contracts/policy/rego-contract.md);
seven ready-made ones are in
[`../docs/contracts/policy/examples/`](../docs/contracts/policy/examples/).
A policy that restricts `browser_execute` must deny `desktop_execute` and the
`exec` server, since either can drive the browser around the rules; the API
answers with a warning when it does not, and the plan and the apply show it.

It is its own Go module, so none of its dependencies reach `backend/`.

## It is built on the SDK, and so needs cgo

The provider has no HTTP client of its own. It calls the API through the
Computer Use SDK's Go package ([`../sdk/go`](../sdk/go)), which is bindings
to a Rust library. `internal/client` turns the SDK's records and errors into
the provider's. What that means for building it:

| | Before | Now |
| --- | --- | --- |
| A build | `go build`, pure Go, `CGO_ENABLED=0` works | cgo, a C compiler, and `libcomputeruse.a` in `.lib/`. `make lib` builds it from `../sdk` with cargo (2 to 4 minutes the first time) |
| A bare `go build` or `go test` | works | fails at link until `make lib` has run and `CGO_LDFLAGS="-L$PWD/.lib"` is set. The make targets do both |
| The binary | static | the SDK is linked into it, so nothing is shipped beside it; it is larger, and it is linked to the system's C library (glibc on Linux) |
| Cross-compiling | `GOOS=... GOARCH=... go build` from any machine | not from one machine: each platform needs the Rust library built for it and a C toolchain for it. In practice one runner per platform |
| Platforms that can be released | everything Go targets | where the SDK's library is built: Linux x86_64 and aarch64 with glibc 2.35 or newer, macOS arm64 and x86_64. **Not** Windows, not musl (Alpine), not FreeBSD, not 32-bit |
| `go install ...@version` from outside the repository | worked | does not: `go.mod` points at `../sdk/go` with a `replace` |

The provider is released nowhere yet, so no released platform is lost today;
the table is what a release can have.

The provider sends the token itself on each request (it does not use the
SDK's token exchange), gets the SDK's retries of `429`, `502`, `503` and
`504`, and accepts an `http` endpoint with a warning, as before.

## Install locally

The provider is in no registry yet. Until it is, build it here and tell
OpenTofu or Terraform where it is. Go, a C compiler and cargo come from the
repository's `sdk` dev shell (`nix develop ..#sdk`, from this directory).
The first build also builds the SDK's library.

### For development: `dev_overrides`

```sh
nix develop ..#sdk -c make build      # ./terraform-provider-browserjs
```

In `~/.tofurc` for OpenTofu, `~/.terraformrc` for Terraform (or any file named
by `TF_CLI_CONFIG_FILE`, which both read):

```hcl
provider_installation {
  dev_overrides {
    "r33drichards/browserjs" = "/path/to/computer-use/terraform-provider-browserjs"
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
nix develop ..#sdk -c make install    # version 0.1.0 into ~/.terraform.d/plugins
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
export BROWSERJS_ENDPOINT=https://api.computeruse.site   # the default
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
nix develop ..#sdk -c make fakeapi      # http://127.0.0.1:18080, token bjs_fake_token
export BROWSERJS_ENDPOINT=http://127.0.0.1:18080 BROWSERJS_TOKEN=bjs_fake_token
```

Its policy check is rough: it reads a Rego module's text (the package line,
brace balance, that `allow_tool_call` is defined) and guesses the warnings
from which tools the module names. The real check is the policy operator's,
which compiles the module and asks it.

## Develop

```sh
nix develop ..#sdk -c make test                                # go vet, unit tests
nix shell nixpkgs#opentofu -c nix develop ..#sdk -c make testacc   # real plans and applies
nix shell nixpkgs#opentofu -c nix develop ..#sdk -c make e2e       # the example, end to end
nix shell nixpkgs#opentofu -c nix develop ..#sdk -c make docs      # regenerate docs/
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
  policy edit, a policy the API warns about, a policy that does not validate,
  and destroy, with `dev_overrides` and the fake.
- The `.rego` files under `examples/` are copies of the contract's
  (`../docs/contracts/policy/examples/`); a test fails when one drifts. Copy
  the file again rather than editing the copy.
- CI (`.github/workflows/terraform-provider.yml`) runs all three on pull
  requests that touch this directory, and fails if `docs/` is stale.
