# Security model

This page says who can reach a session and what an agent can do with it, so
you can decide what to put in one.

## Who can reach a session

**You.** A session belongs to the account that created it. The app, the live
view, the files and the MCP endpoint all check that the caller is the owner.
To anyone else a session answers as if it did not exist.

**Agents you connect.** An MCP client reaches a session only after you sign
in to it with the owner's account. There is no shared secret in the MCP URL:
knowing the URL is not enough.

**The service's operators.** Administrators of browserjs can see and manage
all sessions. Treat a session like any hosted service: do not put in it what
you would not trust the operator with.

There is no way to share a session with another account.

Two things work without sign-in, by design, and are narrow:

- The live view's connection uses a ticket. The app gets it for you after
  checking you are the owner. It is valid for 30 seconds and one connection.
- An upload URL from `get_artifact_upload_url` is valid once, for 10
  minutes, and only stores one file for the agent to read.

## What an agent can do

An agent connected to a session can:

- operate the browser on the desktop: open pages, click, type, read page
  content, take screenshots, run scripts in pages;
- act as you on every site the browser is signed in to;
- read and write files under `/data/memory/` on the session's disk;
- store and read artifacts.

The logins are the powerful part. An agent with a session that is signed in
to your email can read and send email. Sign a session in only to what the
task needs, and use separate sessions for separate jobs.

## What an agent cannot do

- Its `run_js` code has no network access of its own and no file access
  outside `/data/memory/`. It reaches the web only through the browser.
- It cannot reach your other sessions. Each session is its own desktop, disk
  and endpoint.
- It cannot reach your computer. Files move only when you use the Files box,
  or when the agent uploads one from its own environment.
- It cannot create, stop or delete sessions. Those are done in the app.

## Restricting an agent further

Today every connected agent has the same abilities, listed above. Policies
per session, which let you narrow them (which sites, which operations), are
coming and are not enabled yet. See
[Live, coming and planned](/reference/status).

Until then, the controls you have are: what the session is signed in to,
watching the live view, and **Stop**.

## Isolation

Each session runs in its own sandbox, apart from other sessions and other
users, with its own disk. It can reach the public internet and nothing
inside the service. What a session's browser downloads is only ever handed
to you as a file to save, never shown as a page of the app.

## Good practice

- Watch the live view while an agent does something that matters.
- Stop a session to make sure no agent call can start it.
- Delete sessions you no longer need. That removes their logins too.
- Remove the connector from a client you stop using.
