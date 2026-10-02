# Session policies: design

Status: proposal, for review. No product code is written and nothing is
deployed. Open questions for the product owner are in the last section, each
with a recommended default.

## The request

> I want to be able to configure each sandbox with OPA policies with either
> IaC or in the browser with a VS Code editor. These options should be
> mutually exclusive, like how the Tailscale admin dash does it. When you
> select to manage policies with IaC, it asks for a link to send the user to
> edit or update policies. I want to support both OPA and JSON policies, like
> cua driver does in the trycua/cua project. It has a JSON reader that
> translates it to simplified OPA. Or you can use more powerful OPA. I will
> also need a Terraform provider for this repo to create both browser and
> policy resources for these browsers.

## Summary of the proposal

- A **policy** is a named document owned by a user, of kind `json` (a small
  declarative format) or `rego` (raw OPA). JSON is compiled to Rego by the
  backend when it is saved, so there is one engine at enforcement time.
- A session uses the policy it names, else its owner's default policy, else
  the built-in policy (today's behaviour).
- **Enforcement**: mcp-js already asks a "remote OPA" over HTTP for a decision
  when a policy source is an `http://` URL. The backend becomes that policy
  decision point (PDP). Nothing is pushed into a pod, so a policy change
  applies to the next call of a running, warm-adopted or restored session, and
  mcp-js needs no change.
- **Management mode** is one switch per user account: `editor` (edit in the
  UI with Monaco) or `iac` (the UI is read-only and shows a link to where the
  policies are managed; only API tokens may write).
- **API tokens** issued by the backend, on a separate host that Pomerium
  passes through unauthenticated and the backend verifies itself, are how a
  Terraform provider signs in.
- A **Terraform/OpenTofu provider** `browserjs` with `browserjs_session`,
  `browserjs_policy` and `browserjs_policy_management`.
- Version 1 governs **what an agent may ask the browser to do** (the
  operations of `browser_execute` and their parameters). Outbound `fetch`,
  module imports and which sites Chromium may visit are later phases, each
  with a prerequisite named below.

Two findings change the shape of the request and are worth reading first:

1. cua-driver's simple format is **YAML, not JSON, and it is not translated
   to Rego**: it is a second engine evaluated natively, next to a Rego engine
   (section 1.1). This design does what the request describes (JSON compiled
   to Rego) rather than what cua-driver does.
2. Policies on `browser_execute` can be **bypassed from inside the pod**
   today, because the browser MCP server on `127.0.0.1:8081` accepts any
   caller, including a page in the Chromium it drives (section 7.2). That
   needs a small fix before any policy is worth relying on.

---

## 1. Findings

Items that could not be confirmed from a primary source are marked
UNVERIFIED.

### 1.1 cua-driver's policy system (trycua/cua)

Source: `libs/cua-driver/rust/crates/cua-driver-core/src/policy.rs` and
`docs/content/docs/cua-driver/guides/permissions.mdx` on `main`; added by
"feat(driver): add YAML and Rego permission policies (#2235)", 2026-07-15.

- **Two engines, chosen by file extension**, not one translated into the
  other. `PolicyEngine::load` takes `.yaml`/`.yml` to `YamlPolicy`, and
  `.rego` or a directory of `.rego` files to `RegoPolicy` (regorus). There is
  no JSON reader and no YAML-to-Rego step anywhere in the file.
- **The simple (YAML) schema**, with `deny_unknown_fields` throughout:

  ```yaml
  allow:
    tools: [screenshot, click]        # allowed with any arguments
    rules:                            # allowed when every constraint passes
      - tool: type_text
        constraints:
          text: { max_length: 500, pattern: "^[\\x20-\\x7E\\n\\t]*$" }
      - tool: launch_app
        constraints:
          bundle_id: { allowed: ["com.apple.Safari"] }
  deny:
    tools: [shell_execute]            # checked first
  ```

  Constraint checks are `min`, `max`, `max_length`, `pattern` and `allowed`;
  a constraint with none of them is rejected at load. `allow` and `deny` also
  accept a flat list. Evaluation order: `deny.tools`, then `allow.tools`, then
  any matching `allow.rules` entry, else deny. A constrained argument that is
  missing denies.
- **Rego**: the rule `data.cua.policy.allow` must be a boolean; undefined
  denies. Input is `{"server": "cua-driver", "tool": "<name>", "arguments":
  {…}}`, with internal transport arguments stripped first.
- **Selection and loading**: `CUA_DRIVER_POLICY_FILE` (user) and
  `CUA_DRIVER_MANAGED_POLICY_FILE` (administrator ceiling). Both are loaded
  once into a `OnceLock`; "a running daemon cannot be reconfigured (restart
  it)". A configured path that is missing or invalid stops the daemon from
  starting. The two layers are **intersected**: each can only narrow.
- **What it governs**: every tool call, on every dispatch path
  (`authorize_tool_call`), and the tool roster (`is_tool_listable`: a denied
  tool is not advertised in `tools/list`; a conditionally allowed one is).
- **Provenance**: a SHA-256 over the policy file names and contents
  (`policy_sha256`).
- **Tests**: unit tests in `policy.rs` (deny by default, deny overrides allow,
  constraints, layer intersection, hash stability, Rego directory loading,
  fail closed on a missing path), `tests/policy_tools_list_test.rs`,
  `tests/permission_policy_startup_test.rs`, `nix/cua-driver/tests/policy-yaml.nix`
  and `policy-rego.nix`.
- UNVERIFIED observation: `policy_tools_list_test.rs` writes a constraint
  `display_id: {"const": 0}`, and `const` is not a field of `RawConstraint`
  (which denies unknown fields). If that fails the load, the test returns
  early at `let Some(driver) = … else { return }` and passes without checking
  anything. Worth a look by whoever owns cua-driver; it does not affect this
  design.

`libs/fleet/backend/auth` does not translate a simple format either. It is a
Go policy AST (`policy_ast.go`: `Leaf`, `AllNode`, `AnyNode`, `BecauseNode`)
whose leaves are Rego queries over embedded `.rego` modules, with a `_test.rego`
beside each module. What is worth taking from it: denial reasons attached to
policy subtrees (`Because`), and golden and equivalence tests for policies.

**What this design takes from cua-driver**: the shape of the simple format
(allow list, constrained rules, deny first, deny by default), the five
constraint checks, an administrator layer that the owner's policy can only
narrow, fail closed on a broken policy, and a content hash for provenance.
**What it does differently**: the simple format is JSON and is compiled to
Rego, as the request asks, so there is one engine and the compiled Rego can be
shown to the user.

### 1.2 Tailscale: policy file managed externally

