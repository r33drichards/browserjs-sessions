# Session lifecycle

A session is one desktop container with one disk. It is in one of six
states. The app shows the state; the API returns it as `state`.

| State | Meaning | Compute | What an MCP call does |
| --- | --- | --- | --- |
| `starting` | The desktop is starting or waking | Starting | Waits until it runs, then answers |
| `running` | The desktop is up | In use | Answers |
| `stopping` | The desktop is shutting down, after a sleep, a stop or a delete | Releasing | Depends on what it becomes |
| `asleep` | You chose **Sleep**, or it was idle for 15 minutes. Disk and snapshot kept | None | Wakes it, waits, then answers |
| `stopped` | You chose **Stop**. Disk kept, no snapshot | None | Fails with `409` |
| `failed` | The desktop could not start. `message` says why | None | Fails with `502` |

## Transitions

| From | Cause | To |
| --- | --- | --- |
| (none) | Create | `starting`, then `running` |
| `running` | **Sleep**, or 15 minutes without use | `stopping`, then `asleep` |
| `asleep` | An MCP call, **Wake**, or a file sent or saved | `starting`, then `running` |
| `running` or `starting` | **Stop** | `stopping`, then `stopped` |
| `asleep` | **Stop** | `stopped`; the snapshot is discarded |
| `stopped` | **Start** | `starting`, then `running` |
| any | **Delete** | gone |

## Sleep and stop compared

| | Sleep | Stop |
| --- | --- | --- |
| Who causes it | You, with **Sleep**, or the service when idle | You, with **Stop** |
| Snapshot of the running desktop | Taken; this takes a few seconds | Not taken |
| Wakes on an agent's call | Yes | No |
| Comes back with | The screen and processes as they were | A fresh start from the disk: an empty desktop; tabs reload when the browser is next opened |
| The app says | `asleep` | `stopped: starts fresh` |

A sleep you ask for and an idle sleep are the same thing: both wake on the
next MCP call. To keep a session off until you start it, stop it.

If the snapshot cannot be taken, the session still goes to sleep, and wakes
like a stopped one: from its disk. The app then says
`asleep: state not saved`, and the API leaves `stateSaved` out. A snapshot
that cannot be restored is given up the same way.

## What counts as use

Keeps a session awake:

- its screen open in a visible browser tab,
- an MCP call, for as long as it runs,
- sending, saving or deleting a file.

Does not:

- a session page in a background tab, after one minute,
- an MCP client that is connected but not calling,
- listing sessions or files.

## What is kept

| | Across calls | Across sleep | Across stop | After delete |
| --- | --- | --- | --- | --- |
| Disk: browser profile and logins, downloads, `/data/memory/`, artifacts | Yes | Yes | Yes | No |
| Open windows, page state, running processes | Yes | Yes, from the snapshot | No | No |
| Variables in `run_js` | No | No | No | No |
| Name and MCP URL | Yes | Yes | Yes | No |
