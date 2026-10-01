# browserjs sessions — design

Date: 2026-10-01. Status: agreed in conversation, not yet implemented.

## Goal

A web app where a signed-in user creates, opens, stops and deletes **browserjs
sessions**. A session is a private, persistent Chromium with its own mcp-js
server in front of it — what the single `railway-browser-mcp` deployment is
today, but one per user-created session, run as Kubernetes resources.

The session's page shows a live, interactive VNC view of the browser and the
MCP URL to give to Claude. Idle sessions scale to zero and wake on demand with
their tabs intact.

## Decisions

| Topic | Decision |
|---|---|
| Cluster | A new GKE cluster with the managed Agent Sandbox feature (gVisor enforced). Developed and tested locally against the open-source `kubernetes-sigs/agent-sandbox` controller until the cluster exists. |
| Session contents | One pod per session: the Chromium/VNC container and an mcp-js container. No shared mcp-js, so no mcp-js code changes. |
| State | Persistent. Each session has a disk for the browser profile, agent memory and artifacts. |
| Scale to zero | Idle sessions are suspended automatically and resumed on the next request. The session node pool autoscales to zero. |
| Tabs across sleep | GKE Pod Snapshots restore the running browser exactly; Chromium session restore is the fallback (and the only mechanism on a local cluster). |
| Users | Multi-user. You see and manage only your own sessions; an admin group sees all. |
| Sign-in | Keycloak, following the cua fleet pattern (`keycloak-js` + an auth provider in the UI, bearer tokens to the backend). |
| Authorization | Topaz (OPA policy + relationship directory), so sharing can be added as data later. |
| Create form | A name. Nothing else. |
| UI scope | Sessions only: sign-in, list, detail. No templates or admin pages. |
| Style | Low-fidelity wireframe: monochrome, outlined boxes, sketch-like, no brand colours, as a theme over Cloudscape components. No cua branding. |

Not decided: the idle period before a session sleeps. The design assumes
15 minutes as a deployment-wide setting.

## Architecture

```
                      one public hostname (GKE Gateway, TLS)
                                     │
                           ┌─────────▼──────────┐
 browser UI ──────────────▶│      backend (Go)  │◀──── Claude (MCP)
 (React + Cloudscape)      │  API, auth, proxy  │
                           └──┬────┬────────┬───┘
                 verify token │    │ check  │ create / suspend / resume / delete
                        ┌─────▼┐ ┌─▼────┐ ┌─▼──────────────────────┐
                        │Keycl.│ │Topaz │ │ Kubernetes API         │
                        └──────┘ └──────┘ │  Sandbox per session   │
                                          └─┬──────────────────────┘
                                            │ pod: chromium+VNC | mcp-js   + disk
```

- **Session = one `Sandbox`** (`agents.x-k8s.io`), built by the backend from a
  single `SandboxTemplate` shipped in `deploy/`. Templates are not exposed to
  users; changing image or size means changing that manifest.
- **The backend holds the only Kubernetes credentials.** Users and agents
  never reach the Kubernetes API or a pod directly.
- **No warm pool and no `SandboxClaim`.** Persistent sessions cannot be
  pre-warmed, so two of the four Agent Sandbox resource types are unused.
- **Keycloak and Topaz run in the cluster**, Keycloak with a real database so
  redeploys do not sign everyone out.

### Session pod

- `browser`: the image from `railway-browser-mcp` (Xvfb, Chromium, x11vnc,
  websockify, browser MCP on a pod-local port). Its own basic-auth front is
  dropped; the backend is the only way in.
- `mcp-js`: the released `wholelottahoopla/mcp-js` image, configured with
  - the browser MCP as its upstream, on localhost;
  - its session database (artifacts, upload grants) on the session disk;
  - `--public-url` set to the session's own path, `https://<host>/s/<id>`, so
    one-time upload URLs point back through the backend;
  - JWT verification off: the backend has already authenticated the caller,
    and the pod is unreachable from anywhere else (NetworkPolicy).
- Disk: one volume, mounted for the Chromium profile, `/data/memory` and the
  mcp-js session database.

## Request paths

All under one hostname. Everything except the upload route requires a
Keycloak token and passes a Topaz check.

