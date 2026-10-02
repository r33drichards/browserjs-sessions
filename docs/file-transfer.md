# Files between your computer and a session

VNC's clipboard carries text only. Files move through one folder in the
session, shown under the Clipboard box on the session page:

- **Send**: drop files on the Files box, or choose them. They land in the
  folder; attach them on a website with the browser's file chooser, which
  opens there.
- **Save**: what the session's browser downloads lands in the same folder and
  is listed, with Save (to your computer) and Delete.

## How it works

| Piece | What it does |
| --- | --- |
| Folder | `/data/chrome/Downloads` in the `browser` container (`FILES_DIR`). On the session disk, inside the profile's mount, so it survives sleep and needs no new volume mount. |
| Chromium | `browser-mcp download-dir` (run by the entrypoint before each start of Chromium) writes `download.default_directory`, `savefile.default_directory` and `selectfile.last_directory` into the profile's Preferences. |
| Pod | The browser container's Node server (port 8081) serves `GET /files`, and `GET`, `PUT`, `DELETE /files/<name>` (`images/browser/browser/files.js`). Bodies are streamed to and from disk. An upload never replaces a file: a taken name becomes `name (1).ext`. |
| NetworkPolicy | The backend may reach session pods on 8081 as well as 6080 and 8080. |
| Backend | `GET /api/sessions/{id}/files`, and `GET`, `PUT`, `DELETE /api/sessions/{id}/files/{name}` on the app's host, behind the same sign-in and owner check as the VNC ticket (`backend/internal/proxy/files.go`). |

What keeps it safe:

- Only the session's owner (or an admin) gets past the backend; anyone else is
  answered as if the session did not exist. Nothing is added to the sessions'
  own host.
- A name is one path segment that is not hidden: no `/`, `\`, control
  characters, leading dot, or more than 255 bytes. The backend and the pod
  each check it; links and folders in the folder are neither listed nor served.
- A download is served on the app's origin, and is whatever a website sent
  the session. The backend drops every header the pod sent and answers
  `Content-Type: application/octet-stream`, `Content-Disposition: attachment`
  and what `neuter()` adds (`nosniff`, a sandbox CSP): it is saved, never
  shown. The list is parsed and written out again.
- The pod's port has no login. A page open in the session's own browser can
  reach it on 127.0.0.1 but cannot read the answers (no CORS), and requests
  under any other host name than localhost are refused (DNS rebinding).

## Limits

- 100 MB a file: `MAX_FILE_BYTES` on the backend, `FILES_MAX_BYTES` on the
  browser container (bytes; set both to change it). There is no quota for the
  folder other than the session disk (5 Gi, shared with the profile); a full
  disk answers 507.
- Pomerium gives a request 5 minutes (`timeout_read`, the app route's
  `timeout`): a file has to move in that time.
- Listing the folder neither wakes a session nor keeps it awake; sending,
  saving and deleting do both.
- A session keeps the browser image it was created with. One from before this
  feature says so in the Files box; a new session has it.

## Tests

```bash
nix develop -c bash -c 'cd backend && go test ./internal/proxy/ -run File'
nix develop -c node --test images/browser/test/files.test.mjs
nix develop -c bash -c 'cd web && npx vitest run src/files.test.ts'
```
