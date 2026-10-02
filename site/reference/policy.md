# Policy format

::: warning Coming, not yet enabled
Policies are built and switched off. See
[Live, coming and planned](/reference/status).
:::

A session has one policy. It is asked about every `mcp.callTool` that code
in `run_js` makes. A call it does not allow is refused, and the code gets an
error. If the policy cannot be asked, the call is refused.

A policy is written in Rego. A new session can start from a ready-made one
(unrestricted, browser only, no scripting, observe only, one site, form
filling, read-only shell) and edit it.

## Rego

One module in Rego v1 syntax, at most 65536 bytes.

- It declares `package browserjs.policy`. The name is fixed, and carries the
  product's earlier name.
- It defines `allow_tool_call`. A call is allowed only when that is `true`.

The input for each call:

| Field | Value |
| --- | --- |
| `input.operation` | `"mcp_call_tool"` |
| `input.server` | The capability's server: `"browser"`, or `"exec"` for the shell |
| `input.tool` | The tool: `"browser_execute"` or `"desktop_execute"` on `browser`; `"exec"`, `"stream_logs"`, `"search_logs"` or `"kill"` on `exec` |
| `input.arguments` | The arguments as the code passed them. Not validated first: any field can be missing or of any type |

A call to a server or tool not listed here is refused whatever the policy
says.

A call no rule allows is refused, so a policy that only speaks of
`browser_execute` refuses desktop control and the shell.

Nothing in the input says which user or client is calling.

### Arguments of each tool

| Server | Tool | `input.arguments` |
| --- | --- | --- |
| `browser` | `browser_execute` | `{ operations: [{ type, params }] }`. Types: `navigate`, `click`, `type`, `press`, `select`, `wait`, `screenshot`, `setViewport`, `url`, `evaluate`, `setContent` |
| `browser` | `desktop_execute` | `{ operations: [{ type, params }] }`. Types such as `mouse.click`, `keyboard.type`, `keyboard.pressKey`, `screen.grab`, `clipboard.getContent` |
| `exec` | `exec` | `{ bin, args, timeout, cwd, env }`: a program run directly, with no shell. `args`, `cwd` and `env` are optional; `timeout` is in seconds |
| `exec` | `stream_logs`, `search_logs`, `kill` | `{ id, ... }`: read or stop a command that was started. They start nothing |

For `exec`, compare `bin` as a whole string (`"git"` and `"/usr/bin/git"`
are different), check `args` one by one, bound `timeout`, and refuse `env`
and `cwd` unless a rule checks them: `env` can set `PATH`, which changes
what a program name runs. A list of programs is a list of entry points, not
a sandbox. An allowed program can start others.

## Ready-made policies

| Policy | Browser | Desktop | Shell |
| --- | --- | --- | --- |
| Unrestricted (the default) | Everything | Everything | Everything |
| Browser only | Everything | Refused | Refused |
| No scripting | Everything except running script in a page or replacing its content | Refused | Refused |
| Observe only | Open https pages, wait, take screenshots | Refused | Refused |
| One site | One site and its subdomains, short typed text | Refused | Refused |
| Form filling | The sites you name, printable text, a few keys | Refused | Refused |
| Read-only shell | Everything | Refused | `pwd`, `ls`, `cat` on files below the home folder, `git status`, `log`, `diff`, `show`; 60 seconds at most |

## Warnings when saving

The tools are not independent, so some policies restrict less than they
appear to. Saving one of these succeeds with a warning.

| Warning | The policy |
| --- | --- |
| `browser_bypass_desktop` | Restricts the browser but lets the desktop click or type. The mouse and keyboard reach the address bar and DevTools |
| `browser_bypass_shell` | Restricts the browser but lets `exec` run arbitrary programs. A program can call the browser's control ports inside the session |
| `shell_bypass_desktop` | Restricts `exec` but lets the desktop click or type. The keyboard can type any command into a terminal |
| `shell_launcher_allowed` | Lists programs but allows `sh`, `bash`, `env` or `xargs`, each of which runs any other program |
| `shell_env_allowed` | Lists programs but lets a call set `PATH` |

The warnings come from trying a few calls against the policy. They catch
the common mistakes, not every one.

## Where it is kept

| Mode | Edited |
| --- | --- |
| In the app | In the editor on the session's **Policy** tab |
| As code | Through the API or the Terraform provider. The app shows it read-only |

A new session with no policy given gets the unrestricted one.
