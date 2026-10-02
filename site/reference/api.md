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
is added when there is something to say about `starting` or `failed`. An ID
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
| `DELETE /sessions/{id}` | Deletes it and its disk | `204` |

A session that is not yours answers `404`, like one that does not exist.
`resume` on a sleeping session wakes it.

## Screen and files (the app's API only)

| Method and path | Does | Answers |
| --- | --- | --- |
| `POST /api/sessions/{id}/vnc-ticket` | A ticket for the live view: 30 seconds, one connection | `200` `{ticket, url}` |
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
