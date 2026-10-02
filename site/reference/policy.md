# Policy format

::: warning Coming, not yet enabled
Policies are built and switched off. See
[Live, coming and planned](/reference/status).
:::

A session has one policy. It is asked about every `mcp.callTool` that code
in `run_js` makes. A call it does not allow is refused, and the code gets an
error. If the policy cannot be asked, the call is refused.

A policy is written in one of two forms.

## JSON, version 1

Deny by default. A browser operation is allowed if it is not in
`deny.operations` and is either in `allow.operations` or passes one of
`allow.rules`. A `browser_execute` call is allowed only if every operation
in it is.

| Field | Meaning |
| --- | --- |
| `version` | `1`. Required |
| `description` | Free text, up to 1024 characters |
| `allow.operations` | Operations allowed with any parameters. `"*"` means all |
| `allow.rules` | Up to 64 rules: `{ operation, constraints }`. Allowed when every constraint passes |
| `deny.operations` | Operations refused whatever `allow` says |

Operations: `click`, `evaluate`, `navigate`, `press`, `screenshot`,
`select`, `setContent`, `setViewport`, `type`, `url`, `wait`.

`constraints` is keyed by a parameter of the operation, such as `url` or
`text`. A constrained parameter that is missing fails.

| Constraint | The parameter must be |
| --- | --- |
| `min`, `max` | A number within the bound |
| `max_length` | A string of at most this many characters |
| `pattern` | A string matching this regular expression (RE2) |
| `allowed` | One of these values |
| `hosts` | A URL whose host is one of these: an exact name, or `*.name` for any subdomain |
| `schemes` | A URL with one of these schemes: `http`, `https` |

Version 1 of the JSON form describes browser operations only. A JSON policy
refuses `desktop_execute`. Covering desktop and shell calls is planned.

## Rego

One module in Rego v1 syntax, at most 65536 bytes.

- It declares `package browserjs.policy`. The name is fixed, and carries the
  product's earlier name.
- It defines `allow_tool_call`. A call is allowed only when that is `true`.

The input for each call:

| Field | Value |
| --- | --- |
| `input.operation` | `"mcp_call_tool"` |
| `input.server` | The capability's server, `"browser"` |
| `input.tool` | The tool, such as `"browser_execute"` or `"desktop_execute"` |
| `input.arguments` | The arguments as the code passed them. Not validated first: any field can be missing or of any type |

Nothing in the input says which user or client is calling.

A JSON policy is turned into Rego of this form, so both are checked the same
way.

## Where it is kept

| Mode | Edited |
| --- | --- |
| In the app | In the editor on the session's **Policy** tab |
| As code | Through the API or the Terraform provider. The app shows it read-only |

A new session with no policy given gets the unrestricted one.
