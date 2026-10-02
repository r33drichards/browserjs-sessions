# Create a desktop and connect an agent

In this tutorial you create a session, see its desktop, and connect Claude
to it. It takes about five minutes.

## 1. Sign in

Open [app.computeruse.site](https://app.computeruse.site) and sign in with
Google or GitHub. You see the **Sessions** list.

## 2. Create a session

1. Choose **Create session**.
2. Leave the name empty to get the suggested one, two words such as
   `brave-otter`.
3. Choose **Create**.

The session's page opens. Within seconds its state is
<span class="wf-state">running</span> and the page shows the desktop: a
wallpaper, and a panel along the bottom edge. No window is open yet.

## 3. Use the desktop yourself

Click the browser's icon on the panel: Chromium opens. Click into it and
open a website. This is a real desktop, and it is
the one the agent will use.

## 4. Copy the MCP URL

Choose **Copy MCP URL**. It looks like this:

```
https://sessions.computeruse.site/s-abcde/mcp
```

That one address is all an agent needs.

## 5. Connect Claude

**Claude (web or desktop).** Open the connector settings, add a custom
connector, and paste the MCP URL.

**Claude Code.** Run:

```bash
claude mcp add --transport http desktop https://sessions.computeruse.site/s-abcde/mcp
```

Then run `/mcp` and choose the server to sign in.

Claude opens a sign-in page. Use the account that owns the session.

## 6. Ask for something

Keep the session's page open so you can watch. Ask Claude:

> Using the desktop connector, open example.com and tell me the page's title.

Claude calls `run_js` once. On the screen, Chromium goes to the page (if
you had not opened Chromium, the call opens it first). Claude
answers with the title.

## 7. Leave it

Close the tab. You do not need to stop anything. After 15 minutes without
use the session goes to sleep, and Claude's next call wakes it as it was.

## What next

- [Your first run_js program](/tutorials/first-program)
- [Watch and take over](/guides/take-over)
