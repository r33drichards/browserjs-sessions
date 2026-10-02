# HTTP API

The web app uses this API. It is on the app's host,
`https://app.computeruse.site`.

::: warning
This API is authenticated by the sign-in cookie of the app, so it is not
meant for scripts, and it may change without notice. An API for scripts, on
`https://api.computeruse.site` with API tokens, is coming and is not enabled
yet. See [Live, coming and planned](/reference/status).
:::

Requests and answers are JSON. An error is `{"error": "<message>"}` with a
matching status code.

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

`state` is one of the [session states](/reference/session-states).
`message` is added when there is something to say about `starting` or
`failed`.

## Endpoints

| Method and path | Does | Answers |
| --- | --- | --- |
| `GET /api/me` | Who is signed in | `200` `{email, name, admin}` |
| `GET /api/sessions` | Lists your sessions | `200` array of sessions |
| `POST /api/sessions` | Creates a session. Body `{"name": "..."}`; the name is optional | `201` session. `409` at the session limit. `400` for a bad name |
| `GET /api/sessions/{id}` | One session | `200` session |
| `PATCH /api/sessions/{id}` | Renames and/or stops or resumes. Body `{"name": "...", "action": "stop" \| "resume"}`; both optional | `200` session. `400` for a bad name or action |
| `DELETE /api/sessions/{id}` | Deletes the session and its disk | `204` |
| `POST /api/sessions/{id}/vnc-ticket` | A one-time ticket for the live view | `200` `{ticket, url}` |
| `GET /api/sessions/{id}/files` | Lists the desktop's Downloads folder | `200` `{files: [{name, size, modified}], max_bytes}`. `409` if the session is not running |
| `GET /api/sessions/{id}/files/{name}` | Downloads a file, always as an attachment | `200` the bytes |
| `PUT /api/sessions/{id}/files/{name}` | Uploads a file; the body is the bytes | `2xx` `{name}`, the name it was stored under. `413` too large. `507` disk full |
| `DELETE /api/sessions/{id}/files/{name}` | Deletes a file | `2xx` |
| `POST /api/sessions/{id}/clipboard` | Puts files of the folder on the desktop's clipboard. Body `{"files": ["name", ...]}` | `2xx` |

## Notes

- A session that is not yours answers `404`, the same as one that does not
  exist.
- A session ID is `s-` followed by five or ten lower-case characters.
- `resume` on a sleeping session wakes it.
- Listing files does not wake a session. Downloading, uploading and deleting
  a file do.
- The ticket from `vnc-ticket` is valid for 30 seconds and for one
  connection. `url` is a websocket address on the sessions host.
