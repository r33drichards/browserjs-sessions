# Terraform provider

> The provider calls the API through the SDK's Go package (`sdk/go`), which
> needs cgo and the SDK's library: see "It is built on the SDK" in the
> provider's README for what that does to building and releasing it.

`terraform-provider-browserjs/` is a provider for Terraform and OpenTofu that
creates sessions and manages their policies as code. It is built in this
repository and installed locally; it is in no registry yet.

- Installing it, and trying it against a fake API:
  [`terraform-provider-browserjs/README.md`](../terraform-provider-browserjs/README.md).
- Every argument and attribute:
  [`terraform-provider-browserjs/docs/`](../terraform-provider-browserjs/docs/index.md).
- The contract: [`contracts/policy/terraform-provider.md`](contracts/policy/terraform-provider.md),
  over the API of [`contracts/policy/backend-api.yaml`](contracts/policy/backend-api.yaml).
- The design: section 8 of
  [`plans/2026-10-02-session-policies-design.md`](plans/2026-10-02-session-policies-design.md).

## A configuration

```hcl
terraform {
  required_providers {
    browserjs = { source = "r33drichards/browserjs" }
  }
}

provider "browserjs" {
  # endpoint and token from BROWSERJS_ENDPOINT / BROWSERJS_TOKEN
}

resource "browserjs_session" "research" {
  name = "research"

  lifecycle {
    prevent_destroy = true
  }
}

resource "browserjs_session_policy" "research" {
  session_id  = browserjs_session.research.id
  managed_url = "https://github.com/r33drichards/infra/tree/main/browserjs"
  rego        = file("${path.module}/no-scripting.rego")
}
```

The whole example, with another policy on two more sessions, is
[`examples/session-policies/`](../terraform-provider-browserjs/examples/session-policies/main.tf).

## Writing a policy

A policy is a Rego module of package `browserjs.policy` that defines
`allow_tool_call`. Rego is the only kind: there is no JSON format. The
platform asks the policy about every tool call an agent makes, with
`input.server`, `input.tool` and `input.arguments`: `browser_execute` and
`desktop_execute` on server `browser`, and `exec`, `stream_logs`,
`search_logs` and `kill` on server `exec`. A call the policy does not allow is refused,
so a policy covers the desktop and the shell by having a rule for them, or by
having none. The reference is
[`contracts/policy/rego-contract.md`](contracts/policy/rego-contract.md), and
seven policies to start from are in
[`contracts/policy/examples/`](contracts/policy/examples/).

**A policy that restricts `browser_execute` must deny `desktop_execute` and
the `exec` server.** Either can drive the browser around the rules: the
desktop by typing into the address bar or DevTools, a shell command by
reaching the browser's own control ports. The API does not refuse such a
policy; it returns a warning (`browser_bypass_desktop`,
`browser_bypass_shell`, and `shell_bypass_desktop` for a policy that
restricts the shell and leaves the desktop open), which the provider shows
as a warning on `rego` at plan and at apply.

## Signing in

The provider talks to the API host (`https://api.<domain>`, paths under
`/v1`) with an API token: `Authorization: Bearer bjs_…`. Tokens are created on
the Tokens page of the UI and need the scopes `sessions:read`,
`sessions:write`, `policies:read` and `policies:write` (the first two for
`browserjs_session`, the last two for `browserjs_session_policy`).

`endpoint` and `token` are provider arguments; `BROWSERJS_ENDPOINT` and
`BROWSERJS_TOKEN` are used when they are left out, and an argument wins over
its variable. With no token at all, configuring fails. The token is marked
sensitive, is sent only in the `Authorization` header, and appears in no log
line and no error message. An `http` endpoint that is not on loopback gets a
warning, since the token would cross the network in the clear.

## What the resources do

**`browserjs_session`** creates a session and waits until its policy is
`ready` (the unrestricted policy, which allows the browser, desktop control
and the shell, is loaded), for at most `timeouts.create`,
5 minutes by default. The session is written to the state before the wait, so
one that never becomes ready is tainted rather than lost. A rename is an
update in place. Destroying a session deletes its disk and the browser's
logins: use `prevent_destroy`.

**`browserjs_session_policy`** is the policy of one session. The resource
existing is what "managed as code" means:

- **create and update** send the policy with `management: {mode: "iac",
  managed_url}`, in one call. The UI then shows the policy read-only, with a
  link to `managed_url`.
- **destroy** resets the session to the unrestricted policy in `editor`
  mode.

A policy change is always an update in place. Only `session_id` forces a
replacement, and that replaces the policy resource, never a session.

| Situation | What a plan shows |
|---|---|
| The policy text changes, if only in its formatting or a comment | an update in place; `version`, `hash`, `compiled_rego`, `state` known after apply |
| Only `wait_for_ready` or `timeouts` change | an update that sends nothing to the API |
| Somebody chose "Manage here instead" in the UI | `managed_url` changing from `""`; the apply takes the policy back |
| Somebody edited the policy in the UI | the source changing back, and `managed_url` from `""` |
| The session is gone | the policy to be created again (and failing, unless the session is too) |
| The policy does not validate | an error on `rego` with its line and column, from `POST /policies/validate` |
| The policy leaves a way around its own rules | a warning on `rego`, with no line: it is about the policy as a whole |
| The API cannot validate just now (503) | a warning; the apply validates again before saving |

An apply waits for the policy to be in force: on `202` it polls until `ready`,
for at most `timeouts.create` or `timeouts.update` (2 minutes by default).
`invalid` fails the apply with the compile errors; the policy before it stays
in force. `wait_for_ready = false` returns at once and records `loading`.

A session created before policies existed cannot be given one: the apply
fails saying to recreate the session.

Destroying the resource when the policy is already back in `editor` mode
leaves the policy as it is, with a warning. The API refuses a token's reset
in that mode, and what somebody wrote in the UI is theirs.

**Import** both by session ID: `tofu import browserjs_session_policy.x s-ab2cd`.
A policy imported from `editor` mode reads `managed_url` as `""`, so the
first apply puts it in `iac` mode.

## Data sources

- `browserjs_session`: one session by `id` or by `name` (which must match
  exactly one). The way to manage the policy of a session made in the UI
  without importing the session.
- `browserjs_sessions`: all of the token owner's sessions.

There is no data source that builds a policy: a policy is a `.rego` file,
read with `file()`, or a heredoc.

## Where the contract was silent

- `timeouts.create` exists on `browserjs_session_policy` beside the
  contract's `timeouts.update`, with the same default.
- A read that finds a policy of a kind other than `rego` is an error. The
  API has no other kind; the provider does not guess at one.
- Renaming a session uses `PATCH /sessions/{id}` and deleting one
  `DELETE /sessions/{id}`, which exist today and which `backend-api.yaml`, a
  fragment, covers with "keep their behaviour".
- Plan-time validation answered `503` is a warning, not an error.
- Destroy on a policy in `editor` mode: above.

## Not yet verified

Everything is tested against a fake written from `backend-api.yaml`, with
OpenTofu 1.10.7 and Terraform 1.16.4. Until tracks C and E are merged and
deployed, nothing has run against the real API: token authentication on the
`api.` host, the operator's diagnostics (their `row` and `col`), how long a
real policy takes to be `ready`, and whether the API returns `source` byte
for byte as it was sent (`rego` is compared as text, so a reformatted one
would plan a change). The acceptance tests are the check:
`BROWSERJS_ENDPOINT=… BROWSERJS_TOKEN=… make testacc` against a local
deployment.
