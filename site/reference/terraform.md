# Terraform and OpenTofu

The `browserjs` provider creates Computer Use sessions and manages their
policies through the [HTTP API](/reference/api). The provider retains the product's earlier name; resource and data-source
types use `session`, `session_policy`, and `sessions`. Each block must set
`provider = browserjs` because these type names do not infer the provider.

This page covers every resource and data source in the Computer Use provider:

| Type | Name | Use it to |
| --- | --- | --- |
| Resource | [`session`](#resource-session) | Create, rename, resize and delete a session |
| Resource | [`session_policy`](#resource-session-policy) | Manage one session's Rego policy as code |
| Data source | [`session`](#data-source-session) | Look up an existing session by ID or name |
| Data source | [`sessions`](#data-source-sessions) | List every session belonging to the token's owner |

There are no resources for API tokens, sleep/wake actions, or network rules
in this provider. Create tokens in the app and use the API or app for session
actions. A data source reads a session; it does not create or delete it.

## Migrate from prefixed types

This is a breaking type-name change. The provider source remains
`r33drichards/browserjs`. Rename resource and data blocks and their references,
and add `provider = browserjs` to every block:

| Previous type | Current type |
| --- | --- |
| Resource `browserjs_session` | Resource `session` |
| Resource `browserjs_session_policy` | Resource `session_policy` |
| Data source `browserjs_session` | Data source `session` |
| Data source `browserjs_sessions` | Data source `sessions` |

For existing state, pause automated applies and record each session ID before
upgrading. Back up state, install the updated provider, and update configuration.
Then forget the old addresses and import the same remote objects at the new
addresses. These commands do not delete the sessions or change their policies:

```sh
terraform state pull > terraform-state-before-type-rename.json
terraform state rm browserjs_session_policy.research browserjs_session.research
terraform import session.research s-ab2cd
terraform import session_policy.research s-ab2cd
terraform plan
```

Use your actual addresses and session IDs; repeat for every instance, including
modules and `for_each` keys. Remove old data-source addresses from state with
`terraform state rm data.browserjs_session.research data.browserjs_sessions.account`
if present; the next plan reads the renamed data sources. OpenTofu users can
replace `terraform` with `tofu`. Keep the state backup private because it can
contain sensitive values. Do not apply until all imports succeed and the plan
shows no unintended replacements or policy changes.

`terraform state mv` cannot change resource types. See the Terraform references
for [state removal](https://developer.hashicorp.com/terraform/cli/commands/state/rm)
and [import](https://developer.hashicorp.com/terraform/cli/commands/import).

## Install and authenticate

::: warning Not yet published
The provider works with the live API but is not in a Terraform or OpenTofu
registry. Build and install it locally before running `init`.
:::

### Install the local provider

From a checkout of the repository:

```sh
git clone https://github.com/r33drichards/computer-use.git
cd computer-use/terraform-provider-browserjs
nix develop ..#sdk -c make install
```

This builds the provider and its SDK library, then installs local version
`0.1.0` into `~/.terraform.d/plugins` for both Terraform and OpenTofu. That
version identifies the local build; it is not a published release. The build
requires Go, Cargo and a C compiler, supplied by the SDK dev shell. Supported
platforms are macOS arm64/x86_64 and Linux arm64/x86_64 with glibc 2.35 or
newer; Windows and Alpine Linux are not supported.

The commands below use Terraform. With OpenTofu, replace `terraform` with
`tofu`; the HCL configuration is the same.

### Configure the provider

Create an **API token** in the app. For all examples on this page, give it
`sessions:read`, `sessions:write`, `policies:read` and `policies:write`.
`sessions:connect` is needed only if that token will also call MCP.

```sh
export BROWSERJS_TOKEN='bjs_...'
# Optional; this is the default:
export BROWSERJS_ENDPOINT='https://api.computeruse.site'
mkdir -p ~/computeruse-terraform
cd ~/computeruse-terraform
```

Save this as `providers.tf` in that new directory:

```hcl
terraform {
  required_providers {
    browserjs = {
      source  = "r33drichards/browserjs"
      version = "0.1.0"
    }
  }
}

provider "browserjs" {
  # Uses BROWSERJS_TOKEN and BROWSERJS_ENDPOINT.
}
```

The API token belongs to an account. Resources create sessions for that
account, and data sources see that account's sessions.

| Provider argument | Type | Default or environment variable |
| --- | --- | --- |
| `endpoint` | String, optional | `BROWSERJS_ENDPOINT`, then `https://api.computeruse.site`. Use the API host's base URL, without `/v1` |
| `token` | Sensitive string, optional in HCL | `BROWSERJS_TOKEN`. A token is required to configure the provider |

An explicit nonempty provider argument takes precedence over its environment
variable. Keep the token in the environment rather than committing it to
HCL. For a self-hosted deployment, set `endpoint` to its API host; a nonlocal
`http` endpoint produces a warning because credentials cross it unencrypted.

For a development build using `dev_overrides`, follow the
[provider installation instructions](https://github.com/r33drichards/computer-use/blob/main/terraform-provider-browserjs/README.md#for-development-dev_overrides).
That route uses `plan` directly rather than registry installation through
`init`. The examples below use the local installation above.

## Concepts

A **resource** means Terraform manages that object's lifetime. A session
resource owns the desktop and its disk; a policy resource owns the rules of
one session. They are separate so you can manage a policy on a session
created in the app without managing or importing the session itself.

A **data source** reads an existing object and exposes its attributes to
other blocks. Removing a data source does not delete the remote session.
Using its ID in a policy resource creates a dependency on that lookup.

**Terraform state** records which remote IDs belong to your resource
addresses. Keep that state between runs, and use import to adopt an existing
object rather than declaring it as a new resource. One Terraform state
should manage each session and each session's policy; two configurations
trying to manage the same policy overwrite each other's desired source.

**Plan and apply** reconcile the HCL with the API. A name or size change
updates a session in place; a policy text change updates its rules in place.
The provider does not turn Terraform into the agent: executing MCP calls,
sleeping and waking are separate operations in the API or app.

## Complete configuration

Keep `providers.tf` from the setup section. Save the following as `main.tf`.
It creates one session, restricts its browser operations, reads that session
back, lists the account's sessions, and exposes useful outputs.

```hcl
resource "session" "research" {
  provider = browserjs

  name = "research"
  size = "small"

  timeouts {
    create = "10m"
  }

  lifecycle {
    prevent_destroy = true
  }
}

resource "session_policy" "research" {
  provider = browserjs

  session_id     = session.research.id
  managed_url    = "https://github.com/example/infra/tree/main/desktops"
  wait_for_ready = true

  rego = <<-EOT
    package browserjs.policy

    import rego.v1

    allow_tool_call if {
        input.server == "browser"
        input.tool == "browser_execute"
        is_array(input.arguments.operations)
        every op in input.arguments.operations {
            op.type in {"navigate", "click", "type", "press", "select", "wait",
                        "screenshot", "setViewport", "url"}
        }
    }
  EOT

  timeouts {
    create = "5m"
    update = "5m"
  }
}

data "session" "research" {
  provider = browserjs

  id = session.research.id

  depends_on = [session_policy.research]
}

data "sessions" "account" {
  provider = browserjs

  # Read after this configuration has created its session and policy.
  depends_on = [session_policy.research]
}

output "research" {
  value = {
    id           = data.session.research.id
    mcp_url      = data.session.research.mcp_url
    state        = data.session.research.state
    size         = data.session.research.size
    pending_size = data.session.research.pending_size
    owner        = data.session.research.owner
  }
}

output "policy" {
  value = {
    state   = session_policy.research.state
    version = session_policy.research.version
    hash    = session_policy.research.hash
  }
}

output "account_sessions" {
  value = {
    for session in data.sessions.account.sessions :
    session.id => {
      name    = session.name
      mcp_url = session.mcp_url
      state   = session.state
    }
  }
}
```

Replace `managed_url` with the https URL of your configuration repository.
The Rego allows browser operations except page scripting and replacing page
content. It denies desktop control and the shell, which could otherwise
work around those browser rules. See [Policy format](/reference/policy).

Run:

```sh
terraform init
terraform fmt
terraform validate
terraform plan -out=tfplan
terraform apply tfplan
terraform output
```

A session starts with the unrestricted policy; the separate policy resource
then changes it. Finish applying the configuration and confirm the policy
is `ready` before connecting an agent. The dependencies above make the
example's reads and outputs wait for the policy resource. They do not make
session creation and policy attachment one atomic operation.

## Resource: session

Creates one session and owns its lifecycle. Renaming or resizing updates it
in place and retains its ID and disk. Destroying it deletes its disk, files,
logins and agent memory. `prevent_destroy` in the example blocks a planned
destruction while that lifecycle rule remains in the configuration.

### Arguments

| Argument | Type | Required | Meaning |
| --- | --- | --- | --- |
| `name` | String | No | Session name. Omit to let the server name it. Changes in place |
| `size` | String | No | `small`, `medium` or `large`, as offered by the deployment. Defaults to the server's default, currently `small` |
| `timeouts` | Block | No | Contains only `create`: how long to wait for the new session's initial policy to be ready. Default `5m` |

### Read-only attributes

| Attribute | Type | Meaning |
| --- | --- | --- |
| `id` | String | Session ID, for example `s-ab2cd` |
| `mcp_url` | String | MCP URL returned by the API |
| `owner` | String | Account owning the session |
| `state` | String | Last-read session state, such as `starting`, `running` or `suspended` |
| `pending_size` | String | Size requested for the next start; empty when none is pending |

The configured `size` reports the requested size, including when a resize
is pending. It can differ from the size the awake desktop is still running
at. See [Session sizes](/reference/session-sizes).

### Creation and resize

Creation waits for the initial unrestricted policy to be `ready`, not for
the desktop to reach `running`. If that wait fails, the provider retains the
created session in Terraform state with an error; a failed creation can
leave it tainted for replacement on the next apply. Inspect the plan before
retrying.

Change `size` in HCL and apply to resize. An asleep or stopped session
changes immediately; an awake one takes the new size on its next start.
Resizing drops the saved process snapshot, so the next start is fresh from
the persistent disk. Files and logins remain; open windows and running
programs do not. Terraform does not issue a stop or wake to finish a pending
resize. Use the app or API when you want the next start.

### Standalone example

With the shared `providers.tf`, save this as `main.tf`:

```hcl
resource "session" "desktop" {
  provider = browserjs

  name = "automation-desktop"
  size = "medium"

  timeouts {
    create = "10m"
  }

  lifecycle {
    prevent_destroy = true
  }
}

output "desktop_mcp_url" {
  value = session.desktop.mcp_url
}
```

This creates an unrestricted session. Add a policy resource to restrict it.
The returned MCP URL uses browser sign-in. To connect with an API token, use
the token endpoint described in [Use it from code](/guides/use-from-code).

### Import

Define the resource in HCL, then import by session ID:

```sh
terraform import session.desktop s-ab2cd
terraform plan
```

Match `name` and `size` to the existing session before applying. Import
brings the session under Terraform's lifecycle; it does not import its policy.

## Resource: session_policy

Manages one session's Rego policy. It changes the policy to `iac` mode, with
the app showing a read-only policy and a link to `managed_url`. Policy edits
apply in place and restart neither the session nor its desktop.

### Arguments

| Argument | Type | Required | Meaning |
| --- | --- | --- | --- |
| `session_id` | String | Yes | ID of the session. Changing it replaces the policy resource and resets the old session's policy; it does not replace either session |
| `rego` | String | Yes | Rego v1 module declaring `package browserjs.policy` and defining `allow_tool_call`. Source must be nonempty and at most 65536 bytes |
| `managed_url` | String | Yes | https URL of the configuration's location, shown in the app |
| `wait_for_ready` | Boolean | No | Default `true`. Wait for the saved policy to be in force |
| `timeouts` | Block | No | `create` and `update` are the readiness wait limits; each defaults to `2m` |

### Read-only attributes

| Attribute | Type | Meaning |
| --- | --- | --- |
| `id` | String | Same as `session_id` |
| `state` | String | `ready`, `loading` or `invalid` |
| `version` | Number | Policy version; rises with a change |
| `hash` | String | Hash of the policy in force |
| `compiled_rego` | String | Rego module in force; while invalid, the previous module that compiled |

### Policy files and readiness

Instead of a heredoc, keep the source beside your HCL:

```hcl
resource "session_policy" "research" {
  provider = browserjs

  session_id  = session.research.id
  managed_url = "https://github.com/example/infra/tree/main/desktops"
  rego        = file("${path.module}/policy.rego")
}
```

Use this in place of the policy block in the complete configuration, and
save its Rego module as `policy.rego`. Policy text is compared exactly;
formatting or comment changes plan an update. There is no `json` argument
or policy-building data source.

The provider validates known source at plan time, showing errors with line
and column and warnings about ways around restrictions. If validation is
unavailable, plan warns and apply validates again. A call to a tool no rule
allows is refused. A browser restriction needs to refuse desktop control
and shell access too; see [The containment model](/explanation/containment).

Apply waits for `ready` by default. An invalid policy fails apply; the
previous valid policy stays in force. `wait_for_ready = false` returns
without waiting for loading to complete, so a successful apply does not
prove that the new rules are active. Changing only readiness settings or
timeouts sends no policy update to the API.

Older sessions with policy state `unsupported` cannot be given a policy.
Create a replacement session and move needed work first; recreation does
not carry the old disk or logins over.

### Ownership, deletion and import

If someone chooses **Manage here instead** in the app, the next plan shows
management drift; applying takes the policy back into `iac` mode. Keep one
Terraform resource responsible for each session's policy.

Destroying the policy resource normally resets that session to unrestricted
and returns it to `editor` mode. It does not delete the session. If the
policy is already in `editor` mode, destruction leaves it unchanged with a
warning. Review a plan that removes a policy: the session may become
unrestricted.

Import by the policy's session ID:

```sh
terraform import session_policy.research s-ab2cd
terraform plan
```

Define `session_id`, `rego` and `managed_url` first. Import reads the existing
policy; if it was managed in the editor, the first apply takes it into
managed-as-code mode. Importing a policy does not import its session.

## Data source: session

Finds one session without managing its lifecycle. It needs `sessions:read`.
Exactly one of `id` or `name` is required. Name lookup must match exactly one
of the token owner's sessions; a missing or ambiguous match is an error.

| Argument | Type | Meaning |
| --- | --- | --- |
| `id` | String | Existing session ID; mutually exclusive with `name` |
| `name` | String | Existing session name; mutually exclusive with `id` |

It returns `id`, `name`, `mcp_url`, `owner`, `state`, `size` and `pending_size`,
all strings. When resizing is pending, `size` reports the requested size.
This lookup does not wait for the desktop to run or its policy to load.

### Lookup by ID or name

With `providers.tf`, save as `main.tf`. Replace the sample ID and name with
sessions in your account. These are two separate lookups; they can name
different sessions.

```hcl
data "session" "by_id" {
  provider = browserjs

  id = "s-ab2cd"
}

data "session" "by_name" {
  provider = browserjs

  name = "research"
}

output "session_by_id" {
  value = data.session.by_id
}

output "session_by_name" {
  value = data.session.by_name
}
```

### Manage only an existing session's policy

With `providers.tf`, this complete `main.tf` manages the policy of a session
created in the app. Replace `research` with its name and set your repository
URL. Removing this configuration's policy resets it; the session is not
owned by Terraform and is not deleted.

```hcl
data "session" "existing" {
  provider = browserjs

  name = "research"
}

resource "session_policy" "existing" {
  provider = browserjs

  session_id  = data.session.existing.id
  managed_url = "https://github.com/example/infra/tree/main/desktops"

  rego = <<-EOT
    package browserjs.policy

    import rego.v1

    allow_tool_call if {
        input.server == "browser"
        input.tool == "browser_execute"
    }
  EOT
}

output "existing_mcp_url" {
  value      = data.session.existing.mcp_url
  depends_on = [session_policy.existing]
}
```

This policy allows all browser operations, including scripting, and denies
desktop control and the shell. The token needs `sessions:read`,
`policies:read` and `policies:write` for this example; `sessions:write` is not
needed because the session is only read.

## Data source: sessions

Lists all sessions belonging to the token's owner. It needs `sessions:read`
and accepts no arguments or filters. It does not wait for session readiness.

With `providers.tf`, save this as `main.tf`:

```hcl
data "sessions" "all" {
  provider = browserjs
}

output "session_names" {
  value = [for session in data.sessions.all.sessions : session.name]
}

output "running_sessions" {
  value = {
    for session in data.sessions.all.sessions :
    session.id => session.mcp_url if session.state == "running"
  }
}
```

The only top-level attribute is `sessions`, a list in the API's order. Each
item contains these string attributes:

| Attribute | Meaning |
| --- | --- |
| `id` | Session ID |
| `name` | Session name |
| `mcp_url` | MCP URL |
| `owner` | Owning account |
| `state` | Current API session state |
| `size` | Requested size, including when a resize is pending |
| `pending_size` | Size awaiting the next start, or empty |

An account with no sessions returns an empty list. Use HCL comprehensions,
as above, to filter it. If the list must include resources created in the
same apply, add `depends_on` for those resources, as in the complete example.

## Reuse a policy across sessions

One policy resource belongs to one session. Reuse its source with `for_each`.
With `providers.tf`, save this as `main.tf`, and save the Rego from the
complete configuration as `policy.rego`:

```hcl
locals {
  desktops = {
    "worker-a" = "small"
    "worker-b" = "medium"
    "worker-c" = "large"
  }
}

resource "session" "worker" {
  provider = browserjs

  for_each = local.desktops
  name     = each.key
  size     = each.value

  lifecycle {
    prevent_destroy = true
  }
}

resource "session_policy" "worker" {
  provider = browserjs

  for_each    = session.worker
  session_id  = each.value.id
  managed_url = "https://github.com/example/infra/tree/main/desktops"
  rego        = file("${path.module}/policy.rego")
}

output "worker_mcp_urls" {
  value      = { for name, session in session.worker : name => session.mcp_url }
  depends_on = [session_policy.worker]
}
```

Each map key is a Terraform instance address. Keep keys stable when changing
names or sizes. The deployment must have capacity for the sizes requested.

## Reference and source

The resource registrations and schemas in the
[provider source](https://github.com/r33drichards/computer-use/tree/main/terraform-provider-browserjs/internal/provider)
are the inventory this page describes. The
[generated provider reference](https://github.com/r33drichards/computer-use/tree/main/terraform-provider-browserjs/docs)
contains the same argument schemas. For tool inputs, see
[Policy format](/reference/policy); for session states and operations, see
[Session lifecycle](/reference/lifecycle).
