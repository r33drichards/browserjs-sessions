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
| `input.tool` | The tool: `"browser_execute"` or `"desktop_execute"` on `browser`; `"exec"`, `"stream_logs"` or `"search_logs"` on `exec` |
| `input.arguments` | The arguments as the code passed them. Not validated first: any field can be missing or of any type |

A call to a server or tool not listed here is refused whatever the policy
says.

A call no rule allows is refused, so a policy that only speaks of
`browser_execute` refuses desktop control and the shell.

Nothing in the input says which user or client is calling.

A policy that restricts `browser_execute` should refuse `desktop_execute`
and `exec`: the mouse and keyboard, or a shell command, can drive the
browser around its rules. A policy that restricts `exec` should refuse
`desktop_execute`, which can type into a terminal. Saving a policy that
does otherwise gives a warning.

## Where it is kept

| Mode | Edited |
| --- | --- |
| In the app | In the editor on the session's **Policy** tab |
| As code | Through the API or the Terraform provider. The app shows it read-only |

A new session with no policy given gets the unrestricted one.
