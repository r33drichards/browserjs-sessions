---
layout: home
title: Computer Use
titleTemplate: A Node REPL for cloud computer use

hero:
  name: Computer Use
  text: A Node REPL for cloud computer use.
  tagline: Simple. Stateful. Scale to zero. Serverless, resumable desktop containers with a code-mode MCP tool, a light admin interface, and policy to contain what agents do.
  actions:
    - theme: brand
      text: Create a desktop
      link: /tutorials/first-desktop
    - theme: alt
      text: What is live
      link: /reference/status
    - theme: alt
      text: Open the app
      link: https://app.computeruse.site

features:
  - title: Simple
    details: One resource. A session is a desktop container. Create it with one click and get one MCP URL. There is nothing else to set up.
  - title: Stateful
    details: A real desktop on a persistent disk. Logins, files and open windows stay. Sleep takes a snapshot, and wake restores the screen and its processes as they were.
  - title: Scale to zero
    details: An idle session goes to sleep and uses no compute. The next MCP call wakes it. You do not start or stop anything.
---

## Code mode MCP

A session gives an agent one tool: `run_js`. The agent writes a short
program. The program calls the desktop's capabilities, takes several steps,
and returns only what matters.

```js
const r = await mcp.callTool("browser", "browser_execute", {
  operations: [
    { type: "navigate", params: { url: "https://example.com" } },
    { type: "evaluate", params: { script: "document.title" } },
  ],
})
console.log(r.content[0].text)
```

That is one tool call, not one per click. See [Why code mode](/explanation/code-mode).

| Capability | Called from `run_js` as | Status |
| --- | --- | --- |
| Browser: pages, by selector and script | `browser_execute` | Live |
| Desktop: mouse, keyboard, screen, clipboard | `desktop_execute` | Live |
| Shell: run commands | `exec` | Planned |

## A light admin interface

The app does five things: list sessions, create one, watch its screen and
take over, stop it, delete it. The screen in the page is the same desktop
the agent works on, so you can do a step yourself and hand back.

## Policy to contain it

Each session can carry a policy that decides which of the agent's calls are
allowed: which operations, which sites, with which arguments. Write it as
JSON or as Rego, in the app's editor or as code. The policy binds the agent,
not you at the screen.

Policies are built and not switched on yet. See
[Live, coming and planned](/reference/status).

## What a desktop contains today

An X display, a window manager, and Chromium. A fuller desktop with a
terminal and a file manager is planned. The
[capabilities page](/reference/capabilities) says exactly what is there.

## Read next

| If you want to | Read |
| --- | --- |
| Try it | [Create a desktop and connect an agent](/tutorials/first-desktop), then [Your first run_js program](/tutorials/first-program) |
| Get one thing done | [How-to guides](/guides/sleep-wake-resume) |
| Look something up | [Session lifecycle](/reference/lifecycle), [MCP endpoint and run_js](/reference/mcp), [Capabilities](/reference/capabilities), [Limits](/reference/limits) |
| Understand it | [Serverless, resumable desktop containers](/explanation/desktop-containers), [Why code mode](/explanation/code-mode), [The containment model](/explanation/containment) |
