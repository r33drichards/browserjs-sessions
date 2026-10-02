# Session states

A session is in one of six states. The app shows the state on the list and
on the session's page. The API returns it as `state`.

| State | Meaning | The desktop | What an MCP call does |
| --- | --- | --- | --- |
| `starting` | The desktop is starting or waking. | Not ready yet | Waits until it runs, then answers |
| `running` | The desktop is up. | Live view and MCP work | Answers |
| `stopping` | The desktop is shutting down, after a stop, an idle sleep or a delete. | Gone | Depends on what it becomes |
| `asleep` | Put to sleep after 15 minutes without use. The disk is kept. | Off | Wakes the session, waits, then answers |
| `stopped` | You chose **Stop**. The disk is kept. | Off | Fails with `409`. Resume it in the app |
| `failed` | The desktop could not start. `message` says why. | Off | Fails with `502` |

## Transitions

| From | Action | To |
| --- | --- | --- |
| (none) | Create | `starting`, then `running` |
| `running` | Stop | `stopping`, then `stopped` |
| `running` | 15 minutes idle | `stopping`, then `asleep` |
| `asleep` | Wake in the app, an MCP call, or a file sent or saved | `starting`, then `running` |
| `stopped` | Resume | `starting`, then `running` |
| any | Delete | gone |

## What counts as use

These keep a session awake:

- an open live view in a visible tab,
- an MCP call, for as long as it runs,
- sending, saving or deleting a file.

These do not:

- a session page in a background tab, after one minute,
- an MCP client that is connected but not calling anything,
- listing sessions or files.
