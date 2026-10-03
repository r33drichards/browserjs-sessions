# Computer-use MCP

A standalone code-mode MCP project for browser and desktop control. It uses
[mcp-v8](https://github.com/r33drichards/mcp-js) to expose `run_js`, backed by
this project's `browser_execute` and `desktop_execute` MCP tools. It does
not need the parent repository's backend, database, OAuth, or Kubernetes.

## Run locally

Install Node.js 22 or newer and mcp-v8 v0.21.0-rc.4 or newer (the hosted
image pins v0.21.0-rc.4). Older mcp-v8 builds that only support stdio/SSE
upstreams cannot read this project's HTTP server configuration.
From this directory:

```sh
npm run setup
```

Start Chrome/Chromium with a dedicated profile and remote debugging enabled:

```sh
chromium --remote-debugging-port=9222 --user-data-dir="$HOME/.computer-use-mcp/chrome"
```

Use your installed Chrome executable in place of `chromium`. Configure your
MCP client to launch this project directly (use absolute paths):

```json
{
  "mcpServers": {
    "computer-use": {
      "command": "node",
      "args": ["/absolute/path/to/computer-use-mcp/bin/start.mjs"]
    }
  }
}
```

The launcher starts the browser MCP on an available loopback port, waits
for it, then starts mcp-v8 over stdio. It stops both children on shutdown or
failure. Stdout is reserved for MCP. Tabs and gateway session data live in
`~/.computer-use-mcp/`; each JavaScript execution starts fresh.

```js
const result = await mcp.callTool("browser", "browser_execute", {
  operations: [{ type: "navigate", params: { url: "https://example.com" } },
               { type: "evaluate", params: { script: "document.title" } }]
});
console.log(result);
```

| Setting | Default | Purpose |
| --- | --- | --- |
| `CDP_URL` | `http://127.0.0.1:9222` | Existing Chrome/Chromium debugging endpoint |
| `MCP_V8_BIN` | `mcp-v8` on PATH | Gateway executable |
| `COMPUTER_USE_STATE_DIR` | `~/.computer-use-mcp` | Persistent tabs and sessions |
| `COMPUTER_USE_EXEC_URL` | unset | Optional already-running mcp-exec HTTP endpoint |

The local launcher does not start Chromium or mcp-exec. It grants browser
and desktop tool access, and exec tool access when configured. Direct host
filesystem and network access from JavaScript remain disabled; the tools
can act on the connected computer. Connect only trusted MCP clients.

## Platform status

Browser automation uses CDP and is independent of the browser host's OS.
The local launcher uses Node APIs, but native desktop support has not been
validated on macOS or Windows. The existing desktop code uses nut.js and
Linux/X11 helpers (`xdpyinfo`, and `xclip` for file clipboard support).
The packaged Linux desktop currently targets x86_64. Extracting this
project does not change those limitations.

## Layout and hosted integration

- `browser/`: Node MCP server, native desktop worker, and hosted startup scripts.
- `bin/start.mjs`: local stdio supervisor.
- `code-mode/`: hosted mcp-v8 image, policies, and hosted tool instructions.
- `desktop/`: Linux desktop configuration.
- `flake.nix`, `Dockerfile`: self-contained Linux desktop image build.
- `test/`: unit and integration checks, including real desktop image smoke tests.

The parent service consumes these same sources. Build the hosted images
from the parent root with:

```sh
docker build -t browserjs/browser:dev computer-use-mcp
docker build -t browserjs/mcp-js:dev computer-use-mcp/code-mode
```

The hosted gateway keeps its `/data/memory`, artifacts, HTTP transport,
and service authentication integration. The local gateway uses its own
instructions and does not enable those hosted workflows.

## Validation

```sh
npm test
# Linux with Docker: exercises the actual packaged desktop
./test/desktop-image-smoke.sh browserjs/browser:dev
# Chrome integration (set the installed executable)
CHROME=/path/to/chrome bash test/tabs.test.sh
```
