# Use it from code

## Connect any MCP client

Live. A client needs the Streamable HTTP transport and OAuth sign-in in a
browser.

```json
{
  "mcpServers": {
    "desktop": {
      "type": "http",
      "url": "https://sessions.computeruse.site/s-abcde/mcp"
    }
  }
}
```

The exact keys depend on the client. On first use it opens a sign-in page;
use the account that owns the session.

Several clients can use one session. They share the desktop. Give each
agent its own browser tab with the `tab` parameter of `browser_execute`.

| What you see | Why |
| --- | --- |
| Calls answer 404 after sign-in | The account does not own the session, or the session was deleted |
| Calls answer 409 | The session is stopped. Start it in the app, or `POST /v1/sessions/{id}/wake` |
| A call answers 504 with `Retry-After` | The session was still waking. Try again |

## Without a person: API tokens

A script, a CI job or a service cannot use a sign-in page. It uses a token.

1. Create a token on the **API tokens** page of the app. Choose its scopes.
   The token is shown once. It starts with `bjs_`.
2. Call the API on `https://api.computeruse.site`:

```bash
# One call creates a desktop.
curl -X POST -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"name": "nightly"}' https://api.computeruse.site/v1/sessions
```

3. Point an MCP client at the session on the API host, with the token as a
   header:

```
URL:    https://api.computeruse.site/s-abcde/mcp
Header: Authorization: Bearer <token>
```

| Scope | Allows |
| --- | --- |
| `sessions:read` | List and read sessions |
| `sessions:write` | Create, rename, sleep, wake, stop, resume and delete sessions |
| `sessions:connect` | Call a session's MCP endpoint |
| `policies:read` | Read policies |
| `policies:write` | Change policies |

Give an agent a token with `sessions:connect` only. A token that can also
write policies lets the agent rewrite its own limits.

A token can also be exchanged for an access token of one hour, with the
OAuth client-credentials grant at `https://api.computeruse.site/oauth/token`.

## From a program: the SDK

::: warning Coming, not yet published
The SDK is built and tested, needs API tokens, and is in no package registry
yet. Until it is, build it from `sdk/` in the repository.
:::

One client in four languages. It exchanges the token, retries, waits for a
session that is waking, and calls `run_js` for you.

::: code-group

```rust [Rust]
use computeruse::Client;

let client = Client::builder()
    .api_token(std::env::var("COMPUTERUSE_API_TOKEN").unwrap())
    .build()?;

let session = client.sessions().create().name("demo").send().await?;
let result = session.run_js("console.log(6 * 7)".into()).await?;
println!("{}", result.output);
session.sleep().await?;
```

```python [Python]
import os
from computeruse import Client, CreateSessionRequest

client = Client.with_token(os.environ["COMPUTERUSE_API_TOKEN"])

session = await client.create_session(CreateSessionRequest(name="demo"))
result = await session.run_js("console.log(6 * 7)")
print(result.output)
await session.sleep()
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
import "github.com/r33drichards/computer-use/sdk/go/computeruse"

client, err := computeruse.ClientWithToken(os.Getenv("COMPUTERUSE_API_TOKEN"))

name := "demo"
session, err := client.CreateSession(computeruse.CreateSessionRequest{Name: &name})
result, err := session.RunJs("console.log(6 * 7)")
fmt.Print(result.Output)
_, err = session.Sleep()
```

:::

The token needs `sessions:write` to create and sleep, and `sessions:connect`
to run code. See the [SDK reference](/reference/sdk) for every call.

## As infrastructure: Terraform

::: warning Coming, not yet published
The provider works with the live API but is in no registry yet. Until it is,
build it from `terraform-provider-browserjs/` in the repository.
:::

```hcl
resource "browserjs_session" "research" {
  name = "research"
}

resource "browserjs_session_policy" "research" {
  session_id  = browserjs_session.research.id
  managed_url = "https://github.com/example/infra/tree/main/desktops"
  rego        = file("${path.module}/no-scripting.rego")
}
```

Keep `no-scripting.rego` beside the Terraform configuration. For example:

```txt
package browserjs.policy

import rego.v1

allow_tool_call if {
    input.server == "browser"
    input.tool == "browser_execute"
    is_array(input.arguments.operations)
    every op in input.arguments.operations {
        op.type in {"navigate", "click", "type", "press", "select", "wait",
                    "screenshot", "setViewport", "url"}
    }
}
```

This allows browser operations except scripting and replacing page content,
and refuses desktop control and the shell. The token needs `sessions:read`
and `sessions:write` for the session resource, and `policies:read` and
`policies:write` for its policy. Apply waits for the policy to be in force by
default. The app shows it read-only with a link to `managed_url`.

The provider and its resources keep the product's earlier name.
