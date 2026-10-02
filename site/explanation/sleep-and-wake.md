# Sleep and wake

A browser uses memory and CPU even when nobody looks at it. So a session
that is not being used goes to sleep, and comes back when it is needed. This
page describes what that looks like from your side.

## When a session sleeps

A session sleeps after 15 minutes without use. Use means a live view that is
open and visible, an MCP call in progress, or a file being moved. A session
page left in a background tab stops counting after a minute.

An MCP client that is merely connected does not keep a session awake. Many
clients hold a connection open all day; if that counted, no session would
ever sleep.

## What is saved

Before it sleeps, the session takes a snapshot of the running browser: its
memory as well as its files. Waking restores that snapshot. The pages come
back as they were, including what you had typed into a form and where you
had scrolled to.

Sometimes there is no snapshot to restore, for example because taking it
failed. The session then starts the browser afresh from its disk. Chromium
reopens the tabs it had and reloads them. Logins are kept, because cookies
are on the disk. What existed only in a page's memory is gone.

## Waking

Any real use wakes a sleeping session:

- **Wake** in the app,
- an MCP tool call,
- sending, saving or deleting a file.

The caller waits while the session wakes. An agent sees a slow call, not an
error. If waking takes too long, the call answers with an error that says to
retry.

## Stop is different

**Stop** is you saying the session should stay off. A stopped session does
not wake for an agent. Only **Resume** starts it.

Stop also takes no snapshot. Resuming starts the browser from disk, as in
the case above: tabs reload, logins are kept.

## New sessions start quickly

The service keeps a few browsers running that belong to nobody yet. A new
session takes one of them, so it is usually ready in seconds. When none is
free, the session starts from scratch, which takes longer.
