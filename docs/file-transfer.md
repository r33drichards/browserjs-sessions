# Files between your computer and a session

VNC's clipboard carries text only. Files move through one folder in the
session, shown under the Clipboard box on the session page:

- **Send**: drop files anywhere on the session page (the remote screen
  included), paste a copied file or screenshot (click the Files box and press
  Ctrl+V or ⌘V; a paste while the remote screen has the focus is the remote
  browser's), or choose them. They land in the
  folder; attach them on a website with the browser's file chooser, which
  opens there.
- **Save**: what the session's browser downloads lands in the same folder and
  is listed, with Save (to your computer) and Delete.

- **Paste**: a file you send is also put on the session's clipboard (several
  dropped together go on together; the last batch wins), and every listed file
  has "Copy to browser clipboard". Ctrl+V in the session's browser then pastes
  the file itself into pages that take pasted files, with no file chooser.

## How it works

| Piece | What it does |
| --- | --- |
| Folder | `/data/chrome/Downloads` in the `browser` container (`FILES_DIR`). On the session disk, inside the profile's mount, so it survives sleep and needs no new volume mount. |
| Chromium | `browser-mcp download-dir` (run by the entrypoint before each start of Chromium) writes `download.default_directory`, `savefile.default_directory` and `selectfile.last_directory` into the profile's Preferences. |
| Pod | The browser container's Node server (port 8081) serves `GET /files`, and `GET`, `PUT`, `DELETE /files/<name>` (`computer-use-mcp/browser/files.js`). Bodies are streamed to and from disk. An upload never replaces a file: a taken name becomes `name (1).ext`. |
| NetworkPolicy | The backend may reach session pods on 8081 as well as 6080 and 8080. |
| Backend | `GET /api/sessions/{id}/files`, and `GET`, `PUT`, `DELETE /api/sessions/{id}/files/{name}` on the app's host, behind the same sign-in and owner check as the VNC ticket (`backend/internal/proxy/files.go`). |

### Files on the clipboard

`POST /api/sessions/{id}/clipboard` with `{"files": [name, ...]}` (the same
sign-in and owner check as the file routes; JSON only) becomes `POST /clipboard`
on the pod's port 8081. The browser container starts one `xclip` per copy,
which owns the X11 CLIPBOARD selection and offers a single target,
`text/uri-list`: the `file://` URIs of the files (`computer-use-mcp/browser/clipboard.js`).

- Chromium reads pasted files from exactly that target
  (`ClipboardOzone::ReadFilenames`, `ui/base/clipboard/clipboard_ozone.cc`), and
  blink turns each into a file of the paste event
  (`DataObject::CreateFromClipboard`,
  `third_party/blink/renderer/core/clipboard/data_object.cc`). A page sees the
  file with its name and its type by extension, as if it had been dropped on it.
- No image target is offered beside it: blink makes a second file out of an
  `image/png` on the clipboard, so an image would arrive twice. An editor that
  only takes raw image data, and not pasted files, does not get the image.
- No text target is offered: Xvnc announces a new clipboard owner to the
  viewer only if it offers `STRING` or `UTF8_STRING`
  (`unix/xserver/hw/vnc/vncSelection.c`), so the Clipboard box keeps the text
  it had. Sending text from the box, or copying anything in the browser, takes
  the selection: xclip exits and the files are off the clipboard.
- Only names of files in the folder are accepted (the same name check, in the
  backend and in the pod; a link or folder is refused), and the pod refuses
  the request from anything that looks like a page of its own browser
  (`Origin`, `Sec-Fetch-*`, or a body that is not JSON).
- Deleting a file that is on the clipboard gives the clipboard up.

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
nix develop -c node --test computer-use-mcp/test/files.test.mjs computer-use-mcp/test/clipboard.test.mjs
nix develop -c bash -c 'cd web && npx vitest run src/files.test.ts'
```
