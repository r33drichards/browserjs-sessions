# Session policies: the backend's API

A session's policy says what an agent connected over MCP may ask that
session's browser to do. This page describes the backend's part: the HTTP
API, what it writes to the cluster, and how it is switched on. The design is
[plans/2026-10-02-session-policies-design.md](plans/2026-10-02-session-policies-design.md);
the API's contract is
[contracts/policy/backend-api.yaml](contracts/policy/backend-api.yaml), and
this page does not repeat its schemas.

The code is `backend/internal/policy` (the handlers, the operator client, the
mode rule) and `backend/internal/sessions/policy.go` (making a session's
policy with the session).

## Switching it on

| Variable | Meaning |
|---|---|
| `POLICY_OPERATOR_URL` | The policy operator's base URL, `http://policy-operator.browserjs-sessions.svc:8080`. Unset: policies are off. |
| `OPERATOR_API_TOKEN` | The bearer token the backend calls the operator with. Required when the URL is set. |

While `POLICY_OPERATOR_URL` is unset the backend is what it was before
policies: none of the routes below exist (404), a session carries no
`policy`, no `SessionPolicy` is read or written, and the backend needs none
of the new RBAC. The one difference is that `POST /sessions` with a `policy`
is refused (409) instead of the field being ignored, because the session
would be less restricted than it was asked to be.

So the backend can be deployed before the operator and OPA exist. Setting
the variable needs, in the cluster: the `SessionPolicy` CRD, the backend's
Role on `sessionpolicies`, the operator, OPA, and a blueprint and warm pool
whose mcp-js asks OPA (`MCP_V8_POLICIES_JSON`, see
[contracts/policy/deploy.md](contracts/policy/deploy.md)).

## What the backend writes

One `SessionPolicy` per session, named after it, with:

- `spec`: `sessionRef`, `kind`, `source`, `management`. Never `status`,
  which is the operator's.
- an `ownerReference` to the session's Sandbox (`controller: false`,
  `blockOwnerDeletion: false`), so the policy goes when the session does. The
  backend also deletes it when it deletes the session, so that the name is
  free if the warm pool uses it again;
- the label `browserjs.dev/owner`, as on the Sandbox;
- the annotation `browserjs.dev/updated-by`: `ui`, or `token:<token name>`.

A policy's version is the object's `metadata.generation`. Its state is read
from the conditions the operator writes, for that generation only: `ready`
when `Ready` is `True` at it, `invalid` when `Compiled` is `False` at it,
`loading` otherwise. A condition about an earlier generation says nothing
about what was just saved.

## Creating a session

`POST /sessions` takes an optional `policy: {kind, source, management?}`.

1. A policy that was given is validated by the operator. Invalid is 422 with
   the operator's `errors`; an operator that cannot be asked is 503. In both
   cases nothing is created. With no policy the session gets
   `examples/unrestricted.rego` (the browser, desktop control and the shell, all allowed), which is built into the backend and
   is not sent to be checked, so sessions can be created while the operator
   is away (they stay `starting` until it is back).
2. The session is made in the order that never leaves it usable and
   unrestricted:
   - **cold**: the Sandbox, then its `SessionPolicy`. Between the two the
     session has no policy and OPA denies it everything. If the policy
     cannot be made, the Sandbox is deleted and the request fails.
   - **warm**: the claim carries the policy in the annotations
     `browserjs.dev/policy-kind`, `-source`, `-mode`, `-url` (and
     `browserjs.dev/updated-by`). Once the claim is bound, the
     `SessionPolicy` is made for the Sandbox it was bound to, and only then
     is the owner written to the Sandbox. `RecoverClaims` does the same from
     the annotations after a crash, so the session gets the policy it was
     asked to have and not the default; a policy that exists already is left
     as it is.
3. The answer is 201 at once. The session's `state` is `starting`, whatever
   its pod says, until its first policy is `ready`.
4. The backend keeps watching the new policy for up to two minutes. If the
   operator refuses it at the reconcile after having passed it in step 1,
   the session is deleted.

A Sandbox whose mcp-js does not ask OPA is never given a `SessionPolicy`. If
the pool hands one over, the claim is given back and the session starts cold.
If the blueprint itself predates policies, a session asked for without a
policy is made as before (and is `unsupported`), and one asked for with a
policy is refused (409).

## The policy of a session

All under `/sessions/{id}`, after the same check as the session itself: its
owner or an admin; anyone else gets 404.

