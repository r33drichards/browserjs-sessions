# Computer Use SDK

A client for the API for code (`https://api.computeruse.site`) in four
languages, from one implementation. The core is a Rust crate; Python,
JavaScript and Go get the same client through
[UniFFI](https://mozilla.github.io/uniffi-rs/) bindings.

| Language | Package | Install |
| --- | --- | --- |
| Rust | [`computeruse-sdk`](crates/computeruse) on crates.io (the library is `computeruse`) | `cargo add computeruse-sdk` |
| Python | [`computeruse`](python) on PyPI | `pip install computeruse` |
| JavaScript, TypeScript | [`computeruse`](js) on npm, for Node 20+ | `npm install computeruse` |
| Go | [`sdk/go`](go), a module in this repository; needs cgo and a prebuilt library | `go get github.com/r33drichards/computer-use/sdk/go` |

Nothing is published yet: see [Publishing](#publishing).

## The same example, four times

Create a desktop, run JavaScript in it, put it to sleep. The token is an API
token (`bjs_...`) from the **API tokens** page of the app, with the scopes
`sessions:write` and `sessions:connect`.

Rust:

```rust
use computeruse::Client;

#[tokio::main]
async fn main() -> Result<(), computeruse::ComputerUseError> {
    let client = Client::builder()
        .api_token(std::env::var("COMPUTERUSE_API_TOKEN").unwrap())
        .build()?;

    let session = client.sessions().create().name("demo").send().await?;
    let result = session.run_js("console.log(6 * 7)".into()).await?;
    println!("{}", result.output);
    session.sleep().await?;
    Ok(())
}
```

Python:

```python
import asyncio, os
from computeruse import Client, CreateSessionRequest

async def main():
    client = Client.with_token(os.environ["COMPUTERUSE_API_TOKEN"])

    session = await client.create_session(CreateSessionRequest(name="demo"))
    result = await session.run_js("console.log(6 * 7)")
    print(result.output)
    await session.sleep()

asyncio.run(main())
```

JavaScript (TypeScript types included):

```ts
import { Client } from "computeruse";

const client = Client.withToken(process.env.COMPUTERUSE_API_TOKEN!);

const session = await client.createSession({ name: "demo" });
const result = await session.runJs("console.log(6 * 7)");
console.log(result.output);
await session.sleep();
```

Go:

```go
package main

import (
	"fmt"
	"log"
	"os"

	"github.com/r33drichards/computer-use/sdk/go/computeruse"
)

func main() {
	client, err := computeruse.ClientWithToken(os.Getenv("COMPUTERUSE_API_TOKEN"))
	if err != nil {
		log.Fatal(err)
	}

	name := "demo"
	session, err := client.CreateSession(computeruse.CreateSessionRequest{Name: &name})
	if err != nil {
		log.Fatal(err)
	}
	result, err := session.RunJs("console.log(6 * 7)")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Print(result.Output)
	if _, err := session.Sleep(); err != nil {
		log.Fatal(err)
	}
}
```

## What is in it

| | Calls | Scope |
| --- | --- | --- |
| Client | `me`, `sizes`, `access_token`, `base_url` | any |
| Sessions | `list_sessions`, `get_session`, `session(id)` (a handle, no request), `create_session` | `sessions:read`, `sessions:write` |
| A session | `refresh`, `rename`, `resize`, `stop`, `resume`, `sleep`, `wake`, `delete`, `wait_until`, `wait_until_running`, `last_info`, `mcp_url` | `sessions:read`, `sessions:write` |
| Its policy | `policy`, `put_policy`, `reset_policy`, `set_policy_management` | `policies:read`, `policies:write` |
| Policies | `policy_presets`, `validate_policy`, `evaluate_policy` | any |
| Its MCP endpoint | `run_js`, `run_js_with`, `call_tool`, `list_tools` | `sessions:connect` |

Names are in each language's style: `run_js` in Rust and Python, `runJs` in
JavaScript, `RunJs` in Go. In Rust the same calls are also grouped behind
borrowing builders: `Client::builder()`, `client.sessions().create()`.

Behaviour that is the same everywhere, because it is one implementation:

- **Token exchange.** The API token is exchanged for an access token of an
  hour (`POST /oauth/token`, client credentials); requests carry that. It is
  replaced a minute before it expires and at once after a `401`. With
  `exchange_token` off the API token is sent itself. A token that is not an
  API token (an access token made elsewhere) is sent as it is.
- **Retries.** `429`, `502`, `503`, `504` and failed connections are tried
  again, three times by default, with a pause that doubles from half a
  second, or the pause `Retry-After` asks for (30 seconds at most). A create
  is tried again only after `429`, `503` or a connection that was never
  made, so that it cannot make two sessions. An MCP call that finds the
  session still waking (`504` with `Retry-After`) is tried again for up to
  ten minutes.
- **Timeouts.** 60 seconds for an API request; 630 for an MCP request (the
  service holds a call for up to 300 while a session wakes, and `run_js` may
  then run for 300); three minutes at least for `sleep`, which answers when
  the snapshot is taken.
- **Typed errors.** `Unauthorized` (401), `PaymentRequired` (402, with the
  `code` and the billing URL), `Forbidden` (403), `NotFound` (404),
  `Conflict` (409, with `managed_url` for a policy managed elsewhere; 403
  and 409 carry `code` and the billing URL too when billing refused),
  `InvalidPolicy` (422, with the diagnostics), `RateLimited` (429), `Api`
  (any other status), and `Configuration`, `Transport`, `Timeout`,
  `Decode`, `Mcp`, `Tool`, `SessionFailed`.
- **Typed builders.** Every record a caller fills has a generated builder
  (`ClientOptionsBuilder`, `CreateSessionRequestBuilder`,
  `RunJsRequestBuilder`, `PolicyInputBuilder`, `ManagementBuilder`). A setter
  returns a new builder; `build()` names a required field that was left out.
- **No secrets in output.** The client, its options and its builders do not
  print the token; no error carries it.
- **TLS** is rustls with the platform's trust roots. There is no OpenSSL.
  `http` is accepted for a loopback address only, unless
  `allow_insecure_http` is set.
- **Lists the API writes as `null`** (a Go server's empty list) are empty
  lists.

Not in it: billing endpoints (they are switched off in the service), making
and revoking API tokens (the API takes the browser's cookie only for that),
file transfer and the live view (the app's API only).

`run_js` returns the program's `output`, its `error` if it threw or timed
out (the call itself succeeded), and its `artifacts` as bytes.

## Layout

```
sdk/
  Cargo.toml                    the workspace; the one version
  crates/computeruse/           the client (crate computeruse-sdk, library computeruse)
  crates/computeruse-macros/    #[derive(UniffiBuilder)] (crate computeruse-sdk-macros)
  crates/uniffi-bindgen/        the bindings generator at the pinned UniFFI version
  python/                       pyproject.toml for maturin, py.typed, the smoke test
  js/                           the npm package: generated TypeScript, the smoke test
  go/                           the Go module: generated Go and C header, the smoke test
  scripts/bindings.sh           generate, check and smoke-test one language
```

## Working on it

Cargo runs through the flake's `sdk` dev shell:

```bash
cd sdk
nix develop ..#sdk -c cargo test                    # against a fake API in the test process
nix develop ..#sdk -c cargo clippy --workspace --all-targets -- -D warnings
nix develop ..#sdk -c scripts/bindings.sh python --test
nix develop ..#sdk -c scripts/bindings.sh javascript --test   # also rewrites js/src/generated
nix develop ..#sdk -c scripts/bindings.sh go --test           # also rewrites go/computeruse/computeruse.{go,h}
```

No test talks to the real service. The Rust tests start a fake HTTP server in
the test process; each language's smoke test starts its own and makes real
calls through the built library: the token exchange, create, `run_js` over
MCP, sleep, and a typed error.

### Against the real API

`scripts/live.sh` runs the same scenario in each language against the real
service: the token exchange, `me`, create, wait until running, `run_js`
(console output, a `browser_execute` call, an `exec` call followed by
`stream_logs`), the tools, the policy (read it, put the `browser-only` preset
and see `exec` denied, put `unrestricted` back), sleep with a snapshot, wake,
rename, delete. Each language creates one session and deletes it, whatever
happens in between.

```bash
nix develop ..#sdk -c scripts/live.sh            # all four
nix develop ..#sdk -c scripts/live.sh rust go    # some
```

It needs an API token with every scope. It takes it from
`COMPUTERUSE_API_TOKEN`, or on macOS from the Keychain item
`computeruse-api-token`
(`security add-generic-password -s computeruse-api-token -a computeruse -w`),
and passes it to the tests in their environment only. `COMPUTERUSE_BASE_URL`
names another deployment. The tests are skipped unless `COMPUTERUSE_LIVE=1`,
which the script sets; CI never runs them, since it has no token.

After changing the exported surface (anything under `#[uniffi::export]`, a
`uniffi::Record`, an enum, **or a doc comment on one of them**: UniFFI's
checksums cover the docs, and stale bindings refuse to load), run `scripts/bindings.sh go` and
`scripts/bindings.sh javascript` and commit what they write. CI
(`.github/workflows/sdk.yml`) fails when the generated sources in the tree
are not what the generators produce, and uploads what they produced. Python
has nothing generated in the tree: maturin generates the module when it
builds the wheel.

### Versions that move together

UniFFI is pinned to `=0.31.0` in `Cargo.toml`. The Go and JavaScript
generators each follow one UniFFI release, and the generated code checks at
load that the library was built with the same one:

| Generator | Version | For |
| --- | --- | --- |
| `uniffi-bindgen` (this workspace) | 0.31.0 | Python |
| [`uniffi-bindgen-go`](https://github.com/NordSecurity/uniffi-bindgen-go) | `v0.7.1+v0.31.0` | Go |
| [`uniffi-bindgen-react-native`](https://github.com/jhugman/uniffi-bindgen-react-native), Node (N-API) target | `0.31.0-6`, with `@ubjs/core` and `@ubjs/node` at the same version | JavaScript |

UniFFI 0.32 exists; `uniffi-bindgen-go` has no release for it yet. Move all
three together.

## Publishing

`.github/workflows/sdk-release.yml` builds for Linux x86_64 and aarch64
(glibc 2.35 and newer) and macOS arm64 and x86_64, and publishes when a tag
`sdk-v<version>` is pushed. Run by hand it only builds, unless `publish` is
ticked. It has not been run: nothing is published and no name is reserved.

Names, checked on 2026-10-02:

| Registry | Name | State |
| --- | --- | --- |
| crates.io | `computeruse-sdk`, `computeruse-sdk-macros` | free. `computeruse` is taken by an unrelated placeholder (0.0.1), which is why the crate has a suffix and the library does not |
| PyPI | `computeruse` | free |
| npm | `computeruse` | free |
| Go | `github.com/r33drichards/computer-use/sdk/go` | the repository's path; nothing to reserve |

Before the first release, once:

1. **crates.io**: sign in, make an API token with the scope `publish-new`
   and `publish-update`, and save it as the repository secret
   `CARGO_REGISTRY_TOKEN`.
2. **PyPI**: on <https://pypi.org/manage/account/publishing/> add a pending
   trusted publisher: project `computeruse`, owner `r33drichards`, repository
   `computer-use`, workflow
   `sdk-release.yml`, environment `pypi`. No secret is needed. To use a token
   instead, save it as `PYPI_API_TOKEN`.
3. **npm**: make a granular access token that may publish, and save it as
   `NPM_TOKEN`.

Then, for each release:

1. Set the version in `sdk/Cargo.toml` (`[workspace.package]`), in the
   `computeruse-sdk-macros` dependency of `sdk/crates/computeruse/Cargo.toml`
   and in `sdk/js/package.json`; run `cargo check` so `Cargo.lock` follows.
   Merge.
2. `git tag sdk-v<version> && git push origin sdk-v<version>`.

The workflow refuses a tag that is not the tree's version. It publishes the
two crates (the macros first), the four wheels, and one npm package that
holds all four libraries; it makes the GitHub release `sdk-v<version>` with
the Go archives, and pushes the tag `sdk/go/v<version>`, which is what
`go get` resolves a module in a subdirectory by.

A release that fails part-way can be run again for npm and the GitHub
release. crates.io and PyPI refuse a version that exists: fix forward with a
new version.

### If the repository is renamed again

The repository was `r33drichards/browserjs-sessions` until October 2026. The
lines that name it:

| File | Line |
| --- | --- |
| `sdk/go/go.mod` | `module github.com/r33drichards/computer-use/sdk/go` |
| `sdk/crates/computeruse/uniffi.toml` | `go_mod = ...` |
| `sdk/go/computeruse/smoke_test.go` | the import |
| `sdk/go/install-lib.sh` | `REPO=` |
| `sdk/Cargo.toml` | `repository = ...` |
| `sdk/python/pyproject.toml` | `Repository = ...` |
| `sdk/js/package.json` | `repository.url` |
| `sdk/README.md`, `sdk/go/README.md`, `site/reference/sdk.md`, `site/guides/use-from-code.md` | the Go import path in the examples |

A Go module path is part of every import of it: after the first Go release,
a rename breaks everyone who imports the module.

## Licence

Apache-2.0, for the crates, the wheel, the npm package and the Go module.
