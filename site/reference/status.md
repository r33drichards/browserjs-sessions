# Live, coming and planned

**Live** works today. **Coming** is built and switched off. **Planned** is
being worked on and is not in the product. Nothing here is a promise of a
date.

| Pillar | Capability | Status |
| --- | --- | --- |
| Simple | Sessions: create, rename, stop, resume, delete; suggested names | Live |
| Simple | One MCP URL per session, `https://sessions.computeruse.site/<id>/mcp` | Live |
| Simple | Create and manage sessions with one API call, using API tokens on `api.computeruse.site` | Coming |
| Simple | Terraform provider | Coming |
| Stateful | Persistent disk per session | Live |
| Stateful | Sleep to a snapshot; wake with the screen and processes as they were | Live |
| Stateful | Minimal desktop: X display, window manager, Chromium | Live |
| Stateful | XFCE desktop with a terminal and a file manager | Planned |
| Scale to zero | Sleep after 15 minutes idle; wake on the next MCP call | Live |
| Scale to zero | New sessions ready in seconds, from desktops kept warm | Live |
| Scale to zero | Metering and paid plans | Planned |
| Code mode MCP | `run_js` | Live |
| Code mode MCP | Browser: `browser_execute` | Live |
| Code mode MCP | Desktop: `desktop_execute` (mouse, keyboard, screen, clipboard) | Live |
| Code mode MCP | Shell: `exec` (a program and its arguments), `stream_logs`, `search_logs`, `kill` | Live |
| Admin interface | List, create, watch and take over, stop, delete | Live |
| Admin interface | Full screen; the desktop follows the viewer's size | Live |
| Admin interface | Clipboard box; files in and out; paste or drop files anywhere on the page | Live |
| Policy | Per-session policies in Rego, covering browser, desktop and shell calls, in the editor or as code | Coming |
