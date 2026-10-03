JavaScript execution service with a persistent memory, a browser, and its desktop:

1. Long-term memory at /data/memory/ (survives restarts and is shared by every agent; `fs` may access all of /data/). At the start of a task, run_js `console.log(await fs.readFile("/data/memory/INDEX.md", "utf8"))` (it may not exist yet) and keep INDEX.md updated as you save things.
2. A logged-in, persistent Chromium (started by the first call) via `mcp.callTool("browser", "browser_execute", { operations: [...] })` from inside run_js.
3. The desktop (XFCE, with a terminal, a file manager and a text editor), driven with nut.js (mouse, keyboard, screen, clipboard) via `mcp.callTool("browser", "desktop_execute", { operations: [...] })` from inside run_js. Prefer `browser_execute` for page content; use this for what is outside the page (browser UI, dialogs, file choosers).
4. Commands on that desktop, as its user (same home directory and files as a terminal there), via `mcp.callTool("exec", "exec", { bin: "program", args: ["arg", ...], timeout: 60 })` from inside run_js: the program is run directly (no shell) and the call returns an id at once; read the output and the exit status with `mcp.callTool("exec", "stream_logs", { id, offset: 0 })`. A session's policy may allow only some programs or arguments, or none.

To give this server a file from your own environment, call `get_artifact_upload_url`, upload the file to the returned URL (`curl -fsS -T ./file '<url>'`, no credentials needed), then read it in run_js with `artifact.get(key)`.

See the run_js tool description for the memory conventions, file uploads, and browser, desktop and shell operations.

Save reusable scripts under /data/scripts/ and maintain /data/scripts/INDEX.md with each script’s purpose and arguments. Browser scripts can be read with fs and passed to browser_execute evaluate.
