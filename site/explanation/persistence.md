# What persists and what does not

Each session has its own disk. It lives as long as the session does.

## Kept until you delete the session

- **The browser profile**: cookies and logins, history, bookmarks, saved
  settings.
- **Open tabs.** Chromium reopens them when it starts.
- **Downloads**, and files you sent through the Files box.
- **Agent memory**: what an agent wrote under `/data/memory/`.
- **Artifacts** the agent stored.
- **The name and the MCP URL.** Renaming a session does not change its URL.

These survive sleep, stop and resume.

## Kept across sleep, when the snapshot is restored

- The exact state of the screen and of each page: form input, scroll
  position, a half-finished flow.

See [Sleep and wake](/explanation/sleep-and-wake) for when this does not
apply.

## Not kept

- **Page state after a stop**, or after a wake without a snapshot. Tabs are
  reloaded from their addresses.
- **Variables in `run_js`.** Every call starts fresh. An agent that needs to
  remember something writes it to `/data/memory/`.
- **The clipboard box.** Its text is in your browser tab only.
- **Anything, after delete.** Deleting a session removes its disk. It cannot
  be undone.

## Logins that expire anyway

A site can end a login on its own side: after some days, or when it sees a
new location. The session keeps the cookie, but the site may ask you to sign
in again. Do that in the live view.
