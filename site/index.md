---
layout: home
title: browserjs
titleTemplate: A persistent cloud browser for you and your agents

hero:
  name: browserjs
  text: A cloud browser that stays yours.
  tagline: Sign in, create a session, and get your own Chromium with a live view and an MCP endpoint. Logins and tabs survive. It sleeps when idle and wakes when used.
  actions:
    - theme: brand
      text: Your first session
      link: /tutorials/first-session
    - theme: alt
      text: Connect it to Claude
      link: /tutorials/connect-claude
    - theme: alt
      text: Open the app
      link: https://app.browserjs.com

features:
  - title: Give an agent a logged-in browser
    details: Sign in to the sites you need once, in the live view. The agent works in the same browser through MCP. The logins are still there next week.
  - title: Watch a long task and take over
    details: An agent's clicks happen on a screen you can see. When it gets stuck on a login or a captcha, do that step yourself and let it carry on.
  - title: One browser per task or client
    details: Each session has its own profile and disk. Keep work for different accounts or customers apart, and delete a session when the job is done.
  - title: One MCP endpoint for all your tools
    details: A session's MCP URL does not change. Add it to Claude and to any other MCP client, and they all drive the same browser.
  - title: Reproduce a bug in a clean browser
    details: Start a fresh Chromium that is not your own machine, load the page, and move files in and out to capture what you find.
---

## What a session is

A session is one Chromium browser running in the cloud, with:

- **A live view** in the page. You click and type in it as in any browser.
- **An MCP endpoint** of its own. Add it to Claude or another MCP client as a connector, and the agent can run JavaScript and drive the browser.
- **A persistent disk.** Logins, cookies, tabs and downloads stay until you delete the session.
- **Sleep and wake.** An unused session goes to sleep. The next use wakes it, with its pages as they were.

## Where to go next

| If you want to | Read |
| --- | --- |
| Learn by doing | [Your first session](/tutorials/first-session), then [Connect a session to Claude](/tutorials/connect-claude) |
| Get one thing done | [How-to guides](/guides/manage-sessions) |
| Look something up | [Session states](/reference/session-states), [MCP endpoint and tools](/reference/mcp), [Limits](/reference/limits), [HTTP API](/reference/api) |
| Understand how it behaves | [Sleep and wake](/explanation/sleep-and-wake), [What persists](/explanation/persistence), [Security model](/explanation/security) |

## Planned

These are not available yet:

- Session URLs by path, `sessions.browserjs.com/<id>/mcp`, in place of a host name per session.
- Policies per session for what an agent may do.
- A Terraform provider.
