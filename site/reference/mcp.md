# MCP endpoint and tools

## Endpoint

Each session has one MCP URL. It is shown on the session's page and returned
by the API as `mcp_url`.

```
https://<session id>.sessions.browserjs.com/mcp
```

| | |
| --- | --- |
| Transport | Streamable HTTP |
| Sign-in | OAuth in a browser, with the Google or GitHub account that owns the session |
| Who may call | The session's owner |
| Sleeping session | A call (`POST`) wakes it and waits. The event stream (`GET`) does not wake it and answers `405` until the session runs |

## Tools

### `run_js`

Runs JavaScript or TypeScript in V8 and returns what it printed. Each call
starts fresh: no variables carry over between calls.

| Parameter | Type | Meaning |
| --- | --- | --- |
| `code` | string | The code to run. TypeScript types are removed, not checked. |
| `heap_memory_max_mb` | number, optional | Heap limit in MB. Minimum 4, default 8. |
| `execution_timeout_secs` | number, optional | Time limit in seconds, 1 to 300. Default 30. |

Returns `output` (everything written with `console.log`, `console.info`,
`console.warn`, `console.error`) and `error` if the run failed or timed out.

Inside `run_js`:

| Available | Notes |
| --- | --- |
| `mcp.callTool("browser", "browser_execute", {...})` | Drives the session's browser. See below. |
| `fs` | Node-style file functions, for `/data/memory/` only. Kept on the session's disk. |
| `artifact(key, mime, bytes)` | Returns an image or other file with the result. Up to 16 MiB each. |
| `artifact.get(key)`, `artifact.list()` | Reads stored artifacts and uploaded files. |
| `import` | npm, JSR and URL modules, for example `import { camelCase } from "npm:lodash-es@4.17.21"`. |
| top-level `await` | Supported. |

Not available: `fetch` and other network access from the script, timers
(`setTimeout`, `setInterval`), environment variables, and DOM APIs. Page
scripts run in the browser instead, with the `evaluate` operation.

### `browser_execute`

Called from `run_js`. Runs a list of operations in one tab of the session's
browser.

```js
const r = await mcp.callTool("browser", "browser_execute", {
  operations: [
    { type: "navigate", params: { url: "https://example.com" } },
    { type: "evaluate", params: { script: "document.title" } },
  ],
})
console.log(r.content[0].text)
```

| Parameter | Type | Meaning |
| --- | --- | --- |
| `operations` | array, required | The steps, each `{ type, params }`. Run in order. |
| `tab` | string, optional | Name of the tab to use. Default `"default"`. A tab is created on first use and reused by later calls with the same name. Calls on one tab run one at a time. |
| `close` | boolean, optional | Close the tab after the steps. Default: leave it open. |

| Operation | `params` | Notes |
| --- | --- | --- |
| `navigate` | `{ url, waitUntil? }` | Waits for `domcontentloaded` by default. Times out after 45 seconds. |
| `wait` | `{ ms }` or `{ selector, ms? }` | Waits a fixed time, or for a visible element (10 seconds unless `ms` is given). At most 30 seconds. |
| `click` | `{ selector }` | Waits up to 10 seconds for the element. |
| `type` | `{ selector, text, delay? }` | Clicks the element, then types. |
| `press` | `{ key }` | For example `"Enter"`. |
| `select` | `{ selector, values }` | Chooses options of a `<select>`. |
| `evaluate` | `{ script }` | Runs the script in the page and returns its result. |
| `screenshot` | `{ fullPage? }` | Returned as a PNG image. |
| `url` | `{}` | The current URL and title. |
| `setViewport` | `{ width, height }` | Default 1280 by 800. Between 320 by 200 and 3840 by 2160. |
| `setContent` | `{ html }` | Replaces the page with the given HTML. |

The steps stop at the first failure. The result then includes a screenshot
of the page at that moment.

A tab stays open between calls. A later call continues on the same page,
with the same URL and scroll position.

### `get_artifact_upload_url`

Gives the agent a way to hand the session a file from its own environment.
Takes a `key` and a `mime_type` and returns a `url`. The agent uploads the
file to that URL with an HTTP `PUT`, then reads it in `run_js` with
`artifact.get(key)`.

The URL works once, expires after 10 minutes, and takes up to 16 MiB. It
needs no sign-in: the URL itself is the credential. It works only while the
session is running.

### `get_artifact` and `list_artifacts`

`get_artifact` returns a stored artifact by `key`. Use it for one that was
too large to come back with the `run_js` result (more than 8 MiB in total).
`list_artifacts` lists what is stored.

## Memory

`/data/memory/` is on the session's disk. Files written there with `fs`
survive across calls, sleep and restarts, until the session is deleted. The
server's instructions ask agents to keep an index at `/data/memory/INDEX.md`.
