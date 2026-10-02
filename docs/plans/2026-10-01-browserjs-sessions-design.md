# browserjs sessions — design

Date: 2026-10-01. Status: built and run end to end on a local cluster; not yet
on GKE. Sign-in, authorization and request paths were redesigned during the
build (Pomerium and Dex instead of Keycloak and Topaz) and are described here
as built.

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
| Sign-in | Pomerium (open-source Core) in front of everything, signing users in through Dex, which offers Google and GitHub. |
| Authorization | The backend's own check: a session belongs to the email that created it; a configured list of admin emails may see and manage all. |
| Session addresses | Every session has a hostname of its own, `<id>.<session domain>`, so a session's content never shares an origin with the app or another session. |
| Create form | A name. Nothing else. |
| UI scope | Sessions only: sign-in, list, detail. No templates or admin pages. |
| Style | Low-fidelity wireframe: monochrome, outlined boxes, sketch-like, no brand colours, as a theme over Cloudscape components. No cua branding. |

Not decided: the idle period before a session sleeps. The design assumes
15 minutes as a deployment-wide setting.

## Architecture

```
        app host, authenticate host, <id>.<session domain>  (TLS)
                                     │
                           ┌─────────▼──────────┐       ┌─────┐   Google
 browser UI ──────────────▶│      Pomerium      │──────▶│ Dex │──▶ GitHub
 Claude (MCP) ────────────▶│ sign-in, MCP OAuth │       └─────┘
                           └─────────┬──────────┘
                                     │ request + signed identity header
                           ┌─────────▼──────────┐
                           │      backend (Go)  │
                           │ API, owner check,  │
                           │ proxy, UI files    │
                           └──┬──────────────┬──┘
     create / suspend / resume│              │ MCP, VNC, uploads
                 / delete     │              │
                  ┌───────────▼────────┐  ┌──▼─────────────────────────┐
                  │ Kubernetes API     │  │ pod: chromium+VNC | mcp-js │
                  │ Sandbox per session│  │ + disk                     │
                  └────────────────────┘  └────────────────────────────┘
```

- **Session = one `Sandbox`** (`agents.x-k8s.io/v1beta1`), built by the backend
  from a blueprint file shipped in `deploy/`. There is no `SandboxTemplate`
  object and blueprints are not exposed to users; changing image or size means
  changing that file.
- **The backend holds the only Kubernetes credentials.** Users and agents
  never reach the Kubernetes API or a pod directly.
- **No warm pool and no `SandboxClaim`.** Persistent sessions cannot be
  pre-warmed, so two of the four Agent Sandbox resource types are unused.
- **Pomerium and Dex run in the cluster.** Pomerium keeps its sessions and MCP
  client registrations on a disk, so a restart does not sign everyone out;
  that storage allows one replica. Nothing but Pomerium may reach the backend
  (NetworkPolicy).
- **One backend replica.** VNC tickets and idle tracking live in its memory.

### Session pod

- `browser`: the image from `railway-browser-mcp` (Xvfb, Chromium, x11vnc,
  websockify, browser MCP on a pod-local port). Its own basic-auth front is
  dropped; the backend is the only way in.
- `mcp-js`: the released `wholelottahoopla/mcp-js` image, configured with
  - the browser MCP as its upstream, on localhost;
  - its session database (artifacts, upload grants) on the session disk;
  - `--public-url` set to the session's own host, `https://<id>.<session domain>`,
    so one-time upload URLs point back through the backend;
  - JWT verification off: the backend has already authenticated the caller,
    and the pod is unreachable from anywhere else (NetworkPolicy).
- Disk: one volume, mounted for the Chromium profile, `/data/memory` and the
  mcp-js session database.

## Request paths

Two kinds of host, both served by Pomerium and passed to the one backend
Service with the caller's `Host`. The backend tells them apart by host.

**The app host.** One Pomerium route: any signed-in user. Pomerium adds the
signed identity header.