| Path | Purpose |
|---|---|
| `GET /api/sessions` | Your sessions (all, for admins) |
| `POST /api/sessions` | Create; body is `{ "name": … }` |
| `GET /api/sessions/{id}` | One session, with state and recent events |
| `PATCH /api/sessions/{id}` | Rename, stop, resume |
| `DELETE /api/sessions/{id}` | Delete the session and its disk |
| `/s/{id}/vnc` | VNC websocket for the detail page |
| `/s/{id}/mcp` | The session's MCP endpoint, plus the OAuth discovery metadata Claude's connector needs |
| `/s/{id}/api/artifact-uploads/{token}` | mcp-js one-time upload route. No login: the token is the credential |

A session the caller may not view answers 404, not 403, so names do not leak.

## Sign-in and authorization

- The UI signs in with `keycloak-js` and only renders once authenticated, as
  fleet does. Claude's connector signs in against the same realm.
- Topaz directory: `user` and `session` objects, an `owner` relation, and an
  `admins` group. Policy: **view** = owner, admin, or (later) anyone the
  session is shared with; **manage** = owner or admin.
- Per request: verify the token, ask Topaz, act.
- The backend writes the `owner` relation on create and removes it on delete.
  The owner is also recorded on the `Sandbox`; a periodic reconcile rebuilds
  the directory from the cluster if the two drift. The cluster is the record
  of truth.
- Sharing is supported by the model and has no UI in this version.

## Scale to zero

- The backend tracks last activity per session: an MCP call or an open VNC
  connection.
- After the idle period it suspends the session. On GKE it takes a Pod
  Snapshot first (memory and filesystem to Cloud Storage); the pod is then
  removed and the disk kept.
- The next MCP call or page view resumes it. The backend holds the request
  until the pod is ready, then proxies it; the detail page shows "waking up".
- On resume, the snapshot is restored when one exists and restores cleanly.
  Otherwise the pod starts normally and Chromium reopens its last session's
  tabs.
- Manual stop and resume remain.

Known limits:

- A snapshot restore does not bring back live network connections; pages
  holding websockets or streams must reconnect.
- With session restore only, tabs reopen at the same URLs but pages reload.
- A wake that also needs a new node adds the node start time; a first MCP
  call may time out in some clients and need a retry.
- A page running on its own, with no MCP calls and no viewer, counts as idle.
  There is no per-session opt-out.

## Pages

- **Sign-in**: redirect to Keycloak.
- **Sessions**: table of name, state (starting, running, asleep, stopped,
  failed) and created time; create, stop, resume, delete. Admins can switch
  to all sessions, with an owner column.
- **Create**: a name field.
- **Session detail**: the VNC view taking most of the page; the MCP URL with
  a copy button; state, owner and recent events; stop, resume, delete, rename.

## Failure handling

- States are shown as the cluster reports them, with its events as the
  reason for "starting" or "failed".
- The VNC pane reconnects on its own and says so while trying.
- A per-user session cap (configurable) protects the cluster.
- If Topaz is unreachable, requests are denied, not allowed.

## Testing

- Backend unit tests against a fake Kubernetes client.
- Topaz policy tests: owner, admin, stranger.
- Integration on a local cluster with the open-source controller: create,
  VNC, MCP call, file upload, stop, resume with disk and tabs (session
  restore), delete.
- Browser tests of the UI flows.
- On GKE, once the cluster exists: headed Chromium under gVisor, Pod Snapshot
  suspend and resume, node pool scale to and from zero.

## Risks

1. **Chromium under gVisor.** Agent Sandbox enforces gVisor; a headed
   Chromium with software rendering has to run acceptably inside it. Unproven
   until tested on GKE.
2. **Snapshotting a browser.** Pod Snapshots are aimed at code sandboxes and
   model servers; restoring a multi-process browser with an X server is
   untested here. Session restore is the fallback.
3. **API drift.** Managed GKE Agent Sandbox and the open-source controller
   may differ in version; the backend targets the fields both support.

## Repository layout

```
backend/   Go: API, Keycloak verification, Topaz checks, Kubernetes client,
           VNC and MCP proxy, idle tracker
web/       React + Cloudscape, wireframe theme, noVNC
deploy/    SandboxTemplate, Keycloak, Topaz, Gateway, NetworkPolicy
docs/      this design, then the implementation plan
```
