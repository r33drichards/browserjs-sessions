JavaScript execution service with a persistent memory, a browser, and its desktop:

1. Long-term memory at /data/memory/ (the only path `fs` may touch; survives restarts and is shared by every agent). At the start of a task, run_js `console.log(await fs.readFile("/data/memory/INDEX.md", "utf8"))` (it may not exist yet) and keep INDEX.md updated as you save things.
2. A logged-in, persistent Chromium via `mcp.callTool("browser", "browser_execute", { operations: [...] })` from inside run_js.
3. The desktop that Chromium runs on, driven with nut.js (mouse, keyboard, screen, clipboard) via `mcp.callTool("browser", "desktop_execute", { operations: [...] })` from inside run_js. Prefer `browser_execute` for page content; use this for what is outside the page (browser UI, dialogs, file choosers).
4. Commands on that desktop, as its user (same home directory and files as a terminal there), via `mcp.callTool("browser", "shell_execute", { argv: ["program", "arg", ...] })` from inside run_js. Prefer `argv` (run directly, no shell) to `script` (run by `bash -lc`); a session's policy may allow only some programs, or none.

To give this server a file from your own environment, call `get_artifact_upload_url`, upload the file to the returned URL (`curl -fsS -T ./file '<url>'`, no credentials needed), then read it in run_js with `artifact.get(key)`.

See the run_js tool description for the memory conventions, file uploads, and browser, desktop and shell operations.
