# Session URLs

Where a session is reached, why, and what is left to remove. Written
2026-10-01 from source reading and unit tests; nothing here has run against a
cluster. Claims are marked **VERIFIED** (read in the cited source) or
**UNVERIFIED**.

| | URL |
|---|---|
| MCP endpoint (what the UI shows, what a client is given) | `https://sessions.browserjs.com/<id>/mcp` |
| Screen (websocket, one-time ticket from the app's API) | `wss://sessions.browserjs.com/<id>/vnc?ticket=<ticket>` |
| Upload (one-time, issued by mcp-js's `get_artifact_upload_url`) | `https://sessions.browserjs.com/<id>/api/artifact-uploads/<token>` |
| OAuth protected-resource metadata (Pomerium's) | `https://sessions.browserjs.com/.well-known/oauth-protected-resource/<id>/mcp` |
| OAuth authorization server metadata (Pomerium's) | `https://sessions.browserjs.com/.well-known/oauth-authorization-server` |

`<id>` is the session's ID, `s-` and ten characters, or `s-` and five for a
session that came out of the warm pool.

Before, every session had a host of its own, `https://<id>.sessions.browserjs.com/mcp`.
Those hosts still answer ([Old hosts](#old-hosts-deprecated)), but nothing
hands out their URLs any more.

## One host, not the app's

The sessions are on `sessions.browserjs.com`, a host that has nothing else on
it. The alternative, `https://app.browserjs.com/s/<id>/mcp`, needs no DNS
record and no certificate name, and was not chosen:

- **A browser signed in to the app would be signed in to every MCP
  endpoint.** On an MCP route Pomerium takes the bearer token if there is
  one, and otherwise falls back to the session cookie (VERIFIED:
  `authorize/grpc.go` at v0.33.3, `loadSession`: `maybeGetSessionFromRequest`
  reads the bearer token, and on `ErrNoSessionFound` the code goes on to
  `sessionStore.ReadSessionHandleAndCheckIDP`). An MCP call runs code in the
  user's logged-in browser, so that endpoint should not answer to a cookie
  that every visitor of the app carries. On `sessions.browserjs.com` a
  browser has a Pomerium cookie only if it went through an MCP client's
  sign-in there.
- **What a pod answers would be of the app's origin.** A pod runs what its
  user's agent runs, and its answers are handed on. Today three headers keep
  them from acting as a page (`neuter` in `backend/internal/proxy`:
  `Content-Security-Policy: sandbox`, `nosniff`, no `Set-Cookie`). On the
  app's host one route that forgot them would be script in the origin that
  holds the app's cookie and API. On a separate host it is not.
- The price is one `A` record and one name on the certificate, made once.
  Nothing is made per session in either design.

What one host for all sessions gives up against a host each: the sessions
share an origin, so the browser no longer keeps one session's content from
another's. Two things stand in for that. The response headers above, as
before. And the backend refuses, before asking the pod, any request a
browser makes in order to show or run the answer (`Sec-Fetch-Mode` other than
`cors` or `same-origin`, on the MCP and upload routes): a navigation to one,
or one loaded as a script, image or frame, gets 403. (The screen's route
answers a websocket handshake and nothing else, as before.) An MCP client that runs in
a page uses `fetch`, which is `cors`, and clients that are not browsers send
no such header.

## How a request is routed

Pomerium has three routes on `sessions.browserjs.com`, each a regular
expression on the whole path (`deploy/gke/pomerium-config.yaml`; VERIFIED:
Pomerium hands `regex` to Envoy as `safe_regex`, `mkRouteMatch` in
`config/envoyconfig/routes.go`, of which Envoy says "The entire path
(without the query string) must match the regex"); any other path on that
host has no route. UNVERIFIED: that an MCP server route with a `regex`
behaves like the documented ones with a prefix; nothing in the source treats
it differently, and it is the first thing the test plan below shows. The backend takes `/<id>` off the front of
the path and serves the rest from the session's routes
(`sessionRoutes` in `backend/internal/proxy/proxy.go`: add a route to a
session's pod there, and it exists in both forms of URL).

| Path | Pomerium | Backend |
|---|---|---|
| `/<id>/mcp`, `/<id>/mcp/…` | MCP server route: signs the client's user in, passes `X-Pomerium-Jwt-Assertion` | owner or admin; wakes the session; pod `:8080/mcp…` |
| `/<id>/vnc` | public, websockets | redeems the ticket; wakes the session; pod `:6080/websockify` |
| `/<id>/api/artifact-uploads/<token>` | public | running sessions only, bounded body; pod `:8080/api/artifact-uploads/<token>` |

- The prefix is taken off the path as it was sent. What is left is not
  decoded, so an encoded `/` in it is still refused where it was before, and
  it is not cleaned: `/<id>//mcp` is 404, not a redirect to a path without
  the session.
- A redirect from the pod is to one of the pod's own paths; its `Location`
  is put back under `/<id>` (`rewriteLocation`, after
  `libs/fleet/backend/handlers/svc.go` in trycua/cua). A redirect to another
  origin is handed on unchanged, as before.
- mcp-js is started with `MCP_V8_PUBLIC_URL=https://sessions.browserjs.com/<id>`
  and makes an upload URL by appending the path to it (VERIFIED:
  `server/src/mcp_dispatch.rs` at `v0.21.0-rc.3`,
  `format!("{base}{path}")` with `path = "/api/artifact-uploads/{token}"`,
  and `with_public_url` trims a trailing `/`). The only redirect in its HTTP
  API is `/` to `/llms.txt` (`server/src/api.rs`), which is not exposed.
  UNVERIFIED: that the deployed image is built from that tag (the Dockerfile
  says so in a comment; the digest was not inspected).
- The websocket is opened by the UI with the URL the backend returns with the
  ticket; noVNC is part of the UI's bundle and nothing is loaded from the
  pod, so there are no paths for it to get wrong.

## MCP sign-in

Pomerium is the OAuth authorization server, as before. Because the sessions'
host is exact (no `*`), it now also answers the discovery itself, which it
does not do on wildcard hosts, and the backend's stand-in
(`backend/internal/proxy/metadata.go`) is needed for the old hosts only.

1. The client calls `POST https://sessions.browserjs.com/<id>/mcp` with no
   token. Pomerium answers 401 with
   `WWW-Authenticate: Bearer resource_metadata="https://sessions.browserjs.com/.well-known/oauth-protected-resource/<id>/mcp"`
   (VERIFIED: `internal/mcp/handler_metadata.go`,
   `ProtectedResourceMetadataURL(host, requestPath)` joins the well-known
   path and the request's).
2. That document says `resource` is `https://sessions.browserjs.com/<id>/mcp`
   and `authorization_servers` is `["https://sessions.browserjs.com"]`
   (VERIFIED: same file, `getProtectedResourceMetadata` takes the resource's
   path from what follows the well-known prefix; the route for it is a prefix
   route added to every exact host that has an MCP server route,
   `config/envoyconfig/routes.go` lines 77 to 80). This is RFC 9728's form
   for a resource with a path (section 3.1: the well-known string goes
   "between the host component and the path"). A client that is given no
   `resource_metadata` tries the same URL first: "At the path of the
   server's MCP endpoint: `https://example.com/public/mcp` could host
   metadata at `https://example.com/.well-known/oauth-protected-resource/public/mcp`",
   then "At the root" (MCP authorization specification, 2025-11-25,
   <https://modelcontextprotocol.io/specification/2025-11-25/basic/authorization>).
   At the root Pomerium names the host itself as the resource, not a
   session.
3. `https://sessions.browserjs.com/.well-known/oauth-authorization-server`
   names `/.pomerium/mcp/authorize` and `/.pomerium/mcp/token` on the same
   host. One issuer for every session.
4. The client sends the user to authorize, with `resource` set to the MCP
   URL. Pomerium does not look at `resource` (VERIFIED: it does not occur in
   `internal/mcp/handler_authorization.go`) and its tokens are not bound to
   one: a token is good on every MCP route of this Pomerium, for the user it
   was issued to. Whether a session is that user's is decided by the backend
   on every request, as before.

What follows from one issuer: a client that has signed in for one session
may be able to use another of the same user's sessions without a second
sign-in. UNVERIFIED for Claude's connectors (each URL is a connector of its
own there).

The assertion Pomerium hands the backend has the request's host as its
audience, which the backend checks. It used to name one session's host; it
now names the sessions' host. It is never shown to a pod.

## Old hosts (deprecated)

`https://<id>.sessions.browserjs.com/…` is still served, by the same routes
and the same checks, for MCP URLs already configured in clients and upload
URLs from sessions created before (a session's pod keeps the
`MCP_V8_PUBLIC_URL` it was created with). The API and the UI give out the
new URLs only.

To retire them, when no client uses them and no session from before is left
(Pomerium's access log shows requests to `*.sessions.browserjs.com`):

1. `deploy/gke/pomerium-config.yaml` and `deploy/local/pomerium-config.yaml`:
   delete the four `legacy-session-*` routes.
2. `deploy/gke/patch-backend.yaml`, `deploy/local/patch-backend.yaml`: delete
   `LEGACY_SESSION_URL_TEMPLATE`.
3. `deploy/gke/certificate.yaml`: delete `"*.sessions.browserjs.com"`. With
   no wildcard left, the names could be proven by HTTP-01 instead of DNS-01;
   nothing requires changing that.
4. `infra/main/edge.tf`: delete the `sessions` entry of `public_names` (the
   wildcard `A` record); the plan shows one record to destroy.
5. Backend: `metadata.go`, `Proxy.LegacyURLs`, `Config.LegacySessionURLs`,
   and the host form of `sessions.URLTemplate` with their tests;
   `test/smoke.sh`'s `LEGACY` section; the legacy checks in
   `test/integration.py` and `test/browser-e2e.mjs`.

## Rollout

In this order; each step is safe to stop after.

1. **Merge.** The merge applies `infra/main` ("infra apply" runs on a change
   there): one `A` record, `sessions.browserjs.com`, to the edge address.
   Check that run's summary. Until the name resolves, new sessions' URLs do
   not. The merge deploys nothing.
2. **Images**: build the backend from the merged commit, pin its digest in
   `deploy/gke/kustomization.yaml` (`hack/pin-images.sh`).
3. **Deploy**. The Certificate gains a name, so cert-manager asks Let's
   Encrypt for a new certificate (DNS-01, some minutes; Pomerium serves
   the old one until the new one is in the Secret). UNVERIFIED: that
   Pomerium picks up the renewed Secret without a restart, as it does for
   renewals. The warm pool's template changes, so its pods are replaced.

A backend with the new configuration and a Pomerium with the old one (or the
other way round) serve the old hosts as before and the new URLs not at all:
deploy applies both together.

## Test plan on the cluster

1. `test/smoke.sh`: 30 checks. The ones that are new: the certificate names
   `sessions.browserjs.com`; Pomerium answers the protected-resource
   metadata for a session and names the session's MCP URL; the 401 on
   `/<id>/mcp` points there; nothing else on the host has a route (404).
   The old-host checks must still pass.
2. In the UI, create a session. Its MCP URL reads
   `https://sessions.browserjs.com/<id>/mcp`, and the screen connects (the
   browser's network panel shows the websocket to
   `wss://sessions.browserjs.com/<id>/vnc?ticket=…`).
3. Connect a real client: `claude mcp add --transport http browserjs https://sessions.browserjs.com/<id>/mcp`,
   then `/mcp` in Claude Code and sign in. Expect the browser to open
   `https://sessions.browserjs.com/.pomerium/mcp/authorize?…`, the tools to
   list, and `run_js` to answer. Then the same URL as a custom connector in
   claude.ai. This is the step that shows a client accepts a resource with a
   path on this Pomerium; it is UNVERIFIED until done.
4. From the client, call `get_artifact_upload_url`; the URL starts with
   `https://sessions.browserjs.com/<id>/api/artifact-uploads/`; `curl -T` a
   file to it (200), and a second time (4xx).
5. As a second allowed user, point a client at the first user's URL: sign-in
   succeeds, the call is 404.
6. A session created before the deploy: its old URL still works in a client
   that has it, and its upload URLs (old host) still take a file.
7. A warm-pool session (ID of five characters): steps 2 to 4.
8. In a browser signed in to the app, open
   `https://sessions.browserjs.com/<id>/mcp`: Pomerium's 401 page, or the
   backend's 403 if the browser has been through an MCP sign-in on that
   host; never the pod's answer.
9. Let a session sleep; call its MCP URL; it wakes and answers.
