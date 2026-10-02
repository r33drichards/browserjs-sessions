# Computer use on a Linux desktop

"Computer use" means an AI agent operating a computer the way a person does:
looking at a screen and acting on it. This page explains how Computer Use
approaches that, and where it stops today.

## Why give the agent its own computer

An agent that uses your own laptop has your files, your accounts and your
screen. It also needs the laptop to stay open, and you cannot use it
meanwhile.

A session is a separate machine. It has only the logins you put there. It
runs in the cloud, so a long task continues when you close your laptop, and
you can look in from anywhere. If a task goes wrong, the damage is limited
to that session, and you can delete it.

## What the machine is

Each session is a real Linux desktop in a sandbox: an X display, a window
manager and Chromium. The live view shows that display. It is not a
recording or a rendering of a headless browser; your clicks and the agent's
actions land on the same screen.

It is a small desktop on purpose. Most work an agent does for people today
happens in a browser, and one application that is always there is easier to
keep working, to snapshot and to secure than a full workstation. There is no
terminal and nothing else installed; see
[What is on the desktop](/reference/desktop).

## How the agent acts

The agent connects over MCP and gets a tool called `run_js`. Instead of one
tool call per click, it writes a short JavaScript program. The program calls
`browser_execute` to navigate, click, type, read the page and take
screenshots, and prints what the agent needs to know.

This has two effects. A task of twenty steps can be one call, which is
faster and uses less of the model's context. And the agent can keep notes on
the session's disk, such as which selectors worked on a site, and read them
next time.

## Where it stops today

Today the agent acts on web pages, by selector and script. It does not yet
see the whole desktop or move the mouse across it. So it cannot operate what
lies outside a page: a file chooser, a permission prompt, Chromium's own
menus.

Two things cover the gap:

- **You.** The live view gives a person full control. Take over for that
  step; see [Watch an agent and take over](/guides/take-over).
- **Files by another route.** A file sent through the Files box is put on
  the desktop's clipboard, so it can be pasted into a page without the file
  chooser.

Control of the whole desktop by mouse, keyboard and screenshot is planned.
That is what will make a session a computer-use target in the full sense.
See [Live, coming and planned](/reference/status).

## A person in the loop

The design assumes a person is nearby. Logins are done by you, once, in the
live view, and kept on the disk. The agent never needs your passwords. When
it gets stuck, you can see why, because its screen is your screen.
