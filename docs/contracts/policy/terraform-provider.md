# Terraform provider contract

Provider type `browserjs`; source address `r33drichards/browserjs`. Go,
`terraform-plugin-framework`, protocol version 6. Source in
`terraform-provider-browserjs/` in this repository, a Go module of its own.
It speaks only the API of `backend-api.yaml`, on the API host.

## Provider configuration

```hcl
provider "browserjs" {
  endpoint = "https://api.computeruse.site" # or BROWSERJS_ENDPOINT
  token    = var.browserjs_token            # or BROWSERJS_TOKEN; sensitive
}
```

| Attribute | Type | | Notes |
|---|---|---|---|
| `endpoint` | string | optional | Base URL of the API host, without `/v1`. Env `BROWSERJS_ENDPOINT`. Default `https://api.computeruse.site`. |
| `token` | string, sensitive | optional | Env `BROWSERJS_TOKEN`. Configuring fails when neither is set. |

Every request sends `Authorization: Bearer <token>` and
`User-Agent: terraform-provider-browserjs/<version>`.

## `session`

| Attribute | Type | | Notes |
|---|---|---|---|
| `id` | string | computed | The session ID. Import by it. |
| `name` | string | optional, computed | Updated in place (`PATCH`). Left out, the server names the session. |
| `mcp_url` | string | computed | What an MCP client is pointed at. |
| `state` | string | computed | As the API reports it at read time. Not waited on beyond creation. |
| `owner` | string | computed | |

- Create: `POST /sessions` with the name; then waits until `policy.state` is
  `ready` (the unrestricted policy is loaded), at most `timeouts.create`
  (default 5 minutes).
- Read: `GET /sessions/{id}`; 404 removes it from state.
- Delete: `DELETE /sessions/{id}`. This deletes the session's disk and the
  browser's logins; the documentation recommends
  `lifecycle { prevent_destroy = true }`.
- Requires token scopes `sessions:read`, `sessions:write`.

## `session_policy`

The policy of one session, managed as code. Creating the resource puts the
policy in `iac` mode; destroying it resets the session to the unrestricted
policy in `editor` mode.

| Attribute | Type | | Notes |
|---|---|---|---|
| `id` | string | computed | Equal to `session_id`. Import by it. |
| `session_id` | string | required, forces replacement | |
| `rego` | string | required | The policy: a Rego module, package `browserjs.policy`, at most 65536 bytes. Compared as text. |
| `managed_url` | string | required | `https` URL of where this configuration lives; shown in the UI. |
| `wait_for_ready` | bool | optional, default true | Whether apply waits for the policy to be in force. |
| `version` | number | computed | |
| `hash` | string | computed | |
| `compiled_rego` | string | computed | The module in force: `rego`, or while `state` is `invalid` the last one that compiled. |
| `state` | string | computed | `ready`, `loading`, `invalid`. |

Policies are Rego only. There is no `json` attribute and no JSON policy
format; `kind` is always sent as `"rego"`. A policy decides every tool call:
`browser_execute` and `desktop_execute` on server `browser`, and the tools of
server `exec`. How to write one is `rego-contract.md`; the presets are
`examples/*.rego`. A policy that restricts `browser_execute` must deny
`desktop_execute` and the `exec` server, because either can drive the browser
around the rules; the API returns a warning when it does not
(`browser_bypass_desktop`, `browser_bypass_shell`, and `shell_bypass_desktop`
for the shell), and the provider shows it.

- Plan: `ValidateConfig` checks the length of `rego` and the URL offline;
  `ModifyPlan` calls `POST /policies/validate` when the source is known, and
  turns each `errors[]` entry into an error on `rego` and each `warnings[]`
  entry into a warning on `rego`, with its row and column when it has them
  (the three warnings above have none).
- Create and update: `PUT /sessions/{id}/policy` with
  `{kind: "rego", source, management: {mode: "iac", managed_url}}`. The
  answer's `warnings[]` are shown as at plan. 200 is done. On
  202, when `wait_for_ready`, polls `GET` until `state` is `ready` or
  `timeouts.update` (default 2 minutes) passes; `invalid` is an error with
  the diagnostics. 409 because the session predates policies is an error
  that says to recreate the session.
- Read: `GET /sessions/{id}/policy`. A `kind` other than `rego` is an error.
  If `management.mode` is no longer `iac`
  (someone chose "Manage here instead" in the UI), the resource reports the
  drift by reading `managed_url` as empty, so the next plan shows an update
  that takes the policy back.
- Delete: `DELETE /sessions/{id}/policy`.
- Import: `terraform import session_policy.x s-ab2cd`; the first
  apply puts the policy in `iac` mode if it was not.
- Requires token scopes `policies:read`, `policies:write`.

## Data sources

| Name | Arguments | Attributes |
|---|---|---|
| `session` | `id` or `name` (exactly one; `name` must match one session) | as the resource |
| `sessions` | none | `sessions`: list of objects as the resource |

There is no data source that builds a policy: a policy is Rego text.

## Local installation (before any registry)

`go build -o terraform-provider-browserjs` in the provider directory, then
in `~/.terraformrc` (or `~/.tofurc`):

```hcl
provider_installation {
  dev_overrides {
    "r33drichards/browserjs" = "/path/to/computer-use/terraform-provider-browserjs"
  }
  direct {}
}
```

With `dev_overrides`, `terraform init` is skipped for this provider.

Resource types are `session` and `session_policy`; data-source types are `session`
and `sessions`. Every HCL block must set `provider = browserjs`. Existing prefixed
state requires removal and re-import using the same session IDs; see the
[migration guide](https://computeruse.site/reference/terraform#migrate-from-prefixed-types).
