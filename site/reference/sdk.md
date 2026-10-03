# SDK: Rust, Python, JavaScript, Go

::: warning Coming, not yet published
The SDK is built and tested against a fake of the API. It works with the
live API using API tokens, but is in no package registry yet. Until it is,
build it from `sdk/` in the repository.
:::

One client for the [API for code](/reference/api) in four languages. The
client is written once, in Rust; the other three are generated bindings to
it, so they behave alike.

| Language | Package | Runs on |
| --- | --- | --- |
| Rust | `computeruse-sdk` on crates.io. The library is `computeruse` | Any target of `reqwest` and `tokio` |
| Python | `computeruse` on PyPI | Python 3.9 or newer |
| JavaScript, TypeScript | `computeruse` on npm | Node 20 or newer. Not browsers |
| Go | `github.com/r33drichards/computer-use/sdk/go` | Go 1.22 or newer, with cgo |

The Python, JavaScript and Go packages carry a native library. It is built
for Linux x86_64 and arm64 (glibc 2.35 or newer) and macOS arm64 and x86_64.

## Example

Create a desktop, run JavaScript in it, put it to sleep.

::: code-group

```rust [Rust]
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

```python [Python]
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

```ts [JavaScript]
import { Client } from "computeruse";

const client = Client.withToken(process.env.COMPUTERUSE_API_TOKEN!);

const session = await client.createSession({ name: "demo" });
const result = await session.runJs("console.log(6 * 7)");
console.log(result.output);
await session.sleep();
```

```go [Go]
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

:::

The token is an API token from the **API tokens** page of the app. This
example needs the scopes `sessions:write` and `sessions:connect`.

Names follow each language: `run_js` in Rust and Python, `runJs` in
JavaScript, `RunJs` in Go. This page uses the Rust and Python names.

## Client options

Only the token is required.

| Option | Default | Meaning |
| --- | --- | --- |
| `api_token` | | An API token (`bjs_...`), or an access token made from one |
| `base_url` | `https://api.computeruse.site` | The API host, without a path. `http` is accepted for `localhost` only |
| `scopes` | all of the token's | Narrows the access token to these scopes |
| `exchange_token` | `true` | Exchange the API token for an access token of one hour and send that. `false` sends the API token on every request |
| `timeout_ms` | 60 000 | Time limit of one API request |
| `mcp_timeout_ms` | 630 000 | Time limit of one MCP request. A call can wait 300 seconds for a wake and then run for 300 |
| `max_retries` | 3 | Tries after the first, for `429`, `502`, `503`, `504` and failed connections |
| `retry_base_delay_ms` | 500 | The first pause between tries. It doubles each time. `Retry-After` is used instead when the API sends it |
| `wake_timeout_ms` | 600 000 | How long an MCP call keeps trying while the session wakes |
| `user_agent` | | Put in front of the SDK's own `User-Agent` |
| `allow_insecure_http` | `false` | Accept an `http` base URL that is not `localhost`. The token is then sent unencrypted |

In Rust, `Client::builder()` takes these with `Duration` values. In the
other languages, build a `ClientOptions` with `ClientOptionsBuilder`, or use
`Client.with_token(token)` for the defaults.

## Calls

### The client

| Call | Does | Scope |
| --- | --- | --- |
| `me()` | Who the token acts as | any |
| `list_sessions()` | Your sessions | `sessions:read` |
| `sizes()` | The sizes a session can have, and the default | any |
| `create_session(request)` | Creates a session and returns a handle. The request has an optional `name`, an optional `size`, and an optional `policy` or `policy_preset` | `sessions:write` |
| `get_session(id)` | Reads a session and returns a handle | `sessions:read` |
| `session(id)` | A handle, without a request. For a token that has `sessions:connect` only | none |
| `policy_presets()` | The ready-made policies | any |
| `validate_policy(source)` | Checks Rego without saving it | any |
| `evaluate_policy(source, input_json)` | Asks a policy about one sample call | any |
| `access_token(force_refresh)` | The bearer token the next request would carry | any |

`create_session` does not wait for the desktop. The session is `starting`.
`run_js` waits for it; so does `wait_until_running`.

### A session

| Call | Does | Scope |
| --- | --- | --- |
| `id()`, `mcp_url()` | The id, and the MCP URL on the API host | none |
| `last_info()` | What the API last said about the session. No request | none |
| `refresh()` | Reads the session | `sessions:read` |
| `rename(name)` | Renames it | `sessions:write` |
| `resize(size)` | Changes its size. An awake session changes at its next start (`pending_size`). The next start is fresh: the snapshot is dropped, the disk is kept | `sessions:write` |
| `sleep()` | Sleeps it now, with a snapshot. Answers when the snapshot is taken | `sessions:write` |
| `wake()` | Starts a session that is asleep or stopped. Does not wait | `sessions:write` |
| `stop()` | Stops it. No snapshot. An agent's call does not wake it | `sessions:write` |
| `resume()` | Starts a stopped session, or wakes a sleeping one | `sessions:write` |
| `delete()` | Deletes it and its disk | `sessions:write` |
| `wait_until(state, timeout_ms)`, `wait_until_running(timeout_ms)` | Polls once a second until the state. Five minutes by default | `sessions:read` |
| `policy()` | Reads the policy | `policies:read` |
| `put_policy(policy)` | Replaces the policy | `policies:write` |
| `reset_policy()` | Returns it to unrestricted | `policies:write` |
| `set_policy_management(management)` | Changes who manages the policy | `policies:write` |
| `run_js(code)` | Runs JavaScript or TypeScript in the session | `sessions:connect` |
| `run_js_with(request)` | The same, with a heap limit and a time limit | `sessions:connect` |
| `call_tool(name, arguments_json)` | Calls any tool of the session's MCP server | `sessions:connect` |
| `list_tools()` | Lists the tools | `sessions:connect` |