Sources: <https://tailscale.com/kb/1306/gitops-acls-github>, the changelog
entry of 2025-06-03, the API's OpenAPI schema
(<https://api.tailscale.com/api/v2?outputOpenapiSchema=true>), and
`tailscale/terraform-provider-tailscale`.

- **A manual setting**, on the "Policy file management" settings page: a
  toggle "Prevent edits in the admin console" and a field "External
  reference": "enter the URL of your repository. This lets other admins to
  know where the source of your policies is located." Nothing turns it on
  automatically, neither the GitHub Action nor Terraform.
- **It warns; it does not lock hard.** "Any admin with permissions to edit
  the tailnet policy file can still edit it directly by selecting Edit
  anyway… The next time you use the GitOps flow, it overwrites the tailnet
  policy file changes made in the admin console." The exact banner text on
  the locked page is UNVERIFIED (only the dialog was readable).
- **Turning it off**: "disable the Prevent edits in the admin console toggle.
  Optionally, delete the External reference link and select Save."
- **In the API** it is two tailnet settings: `aclsExternallyManagedOn`
  ("Prevents users from editing policies in the admin console to avoid
  conflicts with external management workflows like GitOps or Terraform")
  and `aclsExternalLink`.
- **Terraform**: `tailscale_acl` only writes the policy file. The lock is a
  different resource, `tailscale_tailnet_settings`
  (`acls_externally_managed_on`, `acls_external_link`). `tailscale_acl`
  "will completely overwrite existing policy file contents"; create sends
  `If-Match: ts-default` unless `overwrite_existing_content` is set, so it
  refuses to clobber a policy it did not import; `reset_acl_on_destroy`
  restores the default. The policy is validated against the API during plan.
- **Collisions**: `POST /tailnet/{tailnet}/acl` honours `If-Match` with the
  current ETag and answers 412 on mismatch. `/acl/validate` checks a policy
  without saving it.

The request says "mutually exclusive", which is stricter than Tailscale's
"Edit anyway". Open question 2 covers it.

### 1.3 mcp-js (v0.21.0-rc.3): what its policies can govern, and how

Sources: `server/src/engine/opa.rs`, `server/src/engine/hooks.rs`,
`site-docs/concepts/policies.md` at tag `v0.21.0-rc.3`; this repo's
`images/mcp-js/Dockerfile`.

- **Categories** (`PoliciesConfig`): `fetch`, `websocket`, `http2`,
  `modules`, `filesystem`, `fs_snapshot`, `mcp_tools`, `subprocess`,
  `run_js_file`. "Absent a policy for a category, the capability is
  unavailable": the JS global is not injected at all.
- **Input documents** (from the concepts page):

  | Category | Input |
  |---|---|
  | `mcp_tools` | `{"operation": "mcp_call_tool", "server", "tool", "arguments"}` |
  | `fetch` | `{"operation": "fetch", "url", "method", "headers", "url_parsed": {scheme, host, port, path, query}}` |
  | `filesystem` | `{"operation": "readFile"…, "path", "destination"?, "encoding"?, "mcp_headers"}` |
  | `subprocess` | `{"operation": "command_output" \| "exec", "command", "args", "cwd", "env"}` |
  | `modules` | `{"specifier", "specifier_type": "npm" \| "jsr" \| "url", "resolved_url", "url_parsed"}` |

- **How policies are supplied**: `--policies-json` /
  `MCP_V8_POLICIES_JSON`, per category a list of sources and a mode (`all`,
  the default, or `any`). A source is `file://` (a `.rego` file or directory,
  evaluated in-process by regorus; default rule `data.mcp.<category>.allow`)
  or `http(s)://` (a remote OPA: `POST {url}/v1/data/{policy_path}` with
  `{"input": …}`, reading `result.allow`; default path `mcp/<category>`,
  `mcp/tools` for `mcp_tools`; 5 s timeout).
- **Reloading**: none for local files. "The chain is built once at startup";
  for `file://`, "Policy changes require a server restart." For a remote
  source, "Policies can be updated without restarting mcp-v8", at the price
  of "a network round-trip to every capability call".
- **Failure**: "a network failure or a 5-second timeout is treated as a
  policy error, not a permit. The call is denied."
- **Hooks**: `pre` and `post` hooks generalise policies; a remote hook may
  answer `{"allow": false, "reason": "…"}` and may rewrite the input for
  `fetch`, `subprocess` and `mcp_tools`. Policies run as the last pre hook.
- **What a session ships today** (`images/mcp-js/`): two local files.
  `mcp_tools.rego` allows exactly `browser` / `browser_execute`;
  `filesystem.rego` allows only `/data/memory`. No `fetch`, `subprocess` or
  `modules` source, so those capabilities do not exist in a session.
  Heap persistence is off (`MCP_V8_HEAP_STORE=none`); artifacts and the
  mcp-js session database are on the session disk (`/data/mcp`).
- **What restarting only the mcp-js container would lose**: nothing on disk.
  It would drop the MCP client's connection and any `run_js` in flight, and
  Kubernetes cannot restart one container of a pod on request; it would have
  to be killed from inside. This design does not restart anything.
- **Not governed by any mcp-js category**: the top-level MCP tools the client
  calls (`run_js`, artifact tools), and anything Chromium does on its own.

**Navigation inside the browser.** The `mcp_tools` input carries the whole
`browser_execute` call, so a policy can read `arguments.operations[]`
(`navigate`, `evaluate`, `click`, `type`, … from
`images/browser/browser/server.js`). That constrains what the agent *asks
for*. It does not constrain where the browser ends up: a click on a link, a
redirect, or `evaluate` running `location.href = …` all navigate without a
`navigate` operation, and a human at the VNC view is not an MCP caller at
all. Binding Chromium itself has three possible places:

| Where | Binds the human too | Granularity | Change to a running pod | Cost |
|---|---|---|---|---|
| Browser MCP server intercepting requests over CDP on every tab | yes | URL | yes (it asks the PDP per navigation) | code in `server.js`; it must attach to tabs it did not open |
| Chromium enterprise policy (`URLAllowlist` / `URLBlocklist` in a managed policy file) | yes | URL patterns | UNVERIFIED: Chromium on Linux watches the policy directory and reloads; needs a spike | a writable policy directory and something to write the file |
| Egress proxy or FQDN network policy | yes, and mcp-js `fetch` | host only (TLS) | yes | a proxy per pod or shared; the heaviest |

`NetworkPolicy` alone cannot do it: it is by IP, and the existing one already
allows "the internet, but not the cluster".

### 1.4 An editor in the React app

- **Monaco** (`monaco-editor` + `@monaco-editor/react`) is the editor inside
  VS Code, which is what the request names. By default the React wrapper
  downloads Monaco from a CDN; since v4.4.0 it can use the npm package
  (`loader.config({ monaco })`), and with Vite the workers are imported with
  `?worker` and returned from `self.MonacoEnvironment.getWorker`
  (<https://github.com/suren-atoyan/monaco-react#use-monaco-editor-as-an-npm-package>).
- **Size**: `monaco-editor` 0.52.2 with everything is about 3.4 MB minified,
  852 KB gzipped, plus 135 KB of CSS and an 80 KB icon font (bundlephobia).
  Trimming languages under Vite is UNVERIFIED (documented only for the
  webpack plugin). Loaded as a lazy route chunk, only the policy editor page
  pays for it.
- **Rego highlighting**: Monaco has no built-in Rego language, and Shiki has
  no Rego grammar. Options: a Monarch grammar written here (Rego's lexical
  grammar is small); the TextMate grammar from `open-policy-agent/vscode-opa`
  (Apache-2.0) through `monaco-textmate`, which adds an Oniguruma WASM; or
  copying `pluralsh/console`'s `registerRegoLanguage.ts`, whose licence shows
  as NOASSERTION.
- **JSON validation**: `monaco.languages.json.jsonDefaults.setDiagnosticsOptions`
  with an inline schema gives completion and squiggles with no server call.
- **Server diagnostics**: `monaco.editor.setModelMarkers(model, owner,
  markers)`. OPA errors carry `Location{Row, Col}` and a code
  (`rego_parse_error`, `rego_compile_error`, `rego_type_error`); the end of
  the range has to be synthesised.
- **Go OPA library** (`github.com/open-policy-agent/opa/v1`):
  `ast.ParseModuleWithOpts`, `ast.CompileModulesWithOpt`,
  `(*Compiler).WithCapabilities`, `rego.Capabilities`, and `Eval(ctx)` which
  honours the context deadline. Removing `http.send`, `net.lookup_ip_addr`
  and `opa.runtime` from `ast.CapabilitiesForThisVersion().Builtins` stops a
  user's Rego from reaching the network; `AllowNet` does not ("this only
  controls fetching remote refs for using JSON Schemas in the type checker").
- **CSP**: the backend sets none today (`backend/cmd/server/web.go`), so
  nothing breaks. If one is added later, Monaco needs workers from `'self'`
  (and `blob:` unless workers are bundled as files) and uses inline styles
  (<https://github.com/microsoft/monaco-editor/issues/4927>); exact
  directives UNVERIFIED.
- **Alternative**: CodeMirror 6 is about 119 KB gzipped, with
  `codemirror-json-schema` for the JSON form, but its only Rego package
  (`codemirror-lang-rego` 0.1.0) is a single-author package from December
  2025. Styra's own Rego mode is for CodeMirror 5.
- **Regal** (the Rego linter) speaks LSP over stdio; a WebSocket bridge
  exists as a demo and needs a server-side process. Not proposed for v1.

### 1.5 A Terraform provider, and how it signs in

Everything today is behind Pomerium v0.33 with a browser sign-in (cookie) or
the MCP OAuth flow. Options for a non-interactive client:

| Option | Finding | Verdict |
|---|---|---|
| Pomerium service accounts | "Service Accounts are a Pomerium Enterprise and Pomerium Zero feature." | not available in core |
| Programmatic access (`/.pomerium/api/v1/login`) | Needs a browser once; returns an opaque `pomerium_jwt` tied to the user's session; "anticipate the possibility that your underlying `refresh_token` may stop working". Lifetime UNVERIFIED. | fine for a CLI on a laptop, wrong for CI |
| MCP OAuth tokens | Only `authorization_code` and `refresh_token` grants; tokens are honoured only on MCP routes (`authorize/grpc.go`). | no |
| `bearer_token_format: idp_access_token` with Dex | Pomerium accepts a Dex token as a bearer. Dex has a device flow (interactive once), and `client_credentials` behind a feature flag, which identifies a client, not a user. Tokens are short-lived. | possible, awkward, ties CI to Dex internals |
| **Backend-issued API tokens on a route with `allow_public_unauthenticated_access`** | The pattern this repo already uses for uploads and VNC ("the token in the path is the credential"). The backend verifies the token itself. | **recommended** |

Pomerium detail that matters for the recommended option: when
`pass_identity_headers` is off, Envoy strips a client-supplied
`X-Pomerium-Jwt-Assertion` (`config/envoyconfig/routes.go`), and the backend
verifies the assertion's signature and audience anyway, so identity cannot be
forged on the public route. Whether Pomerium mints an assertion for an
anonymous request on a public route is UNVERIFIED and does not matter: the
token route does not read it.

**Publishing rules.** The Terraform Registry requires a public GitHub repo
named `terraform-provider-{NAME}`, signed releases, and assets named
`terraform-provider-{NAME}_{VERSION}_{OS}_{ARCH}.zip` with `SHA256SUMS`, a
binary GPG `.sig` and a `terraform-registry-manifest.json`
(<https://developer.hashicorp.com/terraform/registry/providers/publishing>).
The OpenTofu Registry asks for the same repo pattern, submitted through an
issue form (<https://github.com/opentofu/registry>). Neither documents
publishing from a subdirectory of another repo (UNVERIFIED as an explicit
statement; the repo-name rule implies no). Without a registry: `dev_overrides`
for development, `filesystem_mirror` or a static-site `network_mirror` for
users, and for OpenTofu 1.10+ an OCI mirror (`oci_mirror`, mirror only, not a
primary source).

---

## 2. Data model

### 2.1 Policy

| Field | Meaning |
|---|---|
| `id` | `p-` + ten base32 characters, like a session ID |
| `name` | unique per owner; what the UI and Terraform show |
| `owner` | the user's email address, as for sessions |
| `kind` | `json` or `rego` |
| `source` | what the author wrote, at most 64 KiB |
| `rego` | what is enforced: `source` for `rego`, the compiled module for `json` (read-only) |
| `version` | counts saves, starting at 1 |
| `sha256` | of `rego`; shown in the UI and in decision logs |
| `updated`, `updated_by` | when, and `ui` or the token's name |

A policy is validated when it is saved (section 3.3); one that does not
compile cannot be stored. At most 20 policies a user.

### 2.2 How a policy attaches to a session

Two shapes, both laid out:

- **Per session**: a session has an optional `policy_id`. Explicit, visible on
  the session, natural as a Terraform attribute. A user with five sessions
  sets it five times, and a session created by an MCP client or by "Create"
  with no thought given has none.
- **Per-user default**: the account names one policy that applies to its
  sessions. Nothing to remember, but one policy for everything.

**Recommended: both, as one rule.** A session's effective policy is the first
of: its own `policy_id`; its owner's `default_policy_id`; the built-in
policy. It is resolved at each decision, not copied at creation, so changing
the default changes every session that has no policy of its own at once. The
built-in policy is exactly today's behaviour (any `browser_execute`
operation), so nothing changes for a user who never opens the policy pages.

The API shows which rule applied: `effective_policy: {id, name, version,
from: "session" | "default" | "builtin"}`.

Deleting a policy that a session or the default still names is refused (409)
and the answer lists the users of it.

### 2.3 Management mode

One setting **per user account**, as Tailscale's is per tailnet:

```json
{ "mode": "editor" | "iac", "managed_url": "https://github.com/me/infra/tree/main/browserjs" }
```

`managed_url` is required in `iac` mode, must be `https`, and is kept (but
not shown) when the mode goes back to `editor`.

Per policy was considered and rejected: "this one is Terraform's, that one is
mine" is a state users lose track of, the default-policy pointer would need a
mode of its own, and it is not what Tailscale does.

What each credential may do:

| | `editor` mode, UI (cookie) | `editor` mode, API token | `iac` mode, UI (cookie) | `iac` mode, API token |
|---|---|---|---|---|
| Read policies, validate, test | yes | yes | yes | yes |
| Create, edit, delete a policy | yes | **no (409)** | **no (409)** | yes |
| Set the default policy | yes | no | no | yes |
| Set a session's policy | yes | yes | yes | yes |
| Change the mode and `managed_url` | yes | yes | yes | yes |
| Create and revoke API tokens | yes | no | yes | no |

- A refused write answers `409 {"error": "policies are managed externally",
  "managed_url": …}` (or `"…managed in the editor"` for a token in `editor`
  mode), so the Terraform provider can say what to do.
- Picking which existing policy a session uses stays open in both modes: it
  is attachment, not authorship, and sessions are created ad hoc in the UI.
- The mode can always be switched from either side. That is the escape
  hatch; there is no "Edit anyway" (open question 2).
- An MCP client (the agent) can do none of this: the MCP route reaches only
  a session's `/mcp`, never the API.

### 2.4 Where it is stored

**Recommended: ConfigMaps in a namespace of their own**, `browserjs-policies`,
with a Role there for the backend's ServiceAccount (`configmaps`: get, list,
create, update, delete). The backend stays stateless, as today.

| Object | Holds |
|---|---|
| `policy-<id>` | label: owner hash (as `browserjs.dev/owner` on Sandboxes); annotations: owner, name, kind, version; data: `source`, `rego` |
| `account-<owner hash>` | `mode`, `managed_url`, `default_policy_id` |
| `token-<id>` | owner, name, scopes, expiry, the token's SHA-256 (never the token) |

A session's `policy_id` is an annotation on its Sandbox,
`browserjs.dev/policy`, beside the owner and name it already has.
`resourceVersion` is the ETag for `If-Match` on a policy save.

Why a separate namespace: RBAC cannot select by label, and the sessions
namespace also holds Pomerium's and Dex's ConfigMaps. A Role on `configmaps`
there would let the backend rewrite Pomerium's routes.

Alternatives and what they cost:

| Alternative | Cost |
|---|---|
| A CRD (`policies.browserjs.dev`) | typed and `kubectl get`-friendly, RBAC by resource so no extra namespace; but a cluster-scoped object to install and version, and the admission policies of the GKE add-on to check. A reasonable second step. |
| Inline on the Sandbox (annotation) | no new objects; but no sharing between sessions, no default, a 256 KiB limit shared with everything else, and the policy disappears with the session |
| A database (Postgres) | history, audit and queries for free; but the first stateful dependency of the backend: backups, migrations, a secret, another thing to be down |

Revision history is not kept in v1: in `iac` mode git has it, and in `editor`
mode the last save wins with an ETag check (open question 7).

---

## 3. The JSON policy format

Modelled on cua-driver's simple format (section 1.1): an allow list, rules
with constraints, a deny list checked first, deny by default. Adapted because
a session has one upstream tool, `browser_execute`, whose argument is a
pipeline of operations: the unit of the policy is the **operation**, not the
tool.

### 3.1 Schema (version 1)

```json
{
  "version": 1,
  "description": "optional, shown in the UI",
  "allow": {
    "operations": ["<operation>", …],
    "rules": [
      { "operation": "<operation>", "constraints": { "<param>": { …checks… } } }
    ]
  },
  "deny": { "operations": ["<operation>", …] }
}
```

- Operations are those of `browser_execute`: `setViewport`, `navigate`,
  `setContent`, `wait`, `screenshot`, `evaluate`, `click`, `type`, `press`,
  `select`, `url`. `"*"` in `allow.operations` means all of them.
- For one operation: denied if in `deny.operations`; else allowed if in
  `allow.operations`; else allowed if any rule for it passes; else denied.
- A call is allowed only if **every** operation in its pipeline is. A call
  with a denied operation runs nothing.
- Checks on a parameter, all of which must pass: cua-driver's five (`min`,
  `max`, `max_length`, `pattern` in RE2 syntax, `allowed`), and two for URL
  parameters, `hosts` (exact names, or `*.example.com` for subdomains) and
  `schemes`. A constrained parameter that is absent fails.
- Unknown keys are errors. A JSON Schema for this format is served by the
  backend and is the single source for the editor's completion, the API's
  validation and the provider's plan-time check.

Reserved for later phases, rejected in v1 with a clear message: `fetch`,
`modules`, `navigation` (section 4.5).

### 3.2 Examples

Look but do not script: no `evaluate`, no `setContent`.

```json
{
  "version": 1,
  "allow": { "operations": ["*"] },
  "deny": { "operations": ["evaluate", "setContent"] }
}
```

One site, short text only:

```json
{
  "version": 1,
  "allow": {
    "operations": ["click", "press", "select", "wait", "screenshot", "url", "setViewport"],
    "rules": [
      { "operation": "navigate",
        "constraints": { "url": { "schemes": ["https"], "hosts": ["example.com", "*.example.com"] } } },
      { "operation": "type",
        "constraints": { "text": { "max_length": 500 } } }
    ]
  }
}
```

The second example allows `click`, so a click on a link can leave
`example.com`. The validator says so as a warning: "`navigate` is limited to
hosts, but `click` is allowed: this limits what the agent asks for, not where
the browser can go" (section 4.5).

### 3.3 Translation to Rego

The backend compiles JSON to one Rego module on save, stores both, and
enforces only the Rego. The contract for any policy, generated or written by
hand:

- package `browserjs.policy`;
- `allow_tool_call` decides an upstream MCP tool call; undefined or false
  denies. (`allow_fetch`, `allow_module` and `allow_navigation` are the names
  reserved for later phases.)
- optional `deny_reason`, a string, recorded in the decision log;
- `input` is mcp-js's document for the category (section 1.3) with two
  additions made by the PDP: `input.session` (`id`, `name`, `owner`), and for
  every operation with a `url` parameter, `url_parsed` (`scheme`, `host`,
  `port`, `path`, `query`) as parsed by Go's `net/url`. Rego has no URL
  parser, and a regex over a URL is how `https://example.com@evil.test`
  gets through. A URL that does not parse, or parses to something other than
  `http`, `https` or `about:blank`, is denied before the policy runs.

The second example compiles to (illustrative; the implementation's golden
tests fix the exact text):

```rego
# Generated from JSON policy "one-site" v3. Edit the JSON, not this.
package browserjs.policy

import rego.v1

allow_tool_call if {
	input.server == "browser"
	input.tool == "browser_execute"
	every op in input.arguments.operations {
		operation_allowed(op)
	}
}

operation_allowed(op) if op.type in {"click", "press", "select", "wait", "screenshot", "url", "setViewport"}

operation_allowed(op) if {
	op.type == "navigate"
	op.url_parsed.scheme in {"https"}
	host_allowed_0(op.url_parsed.host)
}

host_allowed_0(h) if h in {"example.com"}

host_allowed_0(h) if endswith(h, ".example.com")

operation_allowed(op) if {
	op.type == "type"
	is_string(op.params.text)
	count(op.params.text) <= 500
}
```

The translator is a pure function with table-driven tests: for each example,
the generated Rego (golden file) and a set of inputs whose decisions are
asserted, so the JSON semantics in 3.1 and the Rego cannot drift. The UI
shows the generated Rego read-only, and offers "Convert to Rego", one way:
the policy's kind becomes `rego` and its source the generated module.

Validation of any policy, before it is saved:

1. `json`: schema check, then compile to Rego.
2. Parse and compile with the Go OPA library, with restricted capabilities
   (no `http.send`, `net.lookup_ip_addr`, `opa.runtime`).
3. The package must be `browserjs.policy`, and `allow_tool_call` must exist
   and type-check as a boolean.
4. Evaluate against a fixed set of sample inputs with a deadline, to catch a
   policy that errors or does not terminate quickly.

Errors come back with row and column; warnings (like the `click` one above)
do not block a save.

---

## 4. Enforcement

### 4.1 Options

Per-session policy must reach a pod that is usually already running (warm
pool), and must still be right after a sleep and a restore from a Pod
Snapshot, where the process comes back with whatever it had in memory.

| | A. Backend is the PDP (remote source) | B. Push Rego into the pod; mcp-js reloads | C. OPA sidecar in the pod |
|---|---|---|---|
| mcp-js change | none | a reload endpoint or file watch (new feature, new release) | none |
| Pod change | one env value | a writable policy directory, or the endpoint | a third container (memory per session; the node fits 9) |
| Running or warm pod | applies at the next call | backend must push at adoption, on every change, to every running session | same as B |
| After a snapshot restore | nothing to do | restored memory holds whatever was pushed before; push again on every wake, and close the window before it lands | same as B |
| Engine | Go OPA, the same one that validated | regorus in the pod, validated by OPA in the backend: two dialects | OPA |
| Whose CPU runs the owner's Rego | the backend's | the owner's pod | the owner's pod |
| Latency | one in-cluster round trip per governed call | none | loopback |
| New network path | session pod to backend, one port | backend to pod (exists) | backend to pod, one more port |

**Recommended: A.** It is the only one that needs nothing from mcp-js, has no
push to get wrong, and has no "which version does this pod have" question.
Its costs are acceptable for v1:

- The round trip is paid once per `browser_execute` call, which already
  takes tens to hundreds of milliseconds in the browser.
- Every MCP call already goes through the backend, so the backend being down
  already means no MCP. A `run_js` in flight while the backend restarts is
  the one new failure: its next `mcp.callTool` is denied.
- The owner's Rego runs in the backend. Section 7.4 bounds it. If that or
  latency becomes a problem, B is the next step and the data model does not
  change.

### 4.2 How it works

```
MCP client ── Pomerium ── backend ── pod: mcp-js ── run_js ── mcp.callTool("browser", "browser_execute", …)
                             ▲                │
                             │   POST /s/<session id>/v1/data/mcp/tools   {"input": {…}}
                             └────────────────┘   → {"result": {"allow": true}}
```

1. **Pod template** (blueprint and warm-pool template): `MCP_V8_POLICIES_JSON`
   moves from the image to the pod's env, so the URL can carry the session
   ID, which is the pod's name for warm pods and `{{ .ID }}` for cold ones
   (the trick `MCP_V8_PUBLIC_URL` already uses):

   ```json
   {
     "mcp_tools":  { "mode": "all", "policies": [
       { "url": "file:///etc/mcp/mcp_tools.rego" },
       { "url": "http://backend-pdp.browserjs-sessions.svc:8090/s/$(SESSION_ID)" } ] },
     "filesystem": { "policies": [ { "url": "file:///etc/mcp/filesystem.rego" } ] }
   }
   ```

   The local file stays first: it is the platform's layer (only `browser` /
   `browser_execute`), evaluated in-process, and the owner's policy can only
   narrow it, as cua-driver's managed layer does. `filesystem` is unchanged
   and not owner-configurable.

2. **Backend**: a second listener, `:8090`, serving only
   `POST /s/{id}/v1/data/mcp/{category}`. For each request it:
   - finds the Sandbox `{id}` and requires the request's source address to
     be that Sandbox's pod IP (else deny). A pod can therefore only ask about
     itself, and a stale IP cannot be given another session's policy;
   - requires the Sandbox to have an owner (a warm pod waiting in the pool
     has none: deny);
   - resolves the effective policy (section 2.2), compiles it once per
     `(id, version)` and caches the prepared query;
   - enriches the input (section 3.3), evaluates with a deadline, answers
     `{"result": {"allow": <bool>}}`;
   - records the decision (session, policy id and version, input summary,
     result, `deny_reason`, duration) in a per-session ring buffer and the
     log.

3. **NetworkPolicy**: session pods get one more egress rule, to pods
   `app: backend` on TCP 8090; the backend gets one more ingress rule, from
   pods `app: browserjs-session` on 8090. The main port stays Pomerium-only.

A policy edit, a change of default, a change of a session's `policy_id`, a
warm adoption, a wake from a snapshot: none of them needs any action towards
the pod. The next decision reads the current state.

### 4.3 Failure behaviour: closed

| Situation | Result |
|---|---|
| PDP unreachable, slow (> 5 s) or erroring | mcp-js denies ("treated as a policy error, not a permit") |
| Session names a policy that no longer exists | deny; the session shows "policy missing" |
| Stored policy no longer compiles (an OPA upgrade) | deny; the policy shows the error; found at startup by recompiling all |
| Evaluation exceeds its deadline or errors | deny, logged with the reason |
| Request from an address that is not the session's pod | deny |
| Pod with no owner | deny |

A denied call surfaces to the agent as a failed `mcp.callTool` inside
`run_js`. In v1 the message is mcp-js's generic denial; the reason is in the
session's decision log in the UI. Phase 2 can return the reason to the agent
by configuring the PDP as a `pre` hook, whose answer may carry `reason`
(section 1.3; path `mcp/tools/pre`).

### 4.4 Sessions that exist before this ships

A Sandbox's pod template is fixed when it is created, and a snapshot restores
the old process. Sessions created before the rollout keep the static policy
of their image and never call the PDP. The API marks them
`policy_enforced: false` (their template has no PDP URL), the UI says "created
before policies; recreate to apply one", and assigning a policy to one is
refused. The warm pool replaces its waiting pods on a template change
(`updateStrategy: Recreate`), so new sessions are covered from the deploy on.

### 4.5 What is not enforceable without further work

- **Where the browser goes.** A `hosts` constraint on `navigate` limits the
  agent's requests only. Links, redirects, `evaluate`, and the human at the
  VNC view are not bound. Phase 3 enforces navigation in the browser MCP
  server over CDP (the first row of the table in 1.3), asking the same PDP
  (`allow_navigation`), which binds the human too. Until then the validator
  warns, and the UI labels host constraints "what the agent may ask for".
- **Outbound `fetch`, WebSocket and module imports from `run_js`.** Off
  today, and they stay off in v1. They can become owner-configurable in
  phase 2 by adding the PDP as their source, with a platform layer in front
  that denies loopback and private hosts. Prerequisite: section 7.2, because
  `fetch("http://127.0.0.1:8081/mcp")` would otherwise walk around the
  `mcp_tools` policy, and a public name that resolves to `127.0.0.1` defeats
  a host check.
- **Subprocess, filesystem outside `/data/memory`, `run_js_file`.** Platform
  decisions, not offered to owners.
- **The top-level MCP tools** (`run_js` itself, artifacts). Not an mcp-js
  policy category. The backend proxies `/mcp` and could gate `tools/call` by
  name there; not proposed now.
- **What a page does once loaded** (its own requests, downloads, clipboard).

---

## 5. UI sketch

Wireframe style, as the rest of the app. A second item in the header:
`browserjs sessions · policies`.

### 5.1 Policies (`/policies`)

```
Policies                                              [ Create policy ]
Managed in: (•) this editor   ( ) infrastructure as code      [ Settings ]

  Name              Kind   Version   Used by                Updated
  one-site          JSON   3         default, 2 sessions    2 h ago
  no-scripting      JSON   1         brave-otter            yesterday
  strict            Rego   7         -                      3 d ago

Default for new and unassigned sessions:  [ one-site            v ]
API tokens: 1 active                                   [ Manage tokens ]
```

### 5.2 In `iac` mode

```
+----------------------------------------------------------------------+
| Policies are managed as code.                                        |
| Edit them at  https://github.com/me/infra/tree/main/browserjs  [->]  |
| Changes made here would be overwritten.        [ Change in Settings ]|
+----------------------------------------------------------------------+
Policies                                              [ Create policy ] (disabled)
  Name              Kind   Version   Used by                Updated
  one-site          JSON   3         default, 2 sessions    by token "ci", 2 h ago
```

Read-only in this mode: the editor (a "Read-only: managed as code" strip in
place of Save), Create, Delete, Convert to Rego, and the default picker.
Still usable: viewing, Validate and Test (nothing is saved), a session's
policy picker, tokens, and Settings.

### 5.3 Settings (a modal from either state)

```
How are policies managed?
  (•) In this editor
  ( ) Infrastructure as code (Terraform, OpenTofu, the API)
        Link to where they are managed (required)
        [ https://github.com/me/infra/tree/main/browserjs            ]
        The editor becomes read-only and shows this link.
                                                   [ Cancel ] [ Save ]
```

### 5.4 Editor (`/policies/:id`)

```
< Policies / one-site                    v3 · sha256 9f2c…   [ Delete ]
Kind: [ JSON | Rego ]                    [ Validate ] [ Save ]   saved 2 h ago

+-- policy.json (Monaco) -------------------+-- Generated Rego (read-only) --+
| {                                         | package browserjs.policy       |
|   "version": 1,                           | import rego.v1                 |
|   "allow": {                              | allow_tool_call if {           |
|     "operations": ["click", …],           |   …                            |
|     "rules": [ … ]                        | }                              |
| ~~~~~~~~ unknown operation "clik"         |                                |
+-------------------------------------------+--------------------------------+
Problems (1)   12:9  unknown operation "clik"; did you mean "click"?
Warnings (1)   navigate is limited to hosts, but click is allowed: …

Test
  Input: [ navigate to a URL v ]  [ from this session's recent calls v ]
  +-- input.json ---------------------------+  Result
  | { "server": "browser", "tool": …        |  DENY  operation 0 (navigate):
  |   "arguments": { "operations": [ …      |        host "evil.test" not allowed
  +-----------------------------------------+  [ Run ]
```

- The `JSON | Rego` toggle on a new policy picks the kind. On an existing JSON
  policy, Rego shows the generated module read-only with "Convert to Rego
  (cannot be undone)".
- In Rego kind there is one pane; diagnostics come from the server
  (debounced `validate`), JSON diagnostics also from the schema in the
  browser.
- Save sends `If-Match`; on 412: "This policy changed since you opened it"
  with Reload.

### 5.5 On a session

Session detail gains a row and a panel:

```
Policy:  [ one-site (default)   v ]    v3 · enforced
Recent decisions                                         [ Open policy ]
  12:01:07  allow  browser_execute  navigate, click, screenshot
  12:01:31  DENY   browser_execute  evaluate          [ Use as test input ]
```

The create dialog gains the same picker, preselected "Default".

---

## 6. API

### 6.1 Additions

The same handlers serve the UI (cookie, on the app's host under `/api`) and
tokens (on the API host under `/v1`); the table in 2.3 says who may write.

| Method and path | Purpose |
|---|---|
| `GET /policies`, `POST /policies` | list mine; create `{name, kind, source}` |
| `GET /policies/{id}`, `PUT`, `DELETE` | read (with `rego`, `version`, `sha256`, `used_by`); replace (`If-Match` optional); delete (409 while in use) |
| `POST /policies/validate` | `{kind, source}` → `{ok, rego?, errors[], warnings[]}`; saves nothing |
| `POST /policies/evaluate` | `{kind, source, input}` or `{policy_id, input}` → `{allow, deny_reason?}`; saves nothing |
| `GET /policy-schema.json` | the JSON Schema of the JSON format |
| `GET /policy-settings`, `PUT` | `{mode, managed_url, default_policy_id}` |
| `POST /sessions`, `PATCH /sessions/{id}` | accept `policy_id` (`null` clears it) |
| `GET /sessions/{id}` | adds `policy_id`, `effective_policy`, `policy_enforced` |
| `GET /sessions/{id}/decisions` | the recent decisions |
| `GET /tokens`, `POST /tokens`, `DELETE /tokens/{id}` | cookie only; the token is shown once, on creation |

Errors keep the existing shape, `{"error": "…"}`, plus `errors[]` with `row`,
`col`, `code`, `message` for validation. An OpenAPI document for all of it is
the first deliverable of the plan (section 9), so the UI and the provider are
built against a contract, not against each other.

### 6.2 API tokens

- **Form**: `bjs_<token id>_<43 characters of base64url randomness>`. The
  prefix makes it findable by secret scanners; the id lets the backend look
  up one object and compare a SHA-256 in constant time. Only the hash is
  stored.
- **Belongs to a user**, and acts as that user: it sees and manages that
  user's sessions and policies, nothing else. A token never has admin rights,
  even an admin's.
- **Scopes**: `sessions` and `policies`, each `read` or `write`. A token
  cannot create or revoke tokens.
- **Expiry is mandatory**: 90 days by default, at most a year. The UI shows
  last use, to the hour.
- **Route**: a host of its own, `api.<domain>`, to the backend with
  `allow_public_unauthenticated_access: true` and no identity headers. The
  backend already tells requests apart by host; on this host it accepts only
  `Authorization: Bearer bjs_…` and serves only `/v1/…`. No cookie is ever
  sent there, so there is no CSRF surface. Costs one DNS record and one
  certificate name.
- **Who may still use the product.** Pomerium's allow-list of email
  addresses is not consulted on this route. Without care, a user removed from
  it would keep API access until their tokens expire. The backend therefore
  needs the same list (`ALLOWED_EMAILS`, from the same kustomize value as
  Pomerium's policy) and checks the token's owner against it on every
  request; admins can list and revoke anyone's tokens (open question 6).
- Failed attempts are rate-limited per source address; the answer for a bad,
  expired or revoked token is the same 401.

---

## 7. Security review

### 7.1 What a policy is for

The author of a policy is the owner of the session, who can already do
anything with it. So a policy is not a boundary against its author. It
protects the owner's interests against two other parties:

- **The agent connected over MCP**, which the owner has delegated to and
  which may be wrong, over-eager, or steered by text it read on a page
  (prompt injection). The browser is logged in to the owner's accounts. The
  policy is the owner's statement of how far the delegation goes: no
  scripting, only this site, no typing of long text.
- **Web pages in the browser**, to the extent they act through the agent.
  A page cannot call MCP, but it can try to talk the agent into it; the
  policy bounds what a successfully injected agent can then do.

It follows that the agent must never be able to change, weaken or learn to
evade the policy through any channel the session offers:

- the management API is not reachable over MCP, and API tokens exist only
  where the owner puts them. A token pasted into a page, into `/data/memory`
  or into an agent's prompt gives the agent the policy. The token page says
  so;
- `run_js` has no `fetch` in v1, so code in the session cannot call the API
  host even with a token;
- the PDP answers only "allow or not" and only to the pod it is about.

And some things must stay outside the owner's control whatever they write:

- the platform layer: only the `browser` server and `browser_execute`,
  filesystem only under `/data/memory`, no subprocess; later, no loopback or
  private addresses for `fetch`. The owner's policy is intersected with it
  and can only narrow;
- isolation between sessions and between users (NetworkPolicy, one host per
  session, gVisor): not expressible in a policy at all;
- resource limits on policies: size, count, evaluation time;
- who may sign in, and the admin list.

### 7.2 A hole to close first: callers of the browser MCP server

`images/browser/browser/server.js` listens on `:8081` and serves any caller.
From outside the pod only the backend can reach the pod, and only on 6080 and
8080. But inside the pod everything shares loopback, including Chromium. An
agent that is allowed `navigate` and `evaluate` can open
`http://127.0.0.1:8081/healthz` and, from that origin, `fetch("/mcp", …)` a
`browser_execute` call that no policy sees. The same is true of mcp-js on
`:8080`, which checks no token (those calls are still policy-checked), and a
hostile page may try the same against either port without the agent's help
(whether Chromium's local-network protections stop that is UNVERIFIED).

Today this does not matter, because the policy allows everything anyway. With
owner policies it does: "everything except `screenshot`" is void if
`evaluate` is allowed. Fix, in the browser image, before or with phase 1:

- the browser MCP server refuses any request that carries an `Origin` or
  `Sec-Fetch-Site` header (browsers always send one on such a request; mcp-js
  does not), and binds to `127.0.0.1` rather than `::`;
- better, a shared secret that mcp-js sends and the server requires, if
  `MCP_V8_MCP_CONFIG` supports request headers (UNVERIFIED), generated per
  pod at start;
- the validator warns when a policy allows `evaluate` together with
  unconstrained `navigate` and denies anything else, until this is fixed.

### 7.3 The new network path

Session pods have never been able to reach the backend; now they can reach
one port. That port serves one method on one path shape, has no state to
change, identifies the caller by pod IP against the Sandbox's status (the
cluster's network does not rewrite pod-to-pod source addresses; to be
confirmed on GKE Dataplane V2 with gVisor pods in the phase 1 spike), and
rate-limits per pod. Anything in the pod, a web page included, can ask it
questions about that pod's own policy and get booleans back. That reveals the
policy by probing; it is the owner's own policy and is not treated as secret.

### 7.4 Running the owner's Rego in the backend

- Capabilities without `http.send`, `net.lookup_ip_addr`, `opa.runtime`: no
  network, no environment.
- A deadline on every evaluation (50 ms at the PDP, 1 s in validate and
  test), 64 KiB of source, 20 policies a user, a bound on input size (the MCP
  route already caps a request at 1 MiB).
- OPA has no memory limit of its own. A policy built to allocate could hurt
  the one backend replica, which is everyone's. Users are an allow-list, not
  the public, so this is accepted for v1 and is the main reason to keep
  option B (evaluation in the owner's pod) in view.
- Regular expressions are RE2 (linear time) in both the JSON format and
  Rego's `regex` builtins.

### 7.5 Other points

- **URL parsing differences.** The PDP parses with Go; Chromium parses with
  its own rules. The PDP denies URLs that are not plain `http(s)` with a
  host, or that change when re-serialised, so the two parsers are only
  compared on inputs where they agree. Unit tests carry the known
  confusions (userinfo, backslashes, whitespace, IDNs).
- **`managed_url` is shown as a link.** `https` only, rendered as text with
  `rel="noopener noreferrer"`; it is the user's own setting shown to
  themselves, and to admins.
- **Decision logs contain agent inputs** (URLs, typed text). The ring buffer
  is per session, in memory, visible to the owner and admins, and the log
  line carries a summary (operation types, hosts), not parameters.
- **Admins** can read every policy and decision, as they can already see
  every session. They do not get to edit other users' policies in v1.
- **Fail closed** everywhere (4.3), including the case most likely to be
  got wrong: a pod with no owner.

---

## 8. Terraform provider

### 8.1 Outline

- Go, `terraform-plugin-framework`, protocol 6; works with Terraform and
  OpenTofu. Provider type `browserjs`.
- Configuration: `endpoint` (default `https://api.browserjs.com`,
  `BROWSERJS_ENDPOINT`) and `token` (sensitive, `BROWSERJS_TOKEN`).
- Resources:
  - `browserjs_policy`: `name`, exactly one of `json` or `rego`; computed
    `id`, `version`, `sha256`, `compiled_rego`. Validated at plan time with
    `POST /policies/validate`, as `tailscale_acl` is. JSON is compared
    semantically, so reformatting is not a diff. Import by id or name.
  - `browserjs_session`: `name`, optional `policy_id`; computed `id`,
    `mcp_url`, `url`, `state`. Destroying one deletes its disk and the
    browser's logins; the docs recommend `prevent_destroy`. Import by id.
  - `browserjs_policy_management`: the account's singleton, `mode`,
    `managed_url`, `default_policy_id`. Destroying it returns the account to
    `editor`. Import with any id, as `tailscale_acl` does. Policy writes are
    refused in `editor` mode (2.3), so `browserjs_policy` resources should
    depend on it; the provider turns the 409 into "add a
    `browserjs_policy_management` resource with `mode = \"iac\"`".
- Data sources: `browserjs_policy` (by name), `browserjs_session`,
  `browserjs_sessions`, and `browserjs_policy_document`, which builds the
  JSON format from HCL blocks, in the manner of `aws_iam_policy_document`.
- Tests: unit tests against a fake API from the OpenAPI document; acceptance
  tests (`TF_ACC`) against the local kind deployment.

### 8.2 Example

```hcl
terraform {
  required_providers {
    browserjs = { source = "r33drichards/browserjs" }
  }
}

provider "browserjs" {
  # endpoint and token from BROWSERJS_ENDPOINT / BROWSERJS_TOKEN
}

resource "browserjs_policy_management" "this" {
  mode        = "iac"
  managed_url = "https://github.com/r33drichards/infra/tree/main/browserjs"

  default_policy_id = browserjs_policy.no_scripting.id
}

resource "browserjs_policy" "no_scripting" {
  name = "no-scripting"
  json = jsonencode({
    version = 1
    allow   = { operations = ["*"] }
    deny    = { operations = ["evaluate", "setContent"] }
  })
}

resource "browserjs_policy" "one_site" {
  name = "one-site"
  rego = file("${path.module}/one-site.rego")
}

resource "browserjs_session" "research" {
  name      = "research"
  policy_id = browserjs_policy.one_site.id

  lifecycle {
    prevent_destroy = true
  }
}

output "mcp_url" {
  value = browserjs_session.research.mcp_url
}
```

(`browserjs_policy_management` naming a policy while policies need the mode
set first is a cycle if written naively. The provider breaks it: the
management resource applies `mode` and `managed_url` in one call and
`default_policy_id` in a second, and `browserjs_policy` needs only the
account to be in `iac` mode, which the provider checks rather than requiring
a `depends_on`. An alternative is a separate `browserjs_default_policy`
resource; decided in implementation.)

### 8.3 Where it lives, and how it is installed

The request is for a provider "for this repo". The registries need a repo
named `terraform-provider-browserjs` (1.5). Proposed:

- **Source in this repo**, `terraform-provider-browserjs/`, a Go module of
  its own, so one pull request can change the API, its OpenAPI document and
  the provider together, and CI can run the acceptance tests against the
  same commit's backend.
- **Before any registry**: `dev_overrides` in `~/.terraformrc` (or
  `~/.tofurc`) for development, documented in `docs/`; for users, release
  archives from this repo's CI served as a static `network_mirror`, or
  unpacked into the implied local mirror directory
  (`~/.terraform.d/plugins/…`).
- **For the registries, when wanted**: a release job pushes the provider
  subtree to `r33drichards/terraform-provider-browserjs` and runs GoReleaser
  there with the GPG key, which produces what both registries require. The
  mirror repo holds no work of its own.

---

## 9. Implementation plan

Phase 0 is short and unblocks the rest; after it the tracks of phase 1 can be
built by separate agents at the same time.

### Phase 0: contracts and spikes

1. `docs/api/openapi.yaml` for section 6, and the JSON Schema of section 3.
   Everything else is built against these.
2. Spike in kind: mcp-js with a remote `mcp_tools` source pointing at a stub;
   confirm the request and answer shapes, `$(SESSION_ID)` expansion inside
   `MCP_V8_POLICIES_JSON`, the behaviour on timeout, and what the agent sees
   on a denial.
3. Spike on GKE staging: the source address the backend sees from a gVisor
   session pod through the Service; a restore from a snapshot followed by a
   decision.

### Phase 1: policies on browser operations (tracks in parallel)

| Track | Scope | Depends on |
|---|---|---|
| A. Policy core (backend) | policy, account and annotation storage; JSON-to-Rego translator with golden tests; validate and evaluate; the `/api` handlers; mode rules of 2.3 | phase 0 contract |
| B. PDP and deploy | the `:8090` listener, source check, cache, decision ring; pod template env in blueprint and warm template (and `deploy_test.go`); NetworkPolicy; namespace and Role; e2e in kind: a denied operation is denied, an edit applies without a restart | A's evaluator interface (stub it first) |
| C. UI | policies list, settings modal, Monaco editor with schema and server diagnostics, Rego grammar, test panel, session picker and decisions | phase 0 contract (mock server) |
| D. API tokens | token storage and page, the `api.` host route in Pomerium config, bearer authenticator, `ALLOWED_EMAILS`, rate limit | phase 0 contract |
| E. Terraform provider | section 8, against a fake from the OpenAPI document, then acceptance tests against kind | phase 0 contract; D for acceptance tests |
| F. Browser MCP caller check | section 7.2, in the browser image, with a test that a page cannot call `/mcp` | nothing |

Done when: a user can write a JSON or Rego policy in the UI, attach it, see
an agent's call denied and the decision listed; switch to `iac`, see the UI
lock with the link, and apply the example of 8.2.

### Phase 2

- Denial reasons returned to the agent (PDP as a `pre` hook).
- `fetch`, WebSocket and module imports as policy sections, behind the
  platform layer, after track F.
- Policy revision history, if wanted.
- Provider release pipeline and registry mirror repo.

### Phase 3

- Navigation enforced in the browser (CDP interception by the browser MCP
  server, `allow_navigation`), binding the human at the VNC view; or
  Chromium's managed policy, depending on the spike.
- Option B (evaluation in the pod) if PDP latency or backend load asks for
  it; an administrator policy layer for all users, if the product grows
  organisations.

---

## 10. Open questions

Each with the default this document assumes.

1. **JSON compiled to Rego, not cua-driver's two engines.** cua-driver's
   simple format is YAML evaluated natively. The request describes JSON
   translated to Rego. *Default: JSON, compiled to Rego, shown read-only.*
   Should YAML be accepted as well (the same schema)? *Default: no.*
2. **How hard is the `iac` lock?** Tailscale lets an admin "Edit anyway".
   *Default: a hard lock; switching the mode back in Settings is the escape
   hatch.* And is the exclusion two-way, so that tokens cannot write policies
   in `editor` mode? *Default: yes, as "mutually exclusive" reads.*
3. **Scope of the mode**: per account or per policy? *Default: per account.*
4. **Attachment**: per session, per-user default, or both? *Default: both;
   session, then default, then built-in.* Does `iac` mode also lock the
   per-session picker in the UI? *Default: no, only authorship and the
   default.*
5. **What v1 governs.** *Default: `browser_execute` operations only.
   `fetch`/modules in phase 2, real navigation control in phase 3, with the
   UI honest that host constraints limit requests, not the browser.* If
   "which sites the browser may visit, human included" is the point of the
   feature, phase 3 should be pulled forward and the order changes.
6. **API host and the allow-list.** A separate `api.` host, or a path on the
   app's host? *Default: separate host.* Mirror Pomerium's allow-list into
   the backend so tokens of removed users stop at once? *Default: yes.*
   Maximum token lifetime? *Default: 90 days, at most a year.*
7. **Storage.** ConfigMaps in a `browserjs-policies` namespace, a CRD, or a
   database? *Default: ConfigMaps.* Keep revision history? *Default: not in
   v1.*
8. **Evaluation in the backend** (option A), accepting that owners' Rego
   runs in the shared process, bounded as in 7.4? *Default: yes for v1;
   revisit with option B if users are no longer an allow-list.*
9. **Sessions that predate the feature** cannot be given a policy. *Default:
   mark them, ask the user to recreate; no migration.*
10. **Provider distribution.** Publish to the public registries (needs the
    mirror repo and a GPG key), or mirror only? *Default: in-repo source with
    `dev_overrides` and a network mirror first; registries when someone
    outside needs it.* Provider and resource names: `browserjs`,
    `browserjs_session`, `browserjs_policy`, `browserjs_policy_management`?
11. **Editor.** Monaco at about 850 KB gzipped on the editor page only, or
    CodeMirror 6 at about 120 KB with weaker Rego support? *Default: Monaco,
    bundled (no CDN), lazy-loaded, with a Rego grammar written here.*
12. **Limits**: 64 KiB a policy, 20 policies a user, 50 ms a decision.
    *Default: as stated.*
