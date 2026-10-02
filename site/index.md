---
layout: home
title: browserjs
titleTemplate: A Linux desktop for computer-use agents

hero:
  name: browserjs
  text: A Linux desktop for your agent.
  tagline: Each session is a small Linux desktop in the cloud with Chromium open on it. An AI agent operates it over MCP. You watch it in the page and take over when you want.
  actions:
    - theme: brand
      text: Your first desktop
      link: /tutorials/first-session
    - theme: alt
      text: Connect an agent
      link: /tutorials/connect-claude
    - theme: alt
      text: Open the app
      link: https://app.computeruse.site

features:
  - title: A desktop of its own, not your laptop
    details: The agent works on a machine in the cloud. Your own screen, files and logins are not involved, and your computer can be closed.
  - title: Watch long tasks and take over
    details: The agent's work happens on a screen you can see. When it meets a login or a captcha, do that step with your own mouse and let it carry on.
  - title: One isolated desktop per task or client
    details: Every session has its own sandbox, disk and endpoint. Keep accounts and customers apart, and delete a session when the job is done.
  - title: Logins and state kept between runs
    details: The disk persists. An idle session sleeps, and the next use wakes it with the screen as it was.
  - title: Driven from Claude, any MCP client, or code
    details: Add the session's MCP URL as a connector. The agent writes JavaScript with run_js, and that code drives the desktop's browser.
---

## What a session is

A session is a small Linux desktop: an X display, a window manager, and
Chromium, always open. It is not a full workstation. There is no terminal and
no other application; see [What is on the desktop](/reference/desktop).

Each session has:

- **A live view** in the page. You click and type in it.
- **An MCP endpoint** of its own, for the agent.
- **A persistent disk.** Logins, tabs, downloads and the agent's notes stay until you delete the session.
- **Sleep and wake.** Unused, it sleeps. Used again, it wakes as it was.

## What the agent can do today

Today an agent operates the browser on the desktop: it opens pages, clicks,
types, reads and takes screenshots, through code it runs with `run_js`.
Control of the whole desktop with mouse and keyboard, the way a person at the
screen has it, is planned and not available yet. Until then, the parts
outside a web page (a file chooser, a permission prompt) are yours to do in
the live view.

[Live, coming and planned](/reference/status) lists exactly what works now.

## Where to go next

| If you want to | Read |
| --- | --- |
| Learn by doing | [Your first desktop](/tutorials/first-session), then [Connect an agent with Claude](/tutorials/connect-claude) |
| Get one thing done | [How-to guides](/guides/manage-sessions) |
| Look something up | [What is on the desktop](/reference/desktop), [MCP endpoint and tools](/reference/mcp), [Session states](/reference/session-states), [Limits](/reference/limits) |
| Understand how it behaves | [Computer use on a Linux desktop](/explanation/computer-use), [Sleep and wake](/explanation/sleep-and-wake), [Security model](/explanation/security) |

## Coming and planned

Not available yet:

- **Policies per session**, to restrict what an agent may do. Coming, not yet enabled.
- **API tokens**, to use a session without signing in through a browser, from scripts and CI. Coming, not yet enabled.
- **A Terraform provider**, for sessions and policies as code. Coming, not yet enabled.
- **Full desktop control** for the agent: mouse, keyboard and screenshots of the whole desktop. Planned.
