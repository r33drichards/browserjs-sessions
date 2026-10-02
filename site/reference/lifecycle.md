# Session lifecycle

A session is one desktop container with one disk. It is in one of six
states. The app shows the state; the API returns it as `state`.

| State | Meaning | Compute | What an MCP call does |
| --- | --- | --- | --- |
| `starting` | The desktop is starting or waking | Starting | Waits until it runs, then answers |
| `running` | The desktop is up | In use | Answers |
| `stopping` | The desktop is shutting down, after a stop, an idle sleep or a delete | Releasing | Depends on what it becomes |
| `asleep` | Idle for 15 minutes. Disk and snapshot kept | None | Wakes it, waits, then answers |
| `stopped` | You chose **Stop**. Disk kept | None | Fails with `409` |
| `failed` | The desktop could not start. `message` says why | None | Fails with `502` |

## Transitions

| From | Cause | To |
| --- | --- | --- |
| (none) | Create | `starting`, then `running` |
| `running` | 15 minutes without use | `stopping`, then `asleep` |
| `asleep` | An MCP call, **Wake**, or a file sent or saved | `starting`, then `running` |
| `running` | **Stop** | `stopping`, then `stopped` |
| `stopped` | **Resume** | `starting`, then `running` |
| any | **Delete** | gone |

## Sleep and stop compared

| | Sleep | Stop |
| --- | --- | --- |
| Who causes it | The service, when idle | You |
| Snapshot of the running desktop | Taken | Not taken |
| Wakes on an agent's call | Yes | No |
| Comes back with | The screen and processes as they were | A fresh start from the disk; tabs reload |

If a snapshot cannot be taken or restored, a sleeping session wakes like a
resumed one: from its disk.

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
