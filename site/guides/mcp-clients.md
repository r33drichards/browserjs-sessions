# Connect another MCP client

Any MCP client that can use a remote server works, if it supports:

- the Streamable HTTP transport, and
- signing in with OAuth in a browser.

## Steps

1. Copy the session's **MCP URL** from its page in the app.
2. Add a remote (HTTP) MCP server in your client with that URL. No API key
   or header is needed.
3. When the client opens a sign-in page, use the Google or GitHub account
   that owns the session.
4. List the server's tools. You should see `run_js`.

A typical client configuration:

```json
{
  "mcpServers": {
    "browserjs": {
      "type": "http",
      "url": "https://s-abcde.sessions.browserjs.com/mcp"
    }
  }
}
```

The exact keys depend on the client.

## Several clients, one session

You can add the same MCP URL to more than one client. They share one
browser. Give each agent its own tab with the `tab` parameter of
`browser_execute`; see [MCP endpoint and tools](/reference/mcp).

## Troubleshooting

| What you see | Why |
| --- | --- |
| Sign-in succeeds, then calls answer 404 | The account is not the session's owner, or the session was deleted. |
| Calls answer 409 | The session is stopped. Resume it in the app. |
| A call answers 504 with `Retry-After` | The session was still waking. Try again. |
