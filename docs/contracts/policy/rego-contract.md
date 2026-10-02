# Rego contract

A session's policy is a Rego module. There is no other form: the JSON
format of the first design was removed before policies were enforced
anywhere (design, the note of 2026-10-02), so this file is the whole
reference for someone writing a policy.

## What a tenant policy is

One Rego module (Rego v1 syntax), at most 65536 bytes, which:

- declares exactly `package browserjs.policy`;
- defines `allow_tool_call`. The call is allowed only when it evaluates to
  `true`; undefined, `false`, or any other value denies;
- may define any other rules and functions in that package for its own use.

It is `spec.source` of the session's `SessionPolicy` as written (`spec.kind`
is `rego`, the only kind), after the checks below.

`allow_tool_call` is asked for **every tool call** the agent's code makes
from `run_js`, on every server. Deny by default follows from Rego: a call no
rule allows is refused. So a policy that speaks only of `browser_execute`
refuses desktop control and the shell, and one that says
`allow_tool_call := true` allows them all.

Reserved rule names, which a policy may define but nothing reads:
`allow_fetch`, `allow_module`.

## The input

mcp-js v0.21.0-rc.4 builds it in `server/src/engine/mcp_client.rs`
(`McpToolPolicyInput`), for each `mcp.callTool(server, tool, arguments)` made
from `run_js`:

| Field | Value |
|---|---|
| `operation` | always `"mcp_call_tool"` |
| `server` | the upstream server's name: `"browser"` or `"exec"` |
| `tool` | the tool's name |
| `arguments` | the arguments object as the agent's code passed it, or `null` when it passed none |

**Nothing validates `arguments` before the policy sees it**: the tool checks
its arguments after the policy has allowed the call. Every field can be
missing, of any type, or one the tool does not have. Write rules that match
what they positively recognise (`is_array`, `is_string`, `is_number` first)
and let everything else fall through to the denial. The
`examples/*.cases.json` files hold hostile inputs of this kind.

There is nothing in the input that identifies the session, the user or the
MCP client.

### The servers and tools of a session

| `server` | `tool` | What it does | `arguments` |
|---|---|---|---|
| `browser` | `browser_execute` | drives pages over CDP | `{operations: [{type, params?}], tab?, close?}` |
| `browser` | `desktop_execute` | drives the X display with nut.js: mouse, keyboard, screen, clipboard. Whatever a person at the VNC view can do | `{operations: [{type, params?}], config?}` |
| `exec` | `exec` | runs a program directly (no shell) as the desktop's user and returns at once with `{id, status: "started"}` | `{bin: string, args?: [string], timeout: integer (seconds), cwd?: string, env?: {name: string}}` |
| `exec` | `stream_logs` | reads a started command's output from a byte offset | `{id: string, offset: integer}` |
| `exec` | `search_logs` | searches a started command's output | `{id: string, pattern: string}` |
| `exec` | `kill` | stops a running command and its process group | `{id: string}` |

The platform's decision module (`decision-module.rego.tmpl`) asks the
tenant's policy only for these pairs. **A call to any other server or tool
is refused whatever the policy says**: a policy written before a tool
existed cannot have meant to allow it. A tool is added to the module in the
pull request that adds it to this table.

