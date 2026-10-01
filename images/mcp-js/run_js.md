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
- **No timers**: Functions like `setTimeout` and `setInterval` are not available.
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
