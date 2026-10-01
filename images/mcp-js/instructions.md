JavaScript execution service with two persistent resources:

1. Long-term memory at /data/memory/ (the only path `fs` may touch; survives restarts and is shared by every agent). At the start of a task, run_js `console.log(await fs.readFile("/data/memory/INDEX.md", "utf8"))` (it may not exist yet) and keep INDEX.md updated as you save things.
2. A logged-in, persistent Chromium via `mcp.callTool("browser", "browser_execute", { operations: [...] })` from inside run_js.

To give this server a file from your own environment, call `get_artifact_upload_url`, upload the file to the returned URL (`curl -fsS -T ./file '<url>'`, no credentials needed), then read it in run_js with `artifact.get(key)`.

See the run_js tool description for the memory conventions, file uploads, and browser operations.
