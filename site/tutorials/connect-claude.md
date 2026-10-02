# Connect an agent with Claude

In this tutorial you add a session to Claude as a connector, and watch Claude
work on the desktop. You need a session; see
[Your first desktop](/tutorials/first-session).

## 1. Copy the MCP URL

Open the session in [the app](https://app.computeruse.site) and choose
**Copy MCP URL**. It looks like this:

```
https://sessions.computeruse.site/s-abcde/mcp
```

## 2. Add it to Claude

**Claude (web or desktop).** Open the connector settings, add a custom
connector, and paste the MCP URL. Give it a name, such as the session's name.

**Claude Code.** Run:

```bash
claude mcp add --transport http desktop https://sessions.computeruse.site/s-abcde/mcp
```

Then run `/mcp` in Claude Code and choose the server to sign in.

## 3. Sign in

Claude opens a sign-in page. Use the same Google or GitHub account that owns
the session. Only the owner can connect.

## 4. Ask for something

Keep the session's page open in another window so you can see the screen.
Then ask Claude:

> Using the desktop connector, open example.com and tell me the page's title.

Claude calls the `run_js` tool with a few lines of JavaScript. On the screen,
Chromium goes to the page. Claude answers with the title.

## 5. Take over, then hand back

Ask Claude to do something on a site that needs a login. When it reaches the
sign-in page, click into the screen and sign in yourself. Then tell Claude to
continue. It works on the same desktop, so it is signed in too.

## What next

- [Watch an agent and take over](/guides/take-over)
- [MCP endpoint and tools](/reference/mcp): what the agent can call.
- [Security model](/explanation/security): what the agent can and cannot do.