| Request | Answer |
|---|---|
| `GET /policy` | 200 with the policy and `ETag: "<version>"`. |
| `PUT /policy` | Validates, saves, waits up to 10 seconds. 200 `ready`; 202 saved and not yet in force. |
| `DELETE /policy` | The same, with the unrestricted policy in `editor` mode. The object is kept. |
| `PUT /policy/management` | 200. Changes `mode` and `managed_url` and nothing else. |

Refusals of a write, in the order they are checked:

| Status | When |
|---|---|
| 403 | The token lacks `policies:write` (`policies:read` for a read), or was made for another session. |
| 409 | The session predates policies (`unsupported`); on `GET` too. |
| 400 | The body is not JSON, `kind` is given and is not `rego`, or `management` is not valid. |
| 422 | The source is empty or over 65536 bytes (`size_error`); or, after the next two, the operator says it is invalid. |
| 409 | The credential may not write in the policy's mode (below). |
| 412 | `If-Match` is given and is not the current version. |
| 503 | The operator could not be asked. |

Nothing is saved by a request that is refused. A request that would change
nothing is answered 200 with the policy as it is; it does not raise the
version and the operator is not asked.

A 202 whose body has `state: "invalid"` means the operator refused at the
reconcile what it had passed when asked. The policy is saved, `errors` says
why, and the policy before it is still in force.

### Who may write

| | cookie (the UI) | API token |
|---|---|---|
| mode `editor` | may | 409 `this policy is managed in the editor`, unless the request sets `management.mode` to `iac` |
| mode `iac` | 409 `this policy is managed externally`, with `managed_url` | may |

`PUT /policy/management` is allowed in every cell. That is how the owner
takes a policy back from Terraform in the UI, and there is no other way: a
`PUT /policy` by cookie on an `iac` policy is refused even if it asks for
`editor`.

A token's reset (`DELETE`) of an `iac` policy leaves it in `editor` mode, as
the contract says a reset does; the token's next save has to take it over
again.

### Sessions that predate policies

A session is policy-capable when its Sandbox's `mcp-js` container has an
`MCP_V8_POLICIES_JSON` that contains `browserjs/decision/`. One that is not
has `policy: {"state": "unsupported"}`, runs as it always did, and answers
409 on all four routes.

### A policy removed behind the backend

A capable session whose `SessionPolicy` was deleted with `kubectl` is denied
everything by OPA. The API shows it as `starting` with
`policy: {"state": "loading"}`; `GET /policy` is 404; `PUT` or `DELETE
/policy` makes the object again.

## Without a session

| Request | Answer |
|---|---|
| `POST /policies/validate` | The operator's verdict, passed on as it is. An invalid policy is 200 with `ok: false`. |
| `POST /policies/evaluate` | The operator's `{ok, allow, errors}` for `{kind, source, input}`. |
| `GET /policy-presets` | The contract's examples, `unrestricted` first, then by id. |

The first two are 503 when the operator cannot be reached. Any token may
use all three, whatever its scopes.

A policy is a Rego module and nothing else: `kind` is `rego`, and may be
left out of a request. (The JSON form of the first design, and
`GET /policy-schema.json` with it, were removed before policies were
enforced anywhere.) What a module may say, the input for each tool
(`browser_execute`, `desktop_execute`, and `exec`, `stream_logs`,
`search_logs`, `kill` on the `exec` server) and the warnings `validate` returns
when a policy restricts one tool and leaves open another that walks around
it are in [`contracts/policy/rego-contract.md`](contracts/policy/rego-contract.md).
`evaluate` answers as a session is answered: a server or tool the platform
does not know is `allow: false` under any policy.

The seven presets (`unrestricted`, `browser-only`, `form-filling`,
`no-scripting`, `observe-only`, `one-site`, `read-only-shell`) each begin
with a comment that says what they allow; that comment, as one line, is the
preset's `description`.

The presets are copies of `docs/contracts/policy/examples/*.rego` in
`backend/internal/policy/presets/`, because a Go binary embeds only files
below its package and the image is built from `backend/`. A test fails when a
copy differs from the contract or one is missing: after changing an example,
copy it over.

## Tests

`cd backend && nix develop -c go test -race ./...`. The operator is an
`httptest` server answering as `operator-api.yaml`; the cluster is the
dynamic client's fake, taught to treat `SessionPolicy` as the API server
does (`internal/sessions/sessionstest/policies.go`: the CRD's validation, a
generation that rises with the spec, a status that only the test, playing
the operator, writes).
