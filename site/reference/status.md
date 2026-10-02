# Live, coming and planned

What works today, what is built but switched off, and what is only planned.
This page is the one to trust when another page is unclear.

## Live

- Sessions: create, rename, stop, resume, delete. Suggested two-word names.
- A live view of the desktop in the page, with full screen. The desktop
  follows the size of your window.
- An MCP endpoint per session, at
  `https://sessions.computeruse.site/<id>/mcp`, with sign-in through OAuth.
- `run_js`, and `browser_execute` for the desktop's browser.
- A persistent disk per session.
- Sleep after 15 minutes idle, and wake on use from a snapshot that restores
  the screen as it was.
- New sessions that start in seconds, from desktops kept ready.
- A clipboard box for text.
- File transfer in both directions, and pasting a sent file with `Ctrl+V`.

## Coming, not yet enabled

These are built and switched off. You cannot use them yet.

| Feature | What it will do |
| --- | --- |
| Policies per session | Say what an agent connected over MCP may ask the session to do, for example which sites it may open or which operations it may use. Written as JSON or Rego, in an editor in the app or managed as code. A policy restricts the agent, not you at the screen. |
| API tokens | Let a script, CI job or service use the API and a session's MCP endpoint without a sign-in page, on `https://api.computeruse.site`. A token can also be exchanged for a short-lived one with the OAuth client-credentials grant. |
| Terraform provider | Create sessions and manage their policies as code, with Terraform or OpenTofu. It needs API tokens. |

## Planned

Not built into the product yet.

| Feature | What it will do |
| --- | --- |
| Full desktop control (`desktop_execute`) | Let the agent use the mouse and keyboard on the whole desktop and take screenshots of the screen, so it can operate what is outside a web page: browser prompts, the file chooser, other windows. |

## Not planned at present

- Applications other than Chromium on the desktop, or a terminal.
- Sharing a session with another account.

Nothing on this page is a promise of a date.
