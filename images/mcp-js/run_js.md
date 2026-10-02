run javascript or typescript code in v8

Executes code and returns the console output directly. Each call runs in a fresh V8 isolate — no state is carried between calls.

TypeScript support is type removal only — types are stripped before execution, not checked. Invalid types will be silently removed, not reported as errors.

params:
- code (optional): the javascript or typescript code to run. Provide either `code` or `file`.
- file (optional): path to a JavaScript/TypeScript file **on the server's own filesystem** to read and execute instead of inline `code`. Provide either `code` or `file`, not both. This is disabled by default: the server must be started with `--allow-run-js-file` (allow any path) or a `run_js_file` policy in `--policies-json` (allow specific paths/dirs), otherwise the call is rejected. The path is resolved on the server, not uploaded from the client.
- heap_memory_max_mb (optional): maximum V8 heap memory in megabytes (minimum: 4, default: 8). Override the server default for this execution.
- execution_timeout_secs (optional): maximum execution time in seconds (1–300, default: 30). Override the server default for this execution.

returns:
- output: console output from the execution (everything printed via console.log, console.info, console.warn, console.error)
- error: error message if the execution failed, timed out, or was cancelled

## Console Output

Use `console.log()` to produce output. `console.info`, `console.warn`, and `console.error` are also supported (with `[INFO]`, `[WARN]`, `[ERROR]` prefixes respectively).

eg:

```js
const result = 1 + 1;
console.log(result);
```

Returns `output: "2"`.

```js
const obj = { a: 1, b: 2 };
console.log(JSON.stringify(obj));
```

Returns `output: '{"a":1,"b":2}'`.

async/await is supported. The runtime resolves top-level Promises automatically.

## Artifacts (returning images and other non-text content)

Console output is text-only. To return an image — or any other typed payload (audio, CSV, arbitrary binary) — store it as an artifact:

```js
const png = renderChart(); // Uint8Array of PNG bytes
artifact("chart", "image/png", png);
```