See [Session lifecycle](/reference/lifecycle) for the states, and
[MCP endpoint and run_js](/reference/mcp) for what the code can use.

### What `run_js` returns

| Field | Meaning |
| --- | --- |
| `output` | What the code printed |
| `error` | Set when the code threw or ran out of time. The call itself succeeded |
| `artifacts` | What the code attached with `artifact(...)`, each with `kind`, `mime_type` and `data` (bytes) or `text` |
| `raw_json` | The tool's whole answer |

A sleeping session wakes on the call. A stopped session answers `Conflict`.

## Errors

Each documented answer of the API is its own error.

| Error | When |
| --- | --- |
| `Unauthorized` | `401`. The token is wrong, revoked or expired |
| `PaymentRequired` | `402`. Billing refused. Carries `code` and `billing_url` |
| `Forbidden` | `403`. The token lacks the scope, or is for another session, or billing blocked the account (`code`, `billing_url`) |
| `NotFound` | `404`. No such session, or not yours |
| `Conflict` | `409`. The session limit, a stopped session, a session that cannot sleep, no room for the size asked for (`code` is `no_capacity`), a policy managed elsewhere (`managed_url`), or a plan's limit (`code`, `billing_url`) |
| `InvalidPolicy` | `422`. The policy does not validate. Carries the diagnostics. Nothing was saved |
| `RateLimited` | `429`, after the retries |
| `Api` | Any other status, with `status` and `message` |
| `Configuration` | The client was given something it cannot use |
| `Transport` | No answer: DNS, TLS, a refused or dropped connection |
| `Timeout` | No answer in time, or a wait that ran out |
| `Decode` | A success that was not the JSON expected |
| `Mcp` | The session's MCP server answered a JSON-RPC error |
| `Tool` | A tool reported that it failed |
| `SessionFailed` | The session went to `failed` while it was waited for |

| Language | How to tell them apart |
| --- | --- |
| Rust | `match` on `ComputerUseError` |
| Python | `except ComputerUseError.NotFound:` |
| JavaScript | `ComputerUseError.NotFound.instanceOf(error)`; the fields are in `error.inner` |
| Go | `errors.Is(err, computeruse.ErrComputerUseErrorNotFound)`, or `errors.As` with `*computeruse.ComputerUseErrorNotFound` |

## What the client does on its own

- **Exchanges the token.** It asks `POST /oauth/token` for an access token
  and sends that. It asks again one minute before the token expires, and at
  once after a `401`.
- **Retries.** `429`, `502`, `503`, `504` and failed connections. A create
  is retried only after `429`, `503` or a connection that was never made, so
  it cannot create two sessions.
- **Waits for a wake.** An MCP call that finds the session still waking is
  tried again until `wake_timeout_ms`.
- **Keeps the token out of output.** The client, its options and its
  errors do not print it.

## Builders

Each record a caller fills has a builder with the same name and `Builder`
at the end. A setter returns a new builder and leaves the old one as it was.
`build()` fails with `MissingRequiredField` when a required field is
missing.

::: code-group

```python [Python]
request = (CreateSessionRequestBuilder()
    .name("nightly")
    .policy_preset("no-scripting")
    .build())
```

```ts [JavaScript]
const request = new CreateSessionRequestBuilder()
  .name("nightly")
  .policyPreset("no-scripting")
  .build();
```

```go [Go]
request, err := computeruse.NewCreateSessionRequestBuilder().
	Name("nightly").
	PolicyPreset("no-scripting").
	Build()
```

```rust [Rust]
let session = client
    .sessions()
    .create()
    .name("nightly")
    .policy_preset("no-scripting")
    .send()
    .await?;
```

:::

`policy_preset` names one of the presets of `policy_presets()`. The client
reads the preset and sends its source.

## Per language

| | Async | 64-bit numbers | Optional fields |
| --- | --- | --- | --- |
| Rust | `async`, on `tokio` | `u64` | `Option` |
| Python | Coroutines, on `asyncio` | `int` | `None` |
| JavaScript | Promises | `bigint`, for example `30_000n` | `undefined` |
| Go | Calls block. Use goroutines | `uint64` | pointers |

### Go needs cgo and a library

The Go package calls a C library, `libcomputeruse`. A build needs cgo on and
the library's place in `CGO_LDFLAGS`. Each release has a static library per
platform:

```bash
go get github.com/r33drichards/computer-use/sdk/go@v0.1.0
curl -fsSL https://raw.githubusercontent.com/r33drichards/computer-use/main/sdk/go/install-lib.sh | sh -s -- 0.1.0 ./lib
export CGO_LDFLAGS="-L$PWD/lib"
go build ./...
```

## Not in the SDK

| | Why |
| --- | --- |
| Billing | The billing endpoints are switched off in the service |
| Making and revoking API tokens | The API accepts only the app's sign-in for that |
| Files, clipboard and the live view | They are on the app's API only |
| Windows, Alpine (musl), browsers | No library is built for them yet |