| Path | Purpose |
|---|---|
| `GET /api/me` | Who is signed in, and whether they are an admin |
| `GET /api/sessions` | Your sessions (`?all=1`: everyone's, for admins) |
| `POST /api/sessions` | Create; body is `{ "name": … }` |
| `GET /api/sessions/{id}` | One session, with its state |
| `PATCH /api/sessions/{id}` | Rename, stop, resume |
| `DELETE /api/sessions/{id}` | Delete the session and its disk |
| `POST /api/sessions/{id}/vnc-ticket` | A one-time ticket, and the websocket URL to open the screen with |
| everything else | The UI |

**A session's host, `<id>.<session domain>`.** Four wildcard routes; any other
path does not exist.

| Path | Pomerium route | Who checks what |
|---|---|---|
| `/mcp` | MCP server route: Pomerium is the OAuth server for MCP clients and passes the identity header | Backend: the caller owns the session or is an admin |
| `GET /vnc?ticket=…` | Public, websockets allowed | Backend: the ticket is unused, unexpired and for this session |
| `PUT /api/artifact-uploads/{token}` | Public | The session's mcp-js: the one-time token it issued. Backend: size cap, session already running |
| `GET /.well-known/oauth-…` | Public | Nothing to check: the two OAuth discovery documents for the MCP endpoint |

A session the caller may not use answers 404, not 403, so names do not leak.

The MCP endpoint never answers 401: that is Pomerium's cue to an MCP client to
sign in, and from the backend Pomerium would turn it into a 502. A request
with no valid identity is 403.

**The GET stream rule.** A `GET` on `/mcp` is the stream on which the server
sends events to the client. Some clients keep one open for as long as the
server is configured. It is therefore not use of the session: it does not
wake a sleeping session, does not count as activity, and does not keep the
pod. On a session that is not running it answers 405 (`Allow: POST, DELETE`);
the client's next call wakes the session.

## Sign-in and authorization

- **Pomerium authenticates, the backend authorizes.** Pomerium signs users in
  against Dex and adds `X-Pomerium-Jwt-Assertion` to each request: a short-lived
  JWT naming the user's email, with the request's host as its audience. The
  backend verifies it against Pomerium's keys (`POMERIUM_JWKS_URL`), requires
  the audience to be the request's host, and takes the email as the user.
- **Dex federates the sign-in**: Google and GitHub connectors; Pomerium is its
  only client. (Pomerium alone offers a single identity provider.)
- **Owner or admin.** A session's owner is the email recorded on its Sandbox
  when it was created. Admins are the `ADMIN_EMAILS` list, read at startup.
  There is no sharing and no directory.
- **The UI has no sign-in code.** It asks `/api/me`; when the API answers 401
  (or the proxy redirects) it reloads the page, and Pomerium sends the browser
  to sign in. Sign-out is a link to `/.pomerium/sign_out`.
- **MCP clients sign in with Pomerium**, which acts as the OAuth authorization
  server on the session's host. Clients identify themselves by a metadata
  document; the domains allowed to are configured
  (`mcp_allowed_client_id_domains: [claude.ai]`). The token a client gets is
  not bound to one host; the backend's owner check is what protects a session.
- **The backend serves the OAuth discovery documents for session hosts**
  (`/.well-known/oauth-protected-resource`, `/.well-known/oauth-authorization-server`),
  through a public route. Pomerium v0.33.3 serves them only on hosts with an
  exact route, not on hosts matched by a wildcard; the documents say what
  Pomerium's own would, and the endpoints they name are Pomerium's.
- **The screen and uploads carry their own credentials**, because a websocket
  from the app's page to another host and an upload from an agent's machine
  have no Pomerium session: a one-time ticket issued to a user who may see the
  session, and mcp-js's one-time upload token.
- The pod is shown none of this: the backend strips the identity header,
  cookies and `Authorization` before proxying.

Open:

- **Claude as the MCP client is untested.** The MCP SDK's own client signs in
  and uses a session through Pomerium on the local cluster; Claude's clients
  have not been tried against it.
- **Who may sign in** is everyone with a Google or GitHub account until an
  allow-list is added to the routes.
- Not verified: a completed Google or GitHub sign-in (only the redirect to
  each), Claude's own clients against this setup, anything on GKE.

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

- **Sign-in**: Pomerium redirects to Dex, which offers Google and GitHub.
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
- If a session's owner cannot be read from the cluster, the request is
  refused (503), not allowed.

## Testing

- Backend unit tests against a fake Kubernetes client.
- Owner, admin and stranger on every route, in the backend's unit tests and
  again in the integration test.
- Integration on a local cluster with the open-source controller: create,
  VNC, MCP call, file upload, stop, resume with disk and tabs (session
  restore), delete.
- Browser test of the UI through Pomerium and Dex (`test/browser-e2e.mjs`),
  and an MCP client's sign-in walked by hand (`test/mcp-oauth.mjs`).
- On GKE, once the cluster exists: headed Chromium under gVisor, Pod Snapshot
  suspend and resume, node pool scale to and from zero.

## Risks

1. **Chromium under gVisor.** Agent Sandbox enforces gVisor; a headed
   Chromium with software rendering has to run acceptably inside it. Unproven
   until tested on GKE.
2. **Snapshotting a browser.** Pod Snapshots are aimed at code sandboxes and
   model servers; restoring a multi-process browser with an X server is
   untested here. Session restore is the fallback.
3. **MCP sign-in on per-session hosts** depends on the backend standing in
   for Pomerium's discovery documents, and is untested with Claude's clients.
4. **API drift.** Managed GKE Agent Sandbox and the open-source controller
   may differ in version; the backend targets the fields both support.

## Repository layout

```
backend/   Go: API, identity verification, owner check, Kubernetes client,
           VNC and MCP proxy, idle tracker
web/       React + Cloudscape, wireframe theme, noVNC
images/    the session's two container images
deploy/    kustomize: backend, session blueprint, Pomerium, Dex, NetworkPolicy
hack/      local cluster up and down
test/      end-to-end tests against the local cluster
docs/      this design, then the implementation plan
```