`browser_execute` (`images/browser/browser/server.js`; `input-sample.json`
is a sample). Operation types and their `params`:
`navigate {url, waitUntil}`, `click {selector}`, `type {selector, text,
delay}`, `press {key}`, `select {selector, values}`, `wait {ms, selector}`,
`screenshot {fullPage}`, `setViewport {width, height}`, `url`,
`evaluate {script}` (runs script in the page), `setContent {html}`
(replaces the page's content).

`desktop_execute` (`images/browser/browser/desktop.js`, and "Desktop
control" in `images/mcp-js/run_js.md`). Operation types: `mouse.setPosition`,
`mouse.move`, `mouse.click`, `mouse.doubleClick` (`{x, y, button}`),
`mouse.pressButton`, `mouse.releaseButton`, `mouse.drag {to, from}`,
`mouse.scrollUp` / `Down` / `Left` / `Right {amount, x, y}`,
`mouse.getPosition`, `keyboard.type {text}` or `{keys}`,
`keyboard.pressKey {keys}`, `keyboard.releaseKey {keys}`, `screen.width`,
`screen.height`, `screen.grab`, `screen.grabRegion {left, top, width,
height}`, `screen.colorAt {x, y}`, `getActiveWindow`, `getWindows`,
`clipboard.setContent {text}`, `clipboard.getContent`, `sleep {ms}`.

```json
{ "operation": "mcp_call_tool", "server": "browser", "tool": "desktop_execute",
  "arguments": { "operations": [
    { "type": "mouse.click", "params": { "x": 640, "y": 52 } },
    { "type": "keyboard.type", "params": { "text": "example.com" } },
    { "type": "screen.grab" } ] } }
```

A policy sees operation types and parameters, not what is on the screen:
coordinates do not say which window or element is under them, so "only
click inside Chromium" cannot be written. What can be: which kinds of
operation run (screenshots only, no keyboard, no clipboard), how many, what
text and which keys.

The `exec` server is [mcp-exec](https://github.com/r33drichards/mcp-exec)
running in the browser container. `tools/*.schema.json` are the schemas of
its tools, and [`exec-input.md`](exec-input.md) is the longer guide to
writing rules on a program and its arguments, with six worked policies in
`tools/examples/`.

```json
{ "operation": "mcp_call_tool", "server": "exec", "tool": "exec",
  "arguments": { "bin": "git", "args": ["status"], "timeout": 30 } }
```

A policy sees the program and each argument as the program will receive
them: nothing is split, globbed or expanded. What it must do for itself,
because the server does not:

- **Compare `bin` as a whole string.** `"git"` is looked up on `PATH`;
  `"/usr/bin/git"` and `"./git"` are other strings (the last one relative to
  `cwd`). Allow exact names.
- **Handle `args` absent and `args: []` alike** (the server does), and
  require an array of strings: `object.get(input.arguments, "args", [])`,
  then `is_array` and `is_string` on each.
- **Refuse `env`, or allow only listed names.** The server accepts any
  name, `PATH` and `LD_PRELOAD` included, and a `PATH` of the caller's
  choosing changes what an allowed name runs.
- **Refuse `cwd`, or check it yourself.** It is used as written: no
  normalisation, no symbolic links resolved. A "below this directory" rule
  must refuse `..`, `.` and empty segments, and cannot see a link that
  leads out.
- **Bound `timeout`.** The server has no maximum.
- **Allow-list the field names** (`object.keys(input.arguments) - {…}` is
  empty): the policy is asked before the server refuses unknown fields.

And what no rule can do: confine what an allowed program starts. `sh`,
`bash`, `env`, `xargs`, every interpreter, `find -exec`, `git -c` and
`git` in a repository whose config names programs all run something else.
A policy on `exec` is **a list of entry points, not a sandbox**; what
confines a command is the container. So list few programs, pin their
arguments (a subcommand at `args[0]`, a pattern every argument must
match), and leave launchers out. `stream_logs`, `search_logs` and `kill`
start nothing and can be allowed whenever `exec` is.

### Tools that undo each other's rules

The three ways into a session are not independent, and a policy must be
written with that in mind:

- **A policy that restricts `browser_execute` must deny `desktop_execute`**
  (or allow only its `screen.*` operations): with the mouse and keyboard an
  agent types into the address bar, opens DevTools, or pastes script.
- **A policy that restricts `browser_execute` must deny `exec`** (or allow
  only programs and arguments that can neither make requests nor start
  programs): a program runs inside the container and can call the browser's MCP server
  on `127.0.0.1:8081` and Chromium's debugging port on `127.0.0.1:9222`
  directly, with no policy in the way.
- **A policy that restricts `exec` must deny `desktop_execute`**: the
  desktop has a terminal, and the keyboard types any command into it.

The operator checks this by asking, not by reading: after a module passes
the checks below, it is evaluated against a few probe calls (a page script,
a navigation, a click; a desktop click and keystrokes; `curl` to the
debugging port, a program nobody lists, `sh`, `bash`, `env` and `xargs`,
and the same and a few common programs with `PATH` set in `env`). The outcome is a **warning**, shown by the
editor, the API and `status.warnings`, never an error:

| Code | When |
|---|---|
| `browser_bypass_desktop` | some browser probe is refused, and a desktop click or keystroke is allowed |
| `browser_bypass_shell` | some browser probe is refused, and an arbitrary program or a launcher is allowed |
| `shell_bypass_desktop` | some program is refused, and a desktop click or keystroke is allowed |
| `shell_launcher_allowed` | some program is refused, and `sh`, `bash`, `env` or `xargs` is allowed: the list of programs is then every program |
| `shell_env_allowed` | some program is refused, and a call that sets `PATH` in `env` is allowed |

A warning and not an error because the probes are a heuristic (a policy can
have reasons the operator cannot see, and one can restrict in ways the
probes do not notice), and because refusing to save would only move the
author to `allow_tool_call := true`.

### The presets

`examples/<name>.rego`, each with `examples/<name>.cases.json`; the backend
serves them on the create page. Each begins with a comment that says what
it allows, which is the description people see.

| Preset | `browser_execute` | `desktop_execute` | `exec`, `stream_logs`, `search_logs`, `kill` |
|---|---|---|---|
| `unrestricted` (a new session's default) | everything | everything | everything |
| `browser-only` | everything | denied | denied |
| `no-scripting` | everything but `evaluate` and `setContent` | denied | denied |
| `observe-only` | https pages, `wait`, `screenshot`, `url`, `setViewport` | denied | denied |
| `one-site` | one site over https, short text, no script | denied | denied |
| `form-filling` | two sites, printable text, three keys, bounded viewport | denied | denied |
| `read-only-shell` | everything | denied | `pwd`; `ls` and `cat` on relative paths with no `..` and no hidden names; `git status`, `log`, `diff`, `show` with a few flags; no `env`, no `cwd`; `timeout` 1 to 60; reading output and stopping a command |

## How mcp-js asks

`POST {url}/v1/data/{policy_path}` with body `{"input": <input>}`, no
headers of its own, 5 second timeout, 1 second to connect
(`server/src/engine/opa.rs`). It allows only when the answer is 2xx and
`result.allow` is `true`. A request that fails in transport (it could not be
sent, it timed out, or the status is 502, 503 or 504) is made once more, on
a new connection; if that fails too, the call is denied. An answer is never
asked for twice: a missing `result`, any other non-2xx status or an
unparsable body denies at once.

In a session pod:

- `url`: `http://opa.browserjs-sessions.svc:8181`
- `policy_path`: `browserjs/decision/<session id>/mcp_tools`

so the request is
`POST http://opa.browserjs-sessions.svc:8181/v1/data/browserjs/decision/<session id>/mcp_tools`.

## What is in OPA

| Document | Owner | Content |
|---|---|---|
| `data.browserjs.tenant["<session id>"]` | generated from the tenant's module | the module, with its package clause replaced |
| `data.browserjs.decision["<session id>"].mcp_tools` | platform | `decision-module.rego.tmpl` with `{{SESSION_ID}}` replaced; its `allow` is what mcp-js reads: true only for a server and tool of the table above for which the tenant's `allow_tool_call` is `true` |
| `data.browserjs.loaded` | platform | an object: session ID to the policy's hash (`"sha256:<hex>"`), for every session in the bundle |
| `data.system.authz` | platform | `system-authz.rego`, loaded from a file at start, not from the bundle |

A session with no entry has no decision document; OPA answers `200 {}` and
mcp-js denies.

Session IDs match `^s-([a-z2-7]{10}|[a-z0-9]{5})$`. They contain a hyphen,
so they appear in package clauses in the bracketed form,
`package browserjs.tenant["s-ab2cd"]`, which OPA 1.9.0 accepts (spike 1).

## Tenant checks (the operator, on every module)

In this order; the first three use the AST from
`opa parse --format json --json-include locations`.

1. **Package**: the package path is exactly `data.browserjs.policy`.
   Otherwise `policy_guard_error`, "the package must be browserjs.policy".
2. **Imports**: each import's path starts with `rego`, `future` or `input`.
   Otherwise `policy_guard_error`.
3. **No `data`, no `with`**: no term of type `var` with value `data` occurs
   anywhere in the module's rules or imports, and no expression has a
   `with` modifier. Otherwise `policy_guard_error`, with the term's
   location. (Rules of the same package are called by their own names, so
   nothing legitimate needs `data`.)
4. **Entry rule**: a rule named `allow_tool_call` exists. Otherwise
   `policy_guard_error`, "the policy must define allow_tool_call".
5. **Compile**: `opa check --capabilities capabilities.json` on the module
   succeeds (not `--strict`: a policy that works is not refused for style). Otherwise the errors OPA reports, with its codes
   (`rego_parse_error`, `rego_type_error`, `rego_compile_error`, …) and
   locations.
6. **Rewrite**: the package clause is replaced, at its location from the
   AST, by `package browserjs.tenant["<session id>"]`. Nothing else in the
   text changes, so rows and columns of later errors still match the source.

A module that passes is then put to the probe calls of "Tools that undo
each other's rules" for its warnings.

The hash of a policy is `"sha256:"` followed by the lower-case hex SHA-256
of the module **before** the rewrite (the text stored in `status.rego`).

`spike/tenant-guard.py` is a sketch of checks 1 to 3 with a corpus of modules
they must refuse (design, section 11).

## Capabilities

`capabilities.json` is OPA's own capabilities document for the pinned
version with `builtins` reduced to the names in `capabilities-allowlist.txt`
(112 of 201 in OPA 1.9.0). It is an allow-list: a built-in added by a later
OPA is not available to tenants until someone adds it.

Not on the list, among others: `http.send`, `net.lookup_ip_addr`,
`opa.runtime`, `rego.metadata.*`, `rego.parse_module`, `trace`, `print`,
`walk`, `numbers.range`, `numbers.range_step`, `rand.intn`, `uuid.*`,
`crypto.*`, `io.jwt.*`, `json.patch`, `yaml.*`, `graphql.*`,
`providers.aws.sign_req`.

`opa run` has no capabilities setting: the OPA server itself would load a
module that uses `http.send`. The restriction holds because only the
operator builds bundles, with `opa build --capabilities`, and OPA's API
accepts no policies from anyone (`system-authz.rego`).

## The bundle

One bundle, `browserjs.tar.gz`, built by the operator with
`opa build -b <dir> --capabilities capabilities.json -r <revision>` from:

```
.manifest                       {"roots": ["browserjs"]}
browserjs/loaded/data.json      {"<session id>": "sha256:…", …}
tenant/<session id>.rego        the rewritten tenant module
decision/<session id>.rego      the decision module
```

- `<revision>` is a decimal counter that increases with every published
  bundle of one operator process, prefixed by the process's start time in
  seconds: `<start>-<n>`.
- A session is in the bundle only when its tenant module passed the checks.
  While the current `spec` of a `SessionPolicy` does not pass, the operator
  uses the module in `status.rego` (the last that did) and leaves `status.rego`
  and `status.hash` unchanged; with no such module the session is left out,
  and is denied.
- The bundle is published only after `opa build` succeeds on the whole
  directory. If it fails although every tenant passed alone, the previous
  bundle stays published and the operator logs and reports the error on
  the resources that changed.

## OPA's API

`system-authz.rego`, with `--authentication=token --authorization=basic`:

| Request | Who | Answer |
|---|---|---|
| `POST /v1/data/browserjs/decision/<id>/<category>`, no query string | anyone | the decision |
| the same with any query parameter (`?explain`, `?instrument`, `?provenance`, `?metrics`, `?pretty`) | anyone | 401 |
| `GET /health`, with or without `?bundles` | anyone | health |
| `GET /v1/data/browserjs/loaded` | bearer of the operator's token | the loaded document |
| everything else, including every write | anyone, the operator too | 401 |

The operator's token reaches the policy as the environment variable
`OPERATOR_TOKEN` of the OPA container.
