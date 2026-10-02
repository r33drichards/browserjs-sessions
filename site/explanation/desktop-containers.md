# Serverless, resumable desktop containers

The product is one thing: a desktop container that keeps its state and runs
only while it is used. This page explains each word.

## Desktop container

A session is a container with a display. It runs an X server, a window
manager and applications, in a sandbox of its own. What you see in the app
is that display, live. It is not a recording and not a headless browser.

An agent needs this because much software has no API. It has a screen. A
desktop gives the agent the same surface a person has: windows, a pointer, a
keyboard, a clipboard.

The desktop is XFCE, with Chromium, a terminal, a file manager and a text
editor. It starts empty: the browser opens when the agent first uses it, or
when you start it from the panel. See
[Capabilities](/reference/capabilities).

## Stateful

A stateless sandbox starts empty each time. That is right for running a
function and wrong for using a computer. Using a computer builds up state:
you sign in, open things, arrange them, get halfway.

A session keeps state at two levels.

- **The disk.** The browser's profile and logins, downloaded files, and the
  agent's own notes under `/data/memory/`. The disk lasts until the session
  is deleted.
- **The running desktop.** Open windows, the page a form is on, a process in
  the middle of its work. This lasts as long as the desktop runs, and across
  sleep.

## Resumable

When a session goes to sleep, a snapshot is taken of the whole running
container: its memory as well as its files. Then the container is removed.

When the session wakes, the container is restored from the snapshot. The
processes continue where they were. The screen shows what it showed. An
agent that comes back the next day finds its task where it left it.

This differs from restarting. A restart gives you the disk and a fresh
desktop: the browser, when it is next opened, reopens its tabs and reloads
them, logins survive, and
whatever lived only in memory is gone. That is what **Stop** and **Start**
do, and what a wake falls back to if a snapshot could not be taken or
restored.

## Serverless, scale to zero

You do not run a server for a session, and you do not start or stop it.

- A session that is not used for 15 minutes goes to sleep by itself.
- Asleep, it uses no compute. Only the disk and the snapshot are stored.
- The next MCP call wakes it. The call waits, then answers. The agent sees a
  slow call, not an error.

So the compute a session uses follows how much it is used, not how long it
has existed.

"Use" means a call in progress, or the screen open in a visible tab. A
client that is merely connected does not count. Many MCP clients hold a
connection open all day; if that counted, nothing would ever sleep.

New sessions are fast for a related reason. The service keeps a few desktops
running that belong to nobody yet, and a new session takes one. Creating a
session then takes seconds.

## Simple

There is one kind of resource. A session is the container, its disk, its
snapshot and its MCP URL together. You create it, and later you delete it.
There are no images to build, no volumes to attach and no ports to open.

The lifecycle in full is in [Session lifecycle](/reference/lifecycle).
