# The containment model

An agent with a desktop can do a lot. This page describes the layers that
limit it, from the outside in, and says what each one does not do.

## 1. The sandbox

Each session runs in its own sandboxed container, with its own disk.

- It cannot see other sessions, yours or anyone else's.
- It cannot reach your computer. Files move only when you send or save them
  in the app, or when the agent uploads one from its own environment.
- Deleting the session removes everything in it.
- Programs the agent runs with `exec` run inside this sandbox, as an
  unprivileged user. They can reach what the browser can reach, including
  the session's files and saved logins, and nothing outside the session.
  The server that runs them listens only inside the session and refuses
  requests from web pages.

## 2. The network

A session can reach the public internet. It cannot reach private addresses
or anything inside the service.

In the other direction, nothing reaches a session directly. Every request
goes through the service, which checks who is asking.

The agent's own code is tighter still. A `run_js` program has no network
access. It reaches the web only by operating the desktop.

## 3. Identity

A session belongs to the account that created it. The app, the screen, the
files and the MCP endpoint all check for the owner. To anyone else the
session answers as if it did not exist. Knowing the MCP URL is not enough.

Two narrow things work without sign-in: the screen's connection, with a
ticket the app gets for the owner (10 seconds), and the agent's
upload URL (10 minutes, one use, one file).

The service's operators can see and manage all sessions. Do not put in a
session what you would not trust the operator with.

## 4. The code's own limits

A `run_js` program can do four things: call the desktop's capabilities
(browser, desktop, and running programs), read and write `/data/memory/`,
handle artifacts, and print. It has a time
limit and a memory limit. It cannot create, stop or delete sessions.

## 5. Policy

::: warning Coming, not yet enabled
Policies are built and switched off. Until they are on, every connected
agent has all of the abilities above.
:::

Every call from a program to the desktop passes one checkpoint. A session's
policy is asked there, with the tool's name and the full arguments, and
answers yes or no. No answer means no.

That makes it possible to say, per session: only these operations; only
these sites; no script in pages; look but do not touch. The policy is
outside the agent's reach. The agent's code cannot read it, change it or
skip it.

## What policy does not cover

Policy decides calls. It does not watch the desktop. Be clear about the
difference.

- **You at the screen.** A policy binds the agent. A person in the live view
  is not restricted.
- **What pages do next.** A rule that limits `navigate` to one site limits
  where the agent may send the browser. A link on that site, or a redirect,
  can still lead elsewhere. Policy is not a network filter.
- **A second way to do the same thing.** The desktop capability can do what
  a person can, including things a rule on browser operations refused. So
  can a program started with `exec`: inside the session it can drive the
  browser directly, without passing the checkpoint. A policy that restricts
  the browser must also refuse `desktop_execute` and `exec`, and a policy
  that restricts programs must refuse `desktop_execute`, which can type into
  any terminal on the desktop. The ready-made restrictive policies do, and
  saving a policy that does not gives a warning.
- **What an allowed program does next.** A policy reads the program and its
  arguments, and decides that one call. It does not follow the program.
  Shells, `env`, `xargs`, interpreters and many ordinary tools given the
  right arguments run other programs. Allow specific programs with specific
  arguments, not a shell, when it matters.
- **What the session is signed in to.** An allowed click acts with the
  session's logins. If the session is signed in to your email, an agent
  allowed to click can send email.
- **The agent's notes.** Reading and writing `/data/memory/` is not a
  desktop call and is not decided by policy.

## Using the layers

- Give each task or client its own session, signed in only to what it
  needs. The sandbox is the strongest boundary.
- Watch the screen when it matters, and take over for the step you want to
  do yourself.
- Use **Stop** when no agent call should start the session.
- When policies are on, start from a narrow one and widen it.
- Delete sessions you no longer need. Their logins go with them.
