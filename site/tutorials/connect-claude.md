# Connect a session to Claude

In this tutorial you add a session to Claude as a connector, and watch Claude
use the browser. You need a session; see
[Your first session](/tutorials/first-session).

## 1. Copy the MCP URL

Open the session in [the app](https://app.browserjs.com). Under **Details**,
choose **Copy** next to the MCP URL. It looks like this:

```
https://s-abcde.sessions.browserjs.com/mcp
```

## 2. Add it to Claude

**Claude (web or desktop).** Open the connector settings, add a custom
connector, and paste the MCP URL. Give it a name, such as the session's name.

**Claude Code.** Run:

```bash
claude mcp add --transport http browserjs https://s-abcde.sessions.browserjs.com/mcp
```

Then run `/mcp` in Claude Code and choose the server to sign in.

## 3. Sign in

Claude opens a sign-in page. Use the same Google or GitHub account that owns
the session. Only the owner can connect.

## 4. Ask for something

Keep the session's page open in another window so you can see the screen.
Then ask Claude:

> Using the browserjs connector, open example.com and tell me the page's title.

Claude calls the `run_js` tool. In the live view the browser goes to the
page. Claude answers with the title.

## 5. Work together

Try a site that needs a login. Sign in yourself in the live view, then ask
Claude to continue. It works in the same browser, so it is signed in too.

## What next

- [MCP endpoint and tools](/reference/mcp): what the agent can call.
- [Security model](/explanation/security): what the agent can and cannot do.
- [Connect another MCP client](/guides/mcp-clients)
