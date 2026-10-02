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

::: warning Coming, not yet enabled
API tokens are built and switched off. This section describes how they work
when they are on.
:::

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

## As infrastructure: Terraform

::: warning Coming, not yet enabled
The provider is built, needs API tokens, and is in no registry yet.
:::

```hcl
resource "browserjs_session" "research" {
  name = "research"
}

resource "browserjs_session_policy" "research" {
  session_id = browserjs_session.research.id
  json = jsonencode({
    version = 1
    allow   = { operations = ["*"] }
    deny    = { operations = ["evaluate", "setContent"] }
  })
}
```

The provider and its resources keep the product's earlier name.
