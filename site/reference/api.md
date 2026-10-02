# HTTP API

There are two APIs with the same session object.

| | Host | Signs in with | Status |
| --- | --- | --- | --- |
| The app's API | `https://app.computeruse.site/api` | The app's sign-in cookie | Live. For the app, not for scripts; it may change |
| The API for code | `https://api.computeruse.site/v1` | An API token | Coming, not yet enabled |

Requests and answers are JSON. An error is `{"error": "<message>"}`.

## The session object

```json
{
  "id": "s-abcde",
  "name": "brave-otter",
  "owner": "you@example.com",
  "state": "running",
  "created": "2026-10-01T12:00:00Z",
  "mcp_url": "https://sessions.computeruse.site/s-abcde/mcp"
}
```

`state` is one of the [lifecycle states](/reference/lifecycle). `message`
is added when there is something to say about `starting` or `failed`.
`stateSaved` is added, as `true`, when a session that is asleep (or on its
way there) has a snapshot to wake from; without it the session starts fresh
from its disk. An ID
is `s-` and five or ten lower-case characters.

## Sessions

Paths are under `/api` on the app's host and under `/v1` on the API host.

| Method and path | Does | Answers |
| --- | --- | --- |
| `GET /me` | Who is calling | `200` |
| `GET /sessions` | Lists your sessions | `200` array |
| `POST /sessions` | Creates one. Body `{"name": "..."}`, optional | `201` session. `409` at the limit. `400` for a bad name |
| `GET /sessions/{id}` | Reads one | `200` session |
| `PATCH /sessions/{id}` | Renames, stops or resumes. Body `{"name": "...", "action": "stop" \| "resume"}`, both optional | `200` session. `400` for a bad name or action |
| `POST /sessions/{id}/sleep` | Puts a running session to sleep: takes a snapshot, then removes the desktop. Answers when the snapshot is done, which takes seconds. No body | `200` session. `409` if it is starting, stopping, stopped or failed |
| `POST /sessions/{id}/wake` | Starts a session that is asleep (from its snapshot) or stopped (fresh). Does not wait for it to run. No body | `200` session. `402` or `403` if billing refuses |
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

## Screen and files (the app's API only)

| Method and path | Does | Answers |
| --- | --- | --- |
| `POST /api/sessions/{id}/vnc-ticket` | A ticket for the live view: good for 10 seconds | `200` `{ticket, url}` |
| `GET /api/sessions/{id}/files` | Lists the Downloads folder. Does not wake the session | `200` `{files: [{name, size, modified}], max_bytes}`. `409` if not running |
| `GET /api/sessions/{id}/files/{name}` | Downloads a file, as an attachment | `200` |
| `PUT /api/sessions/{id}/files/{name}` | Uploads a file; the body is the bytes | `2xx` `{name}`. `413` too large. `507` disk full |
| `DELETE /api/sessions/{id}/files/{name}` | Deletes a file | `2xx` |
| `POST /api/sessions/{id}/clipboard` | Puts files on the desktop's clipboard. Body `{"files": ["name"]}` | `2xx` |

## Policies and tokens

Coming, not yet enabled. On the API host, with the scopes in
[Use it from code](/guides/use-from-code):

| Method and path | Does |
| --- | --- |
| `GET /v1/sessions/{id}/policy` | Reads a session's policy |
| `PUT /v1/sessions/{id}/policy` | Replaces it |
| `DELETE /v1/sessions/{id}/policy` | Returns it to unrestricted |
| `POST /v1/policies/validate` | Checks a policy without saving it |
| `POST /v1/policies/evaluate` | Asks a policy about a sample call |
| `POST /oauth/token` | Exchanges an API token for an access token (client credentials) |
