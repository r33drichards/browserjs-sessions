# HTTP API

There are two APIs with the same session object.

| | Host | Signs in with | Status |
| --- | --- | --- | --- |
| The app's API | `https://app.computeruse.site/api` | The app's sign-in cookie | Live. For the app, not for scripts; it may change |
| The API for code | `https://api.computeruse.site/v1` | An API token | Live |

Requests and answers are JSON. An error is `{"error": "<message>"}`.

## The session object

```json
{
  "id": "s-abcde",
  "name": "brave-otter",
  "owner": "you@example.com",
  "state": "running",
  "created": "2026-10-01T12:00:00Z",
  "size": "small",
  "mcp_url": "https://sessions.computeruse.site/s-abcde/mcp",
  "policy": {
    "kind": "rego",
    "version": 1,
    "state": "ready",
    "management": {"mode": "editor"}
  }
}
```

`state` is one of the [lifecycle states](/reference/lifecycle). `message`
is added when there is something to say about `starting` or `failed`.
`stateSaved` is added, as `true`, when a session that is asleep (or on its
way there) has a snapshot to wake from; without it the session starts fresh
from its disk. `size` is `small`, `medium` or `large`
([Session sizes](/reference/session-sizes)); `pendingSize` is added while a
change of size waits for the session's next start. An ID
is `s-` and five or ten lower-case characters.

`policy` summarizes the session's policy: its kind, version, state, management
and, when available, the hash of the policy in force. The state is `ready`,
`loading`, `invalid` or `unsupported`. A new session stays `starting` until
its first policy is in force. Sessions created before enforcement have
`policy: {"state": "unsupported"}` and remain unrestricted, including after
sleep and wake; create a replacement session to use a policy.

## Sessions

Paths are under `/api` on the app's host and under `/v1` on the API host.

| Method and path | Does | Answers |
| --- | --- | --- |
| `GET /me` | Who is calling | `200` |
| `GET /sessions` | Lists your sessions | `200` array |
| `GET /sizes` | Lists the sizes a session can have, and what each gives the desktop | `200` `{default, sizes: [{name, cpuMillis, memoryMiB, warm}]}` |
| `POST /sessions` | Creates one. Body `{"name": "...", "size": "small" \| "medium" \| "large"}`, all fields optional; the size defaults to `small`. Also accepts `policy: {kind: "rego", source: "...", management?}` | `201` session. `409` at the limit, or when there is no room for the size (`"code": "no_capacity"`). `400` for a bad name or size |
| `GET /sessions/{id}` | Reads one | `200` session |
| `PATCH /sessions/{id}` | Renames, resizes, stops or resumes. Body `{"name": "...", "size": "...", "action": "stop" \| "resume"}`, all optional | `200` session. `400` for a bad name, size or action, and then nothing of the request is done. `409` (`no_capacity`) when a resume finds no room |
| `POST /sessions/{id}/sleep` | Puts a running session to sleep: takes a snapshot, then removes the desktop. Answers when the snapshot is done, which takes seconds. No body | `200` session. `409` if it is starting, stopping, stopped or failed |
| `POST /sessions/{id}/wake` | Starts a session that is asleep (from its snapshot) or stopped (fresh). Does not wait for it to run. No body | `200` session. `402` or `403` if billing refuses. `409` (`no_capacity`) when there is no room for its size |
| `DELETE /sessions/{id}` | Deletes it and its disk | `204` |

A session that is not yours answers `404`, like one that does not exist.
With a token, every route that changes a session needs the scope
`sessions:write`; without it the answer is `403`.

`sleep` is what happens to an idle session, asked for. It answers `200` and
changes nothing if the session is already asleep. If the snapshot could not
be taken the session sleeps all the same, and the answer has no
`stateSaved`. A session you put to sleep wakes on the next MCP call, like
one that went idle; `stop` is the way to keep one off.

`wake` and the `resume` action of `PATCH` do the same thing. On a session
that is already awake they change nothing.