- `artifact(key, mime, bytes)` — store an artifact under a caller-chosen key (same key overwrites). `bytes` may be a Uint8Array, TypedArray, ArrayBuffer, or string (UTF-8 encoded). Max 16 MiB per artifact.
- Emitted artifacts are attached directly to this tool's result as content blocks: `image/*` as an MCP image block (the model can actually see the image), `audio/*` as audio, UTF-8 payloads as text, other binary as base64 text. Up to 8 MiB of payloads are attached inline; anything larger stays retrievable via the `get_artifact(key)` tool.
- The result JSON lists each emitted artifact (`key`, `mime_type`, `size_bytes`, `inline`).
- Artifacts also work as input: a file uploaded through a `get_artifact_upload_url` URL is readable here with `artifact.get(key)` → `{ key, mime_type, size_bytes, created_at, bytes: Uint8Array }` (`null` if the key doesn't exist). `artifact.list()` returns metadata for everything stored.

## Importing Packages

You can import npm packages, JSR packages, and URL modules using ES module `import` syntax. Packages are fetched from esm.sh at runtime — no installation needed.

- **npm**: `import { camelCase } from "npm:lodash-es@4.17.21";`
- **jsr**: `import { camelCase } from "jsr:@luca/cases@1.0.0";`
- **URL**: `import { pascalCase } from "https://deno.land/x/case/mod.ts";`

Always pin versions for reproducible results. Dynamic `import()` is also supported with top-level `await`.

## Filesystem Access

When the server is configured with policies, JavaScript code can use an `fs` module providing Node.js-compatible file operations. Every operation is evaluated against a Rego policy before execution.

**Available operations:**
- `await fs.readFile(path, [encoding])` — Read file as a `Uint8Array` (default, Node semantics) or a string (if a text encoding like `"utf8"` is given)
- `await fs.writeFile(path, data)` — Write string or `Uint8Array` to file
- `await fs.appendFile(path, data)` — Append data to file
- `await fs.readdir(path)` — List directory contents
- `await fs.stat(path)` — Get file metadata
- `await fs.mkdir(path, [options])` — Create directory (supports `{recursive: true}`)
- `await fs.rm(path, [options])` — Delete file or directory (supports `{recursive: true}`)
- `await fs.rename(oldPath, newPath)` — Rename or move file
- `await fs.copyFile(src, dest)` — Copy file
- `await fs.createWriteStream(path)` — Open a streaming write handle (`await w.write(chunk)`, `await w.close()`) for large files
- `await fs.exists(path)` — Check if path exists
- `await fs.unlink(path)` — Delete a file

All operations return Promises and are subject to Rego policy evaluation. Policy input includes `operation`, `path`, `destination` (for rename/copy), `recursive` (for mkdir/rm), and `encoding` (for readFile).

## Limitations

- **No `fetch` or network access by default**: When the server is started with fetch policies configured via `--policies-json`, a `fetch(url, opts?)` function becomes available. `fetch()` follows the web standard Fetch API — it returns a Promise that resolves to a Response object. Use `await` to get the response: `const resp = await fetch(url)`. The response object has `.ok`, `.status`, `.statusText`, `.url`, `.headers.get(name)`, `.text()`, and `.json()` methods (`.text()` and `.json()` also return Promises). Each request is checked against policy before execution. If the server is also configured with `--fetch-header` or `--fetch-header-config`, matching requests may receive static headers or dynamically acquired OAuth client-credentials bearer tokens before policy evaluation. Headers set directly in JavaScript still win. Without fetch policies, there is no network access.
- **No file system access by default**: Filesystem access requires server configuration with policies. See "Filesystem Access" above.
- **No environment variables**: The runtime does not provide access to environment variables.
- **Timers**: `setTimeout`, `clearTimeout`, `setInterval` and `clearInterval` are available.
- **No DOM or browser APIs**: This is not a browser environment; there is no access to `window`, `document`, or other browser-specific objects.

Each execution starts with a fresh V8 isolate — no state is carried between calls.


## This deployment: persistent memory and a logged-in browser

### Persistent memory — `/data/memory/`

`fs` is enabled for exactly one directory: **`/data/memory/`**, on a persistent
volume. Anything written there survives across calls, sessions, agents, and
server restarts. Everything outside it is denied. Use it as your long-term
memory: notes, state, task progress, learned facts, per-site selectors, drafts.

Start every session by reading the index, and keep it current:

```js
const idx = "/data/memory/INDEX.md";
console.log(await fs.exists(idx) ? await fs.readFile(idx, "utf8") : "(no memory yet)");
```

Conventions (follow them so other agents can find your work):
- `/data/memory/INDEX.md` — one line per file: `- path — what it holds`. Update it whenever you add or remove a file.
- One topic per file, markdown or JSON, e.g. `/data/memory/sites/x.com.md`, `/data/memory/tasks/<name>.json`.
- `fs.mkdir(path, { recursive: true })` before writing into a new subdirectory.
- Prefer updating an existing file over creating a near-duplicate; delete what is wrong.
- Never store passwords, tokens, or cookies here — the browser already holds logins.

### Getting a file from your machine into `run_js`

To hand this server a file you have locally (a PDF, an image, a CSV), do not
paste its contents into `run_js` code. Call the `get_artifact_upload_url` tool
with a `key` and `mime_type`; it returns a one-time `url`. Upload the raw file
to it from your own environment — no token or other credentials needed:

```bash
curl -fsS -T ./form.pdf '<url>'
```

Then read it here: `const file = artifact.get("form.pdf")` gives
`file.bytes` (a `Uint8Array`) and `file.mime_type`. A URL works once, expires
after 10 minutes by default, and takes up to 16 MiB.

### Browser — `mcp.callTool("browser", "browser_execute", …)`

A persistent, headed Chromium (already logged into sites by the operator, who
can watch it live over VNC) is available as an upstream MCP server:

```js
const r = await mcp.callTool("browser", "browser_execute", {
  operations: [
    { type: "navigate", params: { url: "https://example.com" } },
    { type: "evaluate", params: { script: "document.title" } },
  ],
});
console.log(r.content[0].text);
```

Operations: `navigate {url}`, `wait {ms | selector}`, `click {selector}`,
`type {selector, text}`, `press {key}`, `select {selector, values}`,
`evaluate {script}`, `screenshot {fullPage?}`, `setViewport`, `setContent`, `url`.
The tab stays open between calls: a later call continues on the same page, so
navigate once and then keep clicking/reading without reloading. Pass
`tab: "<name>"` to work in a separate tab (each name is its own long-lived
tab) and `close: true` on the last call when you are done with one. The
pipeline stops at the first failing step and attaches a screenshot of that
state. Record working selectors for each site in `/data/memory/sites/`.

### Desktop control (nut.js) — `mcp.callTool("browser", "desktop_execute", …)`

The whole desktop that Chromium runs on (the X display the operator watches
over VNC) can be driven with [nut.js](https://github.com/nut-tree/nut.js):
mouse, keyboard, screen and clipboard. nut.js is a Node.js library with a
native addon, so it cannot be imported into `run_js`; it runs next to the
display, in the browser container, and you call it from here. Each operation
is the nut.js call of the same name (`mouse.click`, `keyboard.type`,
`screen.grab`, …):

The desktop is XFCE: Chromium's window, and a panel along the bottom edge
with an applications menu, launchers and the open windows. Besides Chromium
it has a terminal (`xfce4-terminal`: bash with git, curl, python3, node and
the usual Unix tools, as an unprivileged user, with nothing to install
packages with), a file manager (Thunar), a text editor (Mousepad) and an
image viewer (Ristretto). The home directory, `/data/chrome/home`, is kept
with the session; `~/Downloads` is the folder of the session's files. Open a
program from the panel or the menu, or with Alt+F2 and its name.

```js
// A screenshot. It comes back to your code, not to the model.
const r = await mcp.callTool("browser", "desktop_execute", {
  operations: [{ type: "screen.grab" }],
});
const { results, screen } = JSON.parse(r.content[0].text); // screen: { width, height }
const png = r.content[1 + results[0].result.image_index];  // { type: "image", data: <base64 PNG>, mimeType }
// Only when you need to look at it: attach it to this run_js result.
artifact("screen", "image/png", Uint8Array.from(atob(png.data), (c) => c.charCodeAt(0)));
console.log(JSON.stringify(screen));
```

```js
// Click at a point and type text, then press Enter.
await mcp.callTool("browser", "desktop_execute", {
  operations: [
    { type: "mouse.click", params: { x: 640, y: 52 } },
    { type: "keyboard.type", params: { text: "example.com" } },
    { type: "keyboard.type", params: { keys: ["Enter"] } },
  ],
});
```

```js
// A key combination (Ctrl+L): press the keys, then release the same keys.
await mcp.callTool("browser", "desktop_execute", {
  operations: [
    { type: "keyboard.pressKey", params: { keys: ["LeftControl", "L"] } },
    { type: "keyboard.releaseKey", params: { keys: ["LeftControl", "L"] } },
  ],
});
```

Operations (`{ type, params }`, run in order):

- Mouse: `mouse.setPosition {x, y}` (jump), `mouse.move {x, y}` (glide in a
  straight line), `mouse.getPosition`, `mouse.click {button?, x?, y?}`,
  `mouse.doubleClick {button?, x?, y?}`, `mouse.pressButton {button?}`,
  `mouse.releaseButton {button?}`, `mouse.drag {to: {x, y}, from?: {x, y}}`
  (LEFT held along the way), `mouse.scrollUp` / `mouse.scrollDown` /
  `mouse.scrollLeft` / `mouse.scrollRight {amount, x?, y?}` (wheel steps).
  With `x, y`, click and scroll go there first; without, they act where the
  pointer is.
- Keyboard: `keyboard.type {text}` types a string; `keyboard.type {keys}`
  taps each key in turn; `keyboard.pressKey {keys}` and
  `keyboard.releaseKey {keys}` hold and let go.
- Screen: `screen.width`, `screen.height`, `screen.grab` (whole screen),
  `screen.grabRegion {left, top, width, height}`, `screen.colorAt {x, y}` →
  `{R, G, B, hex}`.
- Windows: `getActiveWindow` → `{title, region}`, `getWindows` → a list of
  them. These report what the X server has, which includes the window
  manager's own frames: expect entries with an empty title.
- Clipboard: `clipboard.setContent {text}`, `clipboard.getContent` →
  `{text}`. The clipboard is shared with the person at the VNC view.
- `sleep {ms}` (up to 30000).

`Button`: `LEFT` (default), `MIDDLE`, `RIGHT`.

`Key` (nut.js's names; letters and digits are keys, not characters):
`Escape`, `F1`, `F2`, `F3`, `F4`, `F5`, `F6`, `F7`, `F8`, `F9`, `F10`, `F11`, `F12`, `F13`, `F14`, `F15`, `F16`, `F17`, `F18`, `F19`, `F20`, `F21`, `F22`, `F23`, `F24`, `Print`, `ScrollLock`, `Pause`, `Grave`, `Num1`, `Num2`, `Num3`, `Num4`, `Num5`, `Num6`, `Num7`, `Num8`, `Num9`, `Num0`, `Minus`, `Equal`, `Backspace`, `Insert`, `Home`, `PageUp`, `NumLock`, `NumPadEqual`, `Divide`, `Multiply`, `Subtract`, `Tab`, `Q`, `W`, `E`, `R`, `T`, `Y`, `U`, `I`, `O`, `P`, `LeftBracket`, `RightBracket`, `Backslash`, `Delete`, `End`, `PageDown`, `NumPad7`, `NumPad8`, `NumPad9`, `Add`, `CapsLock`, `A`, `S`, `D`, `F`, `G`, `H`, `J`, `K`, `L`, `Semicolon`, `Quote`, `Return`, `NumPad4`, `NumPad5`, `NumPad6`, `LeftShift`, `Z`, `X`, `C`, `V`, `B`, `N`, `M`, `Comma`, `Period`, `Slash`, `RightShift`, `Up`, `NumPad1`, `NumPad2`, `NumPad3`, `Enter`, `LeftControl`, `LeftSuper`, `LeftWin`, `LeftCmd`, `LeftAlt`, `LeftMeta`, `RightControl`, `RightSuper`, `RightWin`, `RightAlt`, `RightCmd`, `RightMeta`, `Space`, `Menu`, `Fn`, `Left`, `Down`, `Right`, `NumPad0`, `Decimal`, `Clear`, `AudioMute`, `AudioVolDown`, `AudioVolUp`, `AudioPlay`, `AudioStop`, `AudioPause`, `AudioPrev`, `AudioNext`, `AudioRewind`, `AudioForward`, `AudioRepeat`, `AudioRandom`.

How it behaves:

- **Results.** `r.content[0].text` is JSON: `{ results: [{ success, operation,
  result }], screen: { width, height } }`. Screenshots are PNGs in the image
  items after the text; a screenshot's `result` is `{ image_index, width,
  height }`, and its image is `r.content[1 + image_index]`. Nothing reaches
  the model unless you print it or emit it with `artifact(...)`, so grab the
  screen freely and attach it only when you need to see it (a full-screen PNG
  costs far more context than a line of text; `screen.grabRegion` and
  `screen.colorAt` are cheaper ways to check one spot). For a large
  screenshot, pass `heap_memory_max_mb: 64` to `run_js`.
- **Coordinates** are screen pixels, `(0, 0)` at the top left, and must be
  inside the screen. The screen is resized whenever the person watching
  resizes their window, so coordinates from an earlier call can be stale:
  take `screen` from a result, or a fresh screenshot, before aiming.
- **Failures.** The whole call is checked before anything runs (an unknown
  operation or key name runs nothing). It then stops at the first operation
  that fails and `mcp.callTool` throws; `err.result.content` holds the
  results so far and a screenshot of that moment. Keys and buttons still held
  when a call ends are released for you.
- **Speed.** `config: { keyboardDelayMs, mouseDelayMs, mouseSpeed }` next to
  `operations` sets the pause before each key (default 10 ms) and each mouse
  action (50 ms), and the glide speed of `mouse.move` and `mouse.drag`
  (2000 px/s). One call runs at a time; keep a call well under the `run_js`
  timeout (30 s unless you raise `execution_timeout_secs`).
- **Text beyond the keyboard.** `keyboard.type {text}` presses the keys of a
  US layout. For other characters (accents, CJK, emoji), put the text on the
  clipboard and paste it: `clipboard.setContent`, then `LeftControl` + `V`.

**Which tool.** For anything inside a web page, use `browser_execute`: it
finds elements by selector, waits for them, reads the DOM, and does not depend
on where things are on screen. Use `desktop_execute` for what the page does
not contain: the browser's own UI (address bar, tabs, permission and download
prompts, extension popups), native dialogs such as the file chooser, pages
that only react to real input, and other windows. The person watching sees
the pointer move and can use the mouse and keyboard at the same time.

### Shell (mcp-exec) — `mcp.callTool("exec", "exec", …)`

Programs run on the desktop Chromium runs on, as the desktop's user: the same
home directory, files and `PATH` as a terminal there, and `DISPLAY` is set, so
a program can open a window the person watching sees. (`child_process` is not
available in `run_js`; this is the way to run a program.) The server is
[mcp-exec](https://github.com/r33drichards/mcp-exec), and it is asynchronous:
`exec` starts the program and returns an id at once, `stream_logs` returns its
output and status, `search_logs` greps its output, `kill` stops it.

```js
// Run a program and wait for it: exec, then stream_logs until it has ended.
async function run(bin, args = [], { timeout = 60, cwd, env } = {}) {
  const call = async (tool, a) => JSON.parse((await mcp.callTool("exec", tool, a)).content[0].text);
  const { id } = await call("exec", { bin, args, timeout, ...(cwd ? { cwd } : {}), ...(env ? { env } : {}) });
  let logs = "", offset = 0; // exec returned { id: "<uuid>", status: "started" }
  for (;;) {
    const r = await call("stream_logs", { id, offset }); // { logs, next_offset, status }
    logs += r.logs;
    offset = r.next_offset;
    if (r.status !== "running") return { id, logs, status: r.status };
    await new Promise((resolve) => setTimeout(resolve, 250));
  }
}
const r = await run("git", ["clone", "--depth", "1", "https://github.com/octocat/Hello-World", "hello"]);
if (r.status !== "completed:0") throw new Error(`${r.status}\n${r.logs}`);
console.log((await run("ls", ["-la"], { cwd: "/tmp" })).logs);
```

```js
// A long command: start it in one run_js call, keep the id, look at it later.
const start = await mcp.callTool("exec", "exec", { bin: "npm", args: ["test"], cwd: "/tmp/app", timeout: 1800 });
const { id } = JSON.parse(start.content[0].text);
await fs.writeFile("/data/memory/last-build-id.txt", id);
// ...in a later run_js call: the output from byte `offset` on, and the status.
const r = JSON.parse((await mcp.callTool("exec", "stream_logs", { id, offset: 0 })).content[0].text);
console.log(r.status, r.next_offset); // "running", then "completed:<exit code>"
// Lines of a long log, without reading all of it; and stopping the command.
const found = await mcp.callTool("exec", "search_logs", { id, pattern: "(?i)error|failed" });
console.log(JSON.parse(found.content[0].text).matches); // [{ line, offset }]
await mcp.callTool("exec", "kill", { id }); // { id, status: "cancelled" }
```

```js
// Pipes, redirection, globs, &&: those are a shell's. Ask for one, as the program.
const start = await mcp.callTool("exec", "exec", {
  bin: "sh",
  args: ["-c", "ls -1 *.csv | wc -l"],
  cwd: "/tmp",
  timeout: 10,
});
```

The tools:

- `exec { bin, args?, timeout, cwd?, env? }` returns `{ id, status: "started" }`.
  - `bin`: the program, a name on `PATH` or a path. It is run **directly, not
    by a shell**.
  - `args`: its arguments, an array of strings, each passed exactly as it is:
    no quoting needed, nothing is split or expanded (`"*.csv"` and `"$HOME"`
    arrive as those characters). Default: none.
  - `timeout`: in **seconds**, required. Then the program and everything it
    started are killed and the status is `"timeout"`.
  - `cwd`: the working directory, an absolute path of an existing directory.
    Default: the home directory.
  - `env`: `{ NAME: "value" }`, added to the desktop's environment.
  - Anything else is refused, `cmd` included: there is no command-line form.
- `stream_logs { id, offset }` returns `{ logs, next_offset, status }`: the
  output from byte `offset` to the end, and where to continue. `status` is
  `"running"`, `"completed:<exit code>"`, `"timeout"`, `"cancelled"` or
  `"failed:<reason>"` (for example a program that does not exist); `"error"`
  means the id is not known (the reason is in `logs`).
- `search_logs { id, pattern }` returns `{ matches: [{ line, offset }] }` for a
  regular expression (Rust syntax; `(?i)` for case-insensitive).
- `kill { id }` stops a running command, with everything it started, and
  returns `{ id, status }`.

How it behaves:

- **Output.** stdout and stderr go to one log, line by line, in the order they
  arrive. Bytes that are not valid UTF-8 read back as `\uFFFD`: pipe binary
  output through `base64` or into a file. Nothing reaches the model unless you
  print it: read with offsets and `search_logs`, and log the part you need.
- **Exit code.** A program that fails is not an error of the call: check that
  `status` is `"completed:0"`. A refused argument (an unknown field, a `cwd`
  that does not exist) or a policy that denies the call makes `mcp.callTool`
  throw.
- **No terminal, no input.** Programs have no TTY and an empty stdin:
  programs that prompt will not work. Pass flags that make them
  non-interactive.
- **Polling.** Each call is quick; a `run_js` call itself is limited (30 s
  unless you raise `execution_timeout_secs`), so for anything long keep the
  id and come back, as in the second example.
- **Logs are kept** on the session's disk for 7 days and survive the session
  sleeping, so an id stays readable. Commands do not survive it: one that was
  running then reads `"failed:interrupted: …"` afterwards.
- **Limits.** Programs run as an unprivileged user with nothing to gain
  privileges with (no `sudo`, no package installation into the system), and
  reach the network the browser reaches.

**Prefer a program and arguments to `sh -c`.** It runs exactly what you wrote,
with no quoting to get wrong, and it is what a session's policy reads: a
policy may allow only certain programs, subcommands, hosts or directories,
and may deny shells outright. When a call is denied by policy, do not look
for another way to run the same thing; say what was refused.