A `size` in `PATCH` takes effect at once on a session that is asleep or
stopped. On one that is awake it is recorded as `pendingSize` and takes
effect at the session's next start. Either way that start is a fresh one,
from the disk: a snapshot is not restored at another size, so `stateSaved`
goes when a sleeping session is resized. `{"size": "large", "action":
"stop"}` resizes and stops in one request. A `409` with `"code":
"no_capacity"` carries `Retry-After`; nothing was created or started, and
the same request can be sent again later.

## Screen and files (the app's API only)

| Method and path | Does | Answers |
| --- | --- | --- |
| `POST /api/sessions/{id}/vnc-ticket` | A ticket for the live view: good for 10 seconds | `200` `{ticket, url}` |
| `GET /api/sessions/{id}/files` | Lists the Downloads folder. Does not wake the session | `200` `{files: [{name, size, modified}], max_bytes}`. `409` if not running |
| `GET /api/sessions/{id}/files/{name}` | Downloads a file, as an attachment | `200` |
| `PUT /api/sessions/{id}/files/{name}` | Uploads a file; the body is the bytes | `2xx` `{name}`. `413` too large. `507` disk full |
| `DELETE /api/sessions/{id}/files/{name}` | Deletes a file | `2xx` |
| `POST /api/sessions/{id}/clipboard` | Puts files on the desktop's clipboard. Body `{"files": ["name"]}` | `2xx` |

## Policies

Live. Paths are under `/api` on the app's host and `/v1` on the API host.
On the API host, reading a session's policy needs `policies:read`; changing
it or its management needs `policies:write`.

| Method and path | Does | Answers |
| --- | --- | --- |
| `GET /sessions/{id}/policy` | Reads the policy, including its source | `200` policy, with `ETag: "<version>"` |
| `PUT /sessions/{id}/policy` | Saves `{kind: "rego", source: "...", management?}` | `200` ready or unchanged; `202` saved but not yet in force |
| `DELETE /sessions/{id}/policy` | Resets to unrestricted, editable in the app | The same readiness answers as PUT |
| `PUT /sessions/{id}/policy/management` | Sets `{mode: "editor"}` or `{mode: "iac", managed_url: "https://..."}` | `200` policy; source unchanged |
| `GET /policy-presets` | Lists ready-made Rego policies | `200` presets |
| `POST /policies/validate` | Checks `{kind: "rego", source: "..."}` without saving | `200` verdict, including `ok`, errors and warnings; invalid source has `ok: false` |
| `POST /policies/evaluate` | Checks `{kind: "rego", source: "...", input: {...}}` against a sample call | `200` with `ok`, `allow` and errors |

`kind` can be omitted; Rego is the only policy format. Source must be nonempty
and at most 65536 bytes. A session created without a policy starts with the
unrestricted one. An explicit policy is validated before creating the
session; invalid source is `422`, and an unavailable validator is `503`.

An unchanged write returns `200` without raising the version. Otherwise, a
write waits up to 10 seconds for the policy to be loaded. A `202` with
`state: "loading"` means wait for `ready` before relying on the new rules.
The previous policy stays in force while an edit loads, or if it becomes
`invalid`. A `202` can carry `state: "invalid"` and errors if reconciliation
rejects a saved policy. A validation failure before saving is `422` and
changes nothing. `If-Match: "<version>"` on PUT refuses a stale edit with
`412`.

In `editor` mode, an API token's save must set `management.mode` to `iac`
and give an https `managed_url`. In `iac` mode the app shows the policy
read-only; an app save is refused with `409`. Change management back to
`editor` to edit it in the app. Resetting through DELETE also returns it to
`editor`. All session policy routes return `409` for an `unsupported`
session. Validation and evaluation return `503` if the operator is
unavailable. Any API token can use presets, validation and evaluation,
regardless of its scopes.

See [Policy format](/reference/policy) for tool inputs and warnings.

## API tokens

Tokens are live. Create and revoke them on the app's **API tokens** page.
On the API host, `POST /oauth/token` exchanges an API token for an access
token using the client-credentials grant. See
[Use it from code](/guides/use-from-code) for scopes and examples.
