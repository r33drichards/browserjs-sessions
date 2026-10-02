# Session policies: design

Status: proposal, for review. No product code is written and nothing is
deployed. Revision 2: folds in the product owner's decisions on the first
draft (below). Open questions are in the last section, each with a
recommended default.

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

## Decisions already made

These came back from the product owner on the first draft and are settled:

1. **Policies govern mcp-js only.** A person at the VNC view, and where
   Chromium navigates on its own, are out of scope ("the VNC is different and
   that's okay"). No URL allow-lists in Chromium, no NetworkPolicy work.
2. **Applying a policy restarts mcp-js.** The policy is saved somewhere
   durable; applying it restarts that session's mcp-js. No hot reload. A
   policy can be set when the session is created, which for a session adopted
   from the warm pool means save and restart right after adoption. Fail
   closed if the policy cannot be loaded.
3. **Session creation becomes a full page** (Cloudscape's single page
   create), and a policy is a **sub-resource of a session**, following
   Cloudscape's sub-resource patterns.
4. The data model, the editor-or-IaC switch and the Terraform resources
   follow from "a policy belongs to one session".

## Summary of the proposal

- A session has **at most one policy**, of kind `json` (a small declarative
  format) or `rego` (raw OPA). JSON is compiled to Rego by the backend when
  it is saved, so mcp-js loads one thing. A session with no policy runs the
  built-in one, which is today's behaviour.
- The policy is **stored on the session's Sandbox** (annotations), so it
  lives and dies with the session and the backend stays stateless.
- **Applying**: the backend sends the compiled policy, signed, to a small
  supervisor that replaces `start.sh` as PID 1 of the mcp-js container. The
  supervisor writes it to the session disk and restarts the `mcp-v8` process
  with it. The same step runs after warm adoption, and the copy on the disk
  is what a cold wake or a snapshot restore starts from.
- **A gate in the backend's proxy** forwards MCP traffic only while the pod
  reports the policy the Sandbox says it should have. That is what makes a
  policy changed while the session slept, a failed load, or a pod that never
  got its policy all fail closed.
- **Management mode is per session's policy**: `editor` (Monaco in the UI) or
  `iac` (read-only in the UI, with a link to where it is managed; only API
  tokens write).
- **API tokens** issued by the backend, on a separate host that Pomerium
  passes through and the backend verifies itself, are how Terraform signs in.
- A **Terraform/OpenTofu provider** `browserjs` with `browserjs_session` and
  `browserjs_session_policy`.
- **UI**: `/sessions/create` as a single page create with a Policy section;
  the session page becomes a details page with tabs, one of them Policy;
  editing the policy is a page edit with Monaco.
- Version 1 governs **the upstream MCP tool calls mcp-js makes**, which in a
  session means the operations of `browser_execute` and their parameters.
  mcp-js's other capabilities (`fetch`, module imports) are a later phase.

Two findings are worth reading first:

1. cua-driver's simple format is **YAML, not JSON, and it is not translated
   to Rego**: it is a second engine evaluated natively, next to a Rego engine
   (section 1.1). This design does what the request describes (JSON compiled
   to Rego) rather than what cua-driver does.
2. A policy on `browser_execute` can be **walked around from inside the pod**
   today, because the browser MCP server on `127.0.0.1:8081` accepts any
   caller, including a page in the Chromium it drives (section 7.2). This is
   about an agent evading the mcp-js policy, not about governing the browser,
   so it is inside the scope decided above. It needs a small fix.

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
`images/mcp-js/Dockerfile` and `start.sh`.

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
  evaluated in-process by regorus) or `http(s)://` (a remote OPA). Each
  source may name its own `rule`; the default is `data.mcp.<category>.allow`.
- **Reloading**: none for local files. "The chain is built once at startup";
  "Policy changes require a server restart." A bad file stops the server
  from starting.
- **What a session ships today** (`images/mcp-js/`): two local files.
  `mcp_tools.rego` allows exactly `browser` / `browser_execute`;
  `filesystem.rego` allows only `/data/memory`. No `fetch`, `subprocess` or
  `modules` source, so those capabilities do not exist in a session.
- **What lives where**: heap persistence is off (`MCP_V8_HEAP_STORE=none`).
  Artifacts, upload grants and the MCP session database
  (`MCP_V8_SESSION_DB_PATH=/data/mcp/sessions`) are on the session disk.
- **How the container starts**: `start.sh` is PID 1, waits for the browser
  MCP on `127.0.0.1:8081`, runs `mcp-v8` as a child and forwards TERM to it,
  because "mcp-v8 has no TERM handler" as PID 1.
- **Not governed by any mcp-js category**: the top-level MCP tools the client
  calls (`run_js`, artifact tools), and anything Chromium does on its own.
  The second is out of scope by decision 1.

The `mcp_tools` input carries the whole `browser_execute` call, so a policy
can read `arguments.operations[]` (`navigate`, `evaluate`, `click`, `type`, …
from `images/browser/browser/server.js`). That is the lever version 1 uses.
It constrains what the agent asks for through mcp-js, which is the scope.

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

### 1.6 Cloudscape's patterns for create, sub-resources, details and edit

Sources: cloudscape.design pattern pages, read through a summarising fetch;
the quotes are as that returned them and should be checked against the pages
when the UI is built.

- **Single page create**
  (`/patterns/resource-management/create/single-page-create/`): "optimized
  for simple to medium-complex forms"; the wizard is for "a set of
  interrelated tasks". Breadcrumbs with the service as root; a title that
  begins with an active verb; a short primary section ("keep it as short as
  possible") and "as many inputs as possible" in additional settings; "good
  defaults in as many inputs as possible to allow users to simply click the
  Create button"; buttons "Cancel" and "[Active verb] [resource type]"; a
  modal to confirm leaving with unsaved work.
- **Sub-resource create**
  (`/patterns/resource-management/create/sub-resource-create/`): three
  forms. *Embedded*, "a set of inputs within the page… when minimal
  configuration is required". *Split panel*, "for more complex
  configurations": "a dedicated space for task completion while keeping the
  main workflow in view". *New tab*, "a fallback option when the process is
  too complex". When the sub-resource is made, "populate the related resource
  selection input with the new resource", and warn about unsaved changes
  "when canceling or submitting a parent flow if there is a sub-create
  processing".
- **Details page with tabs**
  (`/patterns/resource-management/details/details-page-with-tabs/`): "use
  tabs to organize complex or lengthy content and user tasks into
  independent, self-contained categories"; "one tab, one task"; an optional
  summary container "always visible when users switch between tabs"; start
  with the details of the resource.
- **Page edit** (`/patterns/resource-management/edit/page-edit/`): "when you
  want users to manage an item by editing its properties and configuration
  in bulk"; on save, return to the page the edit started from, with a
  flashbar.
- **Unsaved changes** (`/patterns/general/unsaved-changes/`): a "Leave page"
  modal ("The changes that you made won't be saved.", Cancel / Leave) "after
  any action that could result in data loss", and `beforeunload` for the
  browser's own navigation.
- Cloudscape's own code editor component is built on Ace, not Monaco. The
  request names the VS Code editor, so this design embeds Monaco in a
  Cloudscape container and borrows the code editor's layout (editor above, a
  status bar with error and warning counts, a problems pane).

---

## 2. Data model

### 2.1 A session's policy

A policy is a sub-resource of a session: it belongs to exactly one session,
is created, read and deleted through that session, and goes when the session
goes. A session has zero or one.

| Field | Meaning |
|---|---|
| `kind` | `json` or `rego` |
| `source` | what the author wrote, at most 64 KiB |
| `rego` | what mcp-js loads: `source` for `rego`, the compiled module for `json` (derived, read-only) |
| `version` | counts saves for this session, starting at 1; never goes down |
| `sha256` | of `rego`; what the pod reports back as applied |
| `management` | `{mode: "editor" \| "iac", managed_url}` (section 2.3) |
| `updated`, `updated_by` | when, and `ui` or the token's name |
| `status` | `applied`, `pending` (saved; the session is not running), `failed` (with the pod's message), or `unsupported` (section 4.6) |

No policy means the **built-in policy**: any `browser_execute` operation,
which is exactly what a session does today. Nothing changes for a user who
never opens the Policy tab.

### 2.2 Reuse across sessions

There are no shared policy objects and no account-wide default. Reuse is:

- **By copy, in the UI.** The Policy section of the create page, and the
  policy edit page, offer "Copy from another session", which fills the editor
  with that session's source. The copy is independent from then on.
- **By Terraform.** One `file()` or local value referenced by several
  `browserjs_session_policy` resources, or a `for_each`. This is the answer
  for anyone who wants one policy kept the same across sessions: that is
  what IaC is for.

A shared "policy library" was the first draft's model. It is dropped: it
makes the policy a resource of its own with a lifecycle of its own, which is
what "sub-resource" rules out, and "who else is using this policy" is a
question the simple model never has to answer.

### 2.3 Management mode

Scoped to **the session's policy**. Tailscale's switch is per tailnet, and a
tailnet has exactly one policy file, so the switch there is in effect per
policy file; per session's policy is the same thing here. It also lets one
user keep a Terraform-managed session beside a scratch session edited by
hand, which a per-account switch would forbid.

```json
{ "mode": "editor" | "iac", "managed_url": "https://github.com/me/infra/blob/main/browserjs/research.tf" }
```

`managed_url` is required in `iac` mode and must be `https`.

| | `editor`, UI (cookie) | `editor`, API token | `iac`, UI (cookie) | `iac`, API token |
|---|---|---|---|---|
| Read the policy, validate, test | yes | yes | yes | yes |
| Save or delete the policy | yes | **no (409)** | **no (409)** | yes |
| Change `mode` and `managed_url` | yes | yes | yes | yes |
| Rename, stop, resume, delete the session | yes | yes | yes | yes |

- A token's save may carry `management` in the same request, so "take this
  policy over as code" is one call (it is what the Terraform resource does on
  create). A token's save to an `editor`-mode policy without it is refused.
- A refused write answers `409 {"error": "this policy is managed
  externally", "managed_url": …}`, or `"…managed in the editor"`.
- Switching back to `editor` in the UI is always possible. That is the
  escape hatch; there is no "Edit anyway" (open question 2). Terraform sees
  the change as drift on its next plan.
- An MCP client (the agent) can do none of this: the MCP route reaches only
  a session's `/mcp`, never the API.

### 2.4 Where it is stored

**Recommended: on the session's Sandbox, as annotations**, beside the owner
and name it already carries: `browserjs.dev/policy-source`, `-kind`,
`-version`, `-mode`, `-managed-url`, `-updated-by`. The compiled Rego is not
stored; it is derived from the source when applying.

- Its lifecycle is the session's: deleted with it, no orphans, nothing to
  sweep.
- No new RBAC: the backend already updates Sandboxes, and only Sandboxes.
- Setting it at creation is the same write that makes the session: for a cold
  session the create; for a warm one the compare-and-swap in `adopt()` that
  writes the owner. The `SandboxClaim` carries it too, as it carries the
  owner, so `RecoverClaims` can finish an adoption after a crash without
  losing the policy.
- `resourceVersion` is not a usable ETag (the Sandbox changes for other
  reasons), so `If-Match` uses the policy `version`.

Costs and what to check in the phase 0 spike: annotations total at most
256 KiB an object, which the 64 KiB limit respects; every list of sessions
now carries the policy sources (five sessions a user: at most 320 KiB a
poll; the list handler can ask for metadata it needs only if that hurts);
and it must be confirmed that neither the Sandbox controller nor the claim
controller copies Sandbox annotations onto the pod (UNVERIFIED).

Alternatives:

| Alternative | Cost |
|---|---|
| A ConfigMap per session | needs `configmaps` RBAC, and RBAC cannot select by label: in the sessions namespace that would let the backend rewrite Pomerium's config, so it means a second namespace, where an `ownerReference` to the Sandbox cannot reach; the backend deletes it itself and sweeps orphans |
| A CRD (`sessionpolicies.browserjs.dev`) | typed, owner-referenced, clean RBAC; a cluster-scoped object to install and version. The better home if policies grow history or status |
| A database | history and audit for free; the backend's first stateful dependency |

The copy **in the pod** (section 4) is on the session disk, written by the
supervisor. It is a cache of what the Sandbox says, never the source of
truth.

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
  `max`, `max_length`, `pattern`, `allowed`), and two for URL parameters,
  `hosts` (exact names, or `*.example.com` for subdomains) and `schemes`. A
  constrained parameter that is absent fails.
- Unknown keys are errors. A JSON Schema for this format is served by the
  backend and is the single source for the editor's completion, the API's
  validation and the provider's plan-time check.

Reserved for a later phase, rejected in v1 with a clear message: `fetch` and
`modules` sections.

### 3.2 Examples

No scripting: no `evaluate`, no `setContent`.

```json
{
  "version": 1,
  "allow": { "operations": ["*"] },
  "deny": { "operations": ["evaluate", "setContent"] }
}
```

The agent may only be sent to one site, and may type only short text:

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

By decision 1 a `hosts` constraint limits what the agent asks mcp-js for,
not where the browser can end up (a click on a link can leave the site). The
editor says so beside the constraint, once, as help text; it is not a
warning.

### 3.3 Translation to Rego

The backend compiles JSON to one Rego module on save. The contract for any
policy, generated or written by hand:

- package `browserjs.policy`;
- `allow_tool_call` decides an upstream MCP tool call; undefined or false
  denies. (`allow_fetch` and `allow_module` are reserved for phase 2.)
- `input` is mcp-js's `mcp_tools` document (section 1.3), unchanged.

The policy is evaluated **in the pod by regorus**, as the second source of
the `mcp_tools` chain, after the platform's own file (section 4.2). Two
consequences:

- The translator emits a deliberately small subset of Rego (`in`, `every`,
  comparisons, `count`, `lower`, `is_string`, `is_number`, `regex.match`,
  `startswith`, `endswith`), and phase 0 confirms each against the mcp-js
  image. Whether regorus as built into mcp-js has every one is UNVERIFIED.
- There is no URL parser. A `hosts` or `schemes` constraint compiles to one
  anchored regular expression over the lower-cased URL that only matches
  `scheme://host[:port]` followed by `/`, `?`, `#` or the end, with the host
  drawn from `[a-z0-9.-]`. A URL with userinfo, a backslash, whitespace, a
  percent-encoded or non-ASCII host does not match and is denied. That is
  stricter than a parser and has no parser to disagree with Chromium's.

The second example compiles to (illustrative; golden tests in the
implementation fix the exact text):

```rego
# Generated from the JSON policy of session s-abcde, v3. Edit the JSON, not this.
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
	is_string(op.params.url)
	regex.match(`^https://(example\.com|([a-z0-9-]+\.)+example\.com)(:[0-9]+)?([/?#].*)?$`, lower(op.params.url))
}

operation_allowed(op) if {
	op.type == "type"
	is_string(op.params.text)
	count(op.params.text) <= 500
}
```

The translator is a pure function with table-driven tests: for each example,
the generated Rego (golden file) and inputs whose decisions are asserted,
run twice, once with the Go OPA library and once through the real mcp-js
image, so the JSON semantics, OPA and regorus cannot drift apart unnoticed.
The UI shows the generated Rego read-only and offers "Convert to Rego", one
way.

Validation before a save:

1. `json`: schema check, then compile to Rego.
2. Parse and compile with the Go OPA library, for errors with row and column.
3. The package must be `browserjs.policy`, and `allow_tool_call` must exist.
4. Evaluate a fixed set of sample inputs with a deadline.

That is the fast feedback. The check that counts is the pod loading it
(section 4.3): OPA and regorus are two implementations, and a policy OPA
accepts may be one regorus refuses.

---

## 4. Applying a policy

### 4.1 What has to be true

- The pod is usually **already running** when it gets an owner (warm pool),
  and its pod spec and env cannot be changed then.
- The backend cannot exec into a pod (no such RBAC, and the browser image
  has no shell tools), and a pod can reach only the internet: not the
  backend, not the API server.
- Kubernetes has no "restart this one container" request.
- After a **cold wake** the container starts from nothing; after a
  **snapshot restore** the processes resume with the memory they had.
- Anything listening inside the pod is reachable from Chromium over
  loopback, so from a page, so from the agent.

### 4.2 Mechanism: a supervisor in the mcp-js container

`start.sh` is already PID 1 of the container, starts `mcp-v8` as a child and
forwards signals. It is replaced by a small static Go binary,
`mcp-supervisor`, built in this repo and added to the mcp-js image, which
does what `start.sh` does and three things more.

```
backend ──PUT /policy (signed bundle)──▶ pod :8082  mcp-supervisor (PID 1)
        ◀── 200 {version, sha256} ─────             │ writes /data/mcp/policy/
        ──GET /policy ─────────────────▶            │ stops and starts
                                                    ▼
                                                 mcp-v8 :8080  (policy chain built at start)
```

1. **It holds the policy.** `/data/mcp/policy/current/` on the session disk
   has `policy.rego` and `bundle.json` (session ID, version, sha256, the
   signature). The disk is mounted at that path only in the mcp-js
   container, and `run_js` cannot write there: the filesystem policy allows
   `/data/memory` only, and that policy is the platform's, not the owner's.
2. **It builds mcp-v8's configuration.** `MCP_V8_POLICIES_JSON` is no longer
   fixed in the image; the supervisor sets it for the child:

   ```json
   {
     "mcp_tools":  { "mode": "all", "policies": [
       { "url": "file:///etc/mcp/mcp_tools.rego" },
       { "url": "file:///data/mcp/policy/current/policy.rego",
         "rule": "data.browserjs.policy.allow_tool_call" } ] },
     "filesystem": { "policies": [ { "url": "file:///etc/mcp/filesystem.rego" } ] }
   }
   ```

   The first source is the platform's layer (only `browser` /
   `browser_execute`), and the mode is `all`: the owner's policy can only
   narrow it, as cua-driver's managed layer works. With no policy on the
   disk the second source is left out, which is the built-in policy.
3. **It applies a new one.** `PUT /policy` on port 8082 with a bundle. The
   supervisor verifies it (below), writes it to `next/`, stops `mcp-v8`
   (TERM, a few seconds, then KILL), starts it with the new configuration
   and waits for it to answer on `127.0.0.1:8080`. If it comes up, `next/`
   becomes `current/` and the answer is `200`. If it does not (regorus
   refused the file), the supervisor restarts `mcp-v8` with the previous
   `current/`, and answers `422` with the tail of `mcp-v8`'s stderr.
   `DELETE /policy` (signed the same way) returns to the built-in policy.
4. **It reports.** `GET /policy` answers `{version, sha256, state}` for what
   the running `mcp-v8` was started with. No authentication; it reveals a
   hash.

**Restarting the process, not the container.** The decision says "restarts
the mcp-js container". The supervisor restarts `mcp-v8` inside the container,
which has the same effect on mcp-js (it reads its policies again from
nothing) and avoids what a container exit brings: the kubelet's restart
back-off (ten seconds, doubling, for a container that exits repeatedly), a
restart count that looks like crashing, and the pod going unready. If a true
container restart is wanted, the supervisor exiting after writing `next/` is
a one-line change and the rest of the design stands (open question 8).

**The bundle is signed.** The port is reachable from the backend
(NetworkPolicy: one more ingress port, 8082, from `app: backend`) but also
from anything in the pod over loopback, and a page that could `PUT` a policy
would be the agent writing its own rules. So:

- the backend signs `(session ID, version, sha256, rego)` with an Ed25519
  key from a Secret; the pod template carries the public key in env, which
  is the same for every pod and so fine for warm ones;
- the supervisor requires the session ID to be its own (`SESSION_ID`, which
  the warm template already sets from the pod's name), so another session's
  bundle is refused;
- it requires the version to be higher than the one on its disk, so an old,
  weaker policy of the same session cannot be replayed;
- key rotation: the env carries a list of keys; a Sandbox's env is fixed at
  creation, so the backend keeps signing with a key a session trusts for as
  long as that session exists.

Alternatives considered for the trigger:

| Mechanism | Why not |
|---|---|
| `start.sh` watching a version file | something still has to write the file into the pod; that is the supervisor's endpoint with extra steps |
| A liveness probe keyed on the policy version | probes are part of the pod spec, fixed for a warm pod; and it is a container restart with back-off |
| A ConfigMap volume | a pod template cannot name a per-pod ConfigMap, and warm pods exist before their session |
| mcp-js fetching from the backend at start | session pods cannot reach the backend, by design |
| A shutdown endpoint in mcp-js | a change to mcp-js itself, and still needs the policy delivered |

### 4.3 The flows

**Save and apply, session running** (the UI's Save, a token's `PUT`):

1. Validate (section 3.3). Refuse on errors.
2. Write the source to the Sandbox with `version` n+1. This is the durable
   save.
3. Compile, sign, `PUT` to the pod. Hold new MCP requests for this session
   while it runs (as the waker already holds requests for a waking session).
4. `200`: done, `status: applied`. `422`: the pod is running the previous
   policy again; the backend writes the previous source back to the Sandbox
   as version n+2, applies that, and answers the caller `422` with the pod's
   message. A save either takes effect or changes nothing.

**Session not running** (stopped or asleep): steps 1 and 2 only;
`status: pending`. Saving does not wake a session.

**Create with a policy.** Cold: the Sandbox is created with the policy
annotations; the pod starts with no policy on its disk; the gate (below)
applies it before the first request. Warm: `adopt()` writes the annotations
with the owner, then the backend applies at once, so the restart happens
during creation and not on the user's first call. `POST /sessions` answers
when the Sandbox is written, as today; the session shows `starting` until the
policy is applied.

**The gate.** The proxy already resolves a session to a running pod before
forwarding (`Waker`). It gains one condition: the pod's `GET /policy` must
report the `version` and `sha256` the Sandbox calls for. The answer is
remembered with the pod's address for as long as the waker remembers the
session (two seconds today), and forgotten on a save. If they differ, the
backend applies and then forwards; if applying fails, it answers `503` with
"this session's policy could not be loaded" and forwards nothing. The gate
covers the MCP route and the upload route.

This one rule covers every way the pod and the Sandbox can disagree:

| Situation | What the pod has | What happens |
|---|---|---|
| Cold start, or cold wake | `current/` from the disk; the supervisor verifies its signature before starting `mcp-v8` | matches: forward. No policy on disk but one on the Sandbox (new session): apply first |
| Restore from a snapshot | the `mcp-v8` that was running, with the policy it had | matches unless the policy was saved while asleep: then apply first |
| Policy saved while asleep or stopped | the old one | apply at the next wake, before the first request |
| Warm adoption | none | apply during creation |
| Backend died between save and apply | the old one | apply at the next request |
| The disk copy is corrupt or its signature is bad | the supervisor does not start `mcp-v8`; state `failed` | apply again from the Sandbox |

### 4.4 Failing closed

- `mcp-v8` is never started with a policy the supervisor could not verify,
  and never falls back to the built-in policy when one is expected. The
  built-in policy is used only when the Sandbox has no policy.
- The backend forwards nothing to a pod whose applied policy is not the
  Sandbox's.
- A policy that the pod refuses at a wake (it validated in the backend but
  regorus rejects it, or an image upgrade changed what loads) leaves the
  session unusable over MCP, `status: failed` with the message, until the
  owner saves one that loads or deletes the policy. The browser and the VNC
  view keep working; they are not mcp-js.
- A pod with no supervisor (section 4.6) cannot be given a policy at all.

### 4.5 What a restart costs

Measured values are for the phase 0 spike; the list is from the code.

- **Lost**: every `run_js` in flight (its V8 isolate dies with the process),
  and the open connections of MCP clients.
- **Kept**: artifacts, upload grants and the MCP session database, all on
  the session disk; `/data/memory`; and everything in the browser, which is
  another container: tabs, logins, the page the agent was on.
- **Not lost because it never existed**: JS heap state between calls (heap
  persistence is off).
- **Connected MCP clients**: a call in flight fails (the backend answers
  `502` when the pod drops the connection); the event stream of a client
  that keeps one open closes and the client reconnects. Calls that arrive
  during the restart are held by the backend and forwarded when `mcp-v8` is
  up, so a client that was idle notices nothing. Whether a client's
  `Mcp-Session-Id` is still accepted after the restart, given the session
  database is on disk, is UNVERIFIED; if it is not, the client gets `404`
  and initialises again, which the MCP specification requires clients to
  handle.
- **To be kind to running work**, the backend knows how many calls a session
  has in flight (the idle tracker). The UI's confirmation says "2 calls are
  running and will be interrupted"; the API takes `?wait=30s` to wait for
  them to finish first.
- **Time**: `mcp-v8` start plus the supervisor's wait; the browser MCP is
  already up, so not the 120 s wait of a cold start. Expected a few seconds
  (UNVERIFIED).

### 4.6 Sessions that exist before this ships

A Sandbox's pod template is fixed when it is created, and a snapshot restores
the old processes. Sessions created before the rollout have `start.sh`, no
supervisor and no port 8082. Their policy `status` is `unsupported`, saving a
policy to one is refused, and the UI says "created before policies; recreate
the session to give it one". The gate treats them as the built-in policy,
which is what they enforce. The warm pool replaces its waiting pods on a
template change (`updateStrategy: Recreate`), so new sessions are covered
from the deploy on.

### 4.7 Not covered

- Where the browser goes, and the person at the VNC view: out of scope by
  decision 1.
- `fetch`, WebSocket and module imports from `run_js`: off today, off in v1.
  Phase 2 can add `allow_fetch` and `allow_module` the same way (the
  supervisor adds a chain when the policy defines the rule), with a platform
  file in front that denies loopback and private hosts. Prerequisite:
  section 7.2.
- Subprocess, the filesystem outside `/data/memory`, `run_js_file`: the
  platform's, not offered to owners.
- The top-level MCP tools (`run_js` itself, artifacts): not an mcp-js policy
  category.
- A record of decisions. mcp-js evaluates in the pod and does not report
  each decision; a denial is an error inside `run_js`. The first draft's
  "recent decisions" panel is gone with the first draft's design.

---

## 5. UI

Wireframe theme, as now. Which Cloudscape pattern applies where:

| Task | Pattern | Why |
|---|---|---|
| Create a session | Single page create, `/sessions/create` | one short form; replaces the modal |
| Give the new session a policy | Sub-resource create, **embedded** for the choice and the presets, **split panel** for writing one | a preset is "minimal configuration"; writing Rego is "more complex… with the potential for errors", and the split panel keeps the session form in view |
| See a session | Details page with tabs: Browser, Policy | the screen and the policy are "independent, self-contained" tasks; the summary stays visible |
| Edit the policy | Page edit, `/sessions/:id/policy/edit` | Monaco needs the page's width; one Save for the whole document |
| Leave with changes | Unsaved changes modal, and `beforeunload` | on both the create page and the edit page |
| Policy managed as code | The same pages, read-only, with an info alert carrying the link | nothing is hidden; only the write actions go |

The new-tab form of sub-resource create is not used: a policy is not complex
enough to leave the flow for.

### 5.1 Create session (`/sessions/create`)

The sessions list's "Create session" button goes here instead of opening the
modal.

```
browserjs sessions > Create session

Create session

+-- Session ---------------------------------------------------------------+
| Name - optional                                                          |
| [ brave-otter                                    ]   (placeholder)       |
| Left empty, the session is called brave-otter.                           |
+--------------------------------------------------------------------------+

+-- Policy ----------------------------------------------------------------+
| What an agent connected over MCP may ask this browser to do.             |
| It does not restrict you at the screen. You can change it later.         |
|                                                                          |
| (•) No restrictions            the built-in policy                       |
| ( ) No scripting               everything except evaluate and setContent |
| ( ) Copy from a session        [ research           v ]                  |
| ( ) Write a policy             [ Edit policy ]   JSON · 14 lines · valid |
| ( ) Managed as code            Terraform, OpenTofu or the API            |
|       Link to where it is managed                                        |
|       [ https://                                              ]          |
+--------------------------------------------------------------------------+

                                             [ Cancel ]  [ Create session ]
```

- The name keeps its pet-name placeholder, generated as now and used when
  the field is left empty. "No restrictions" is preselected, so Create works
  untouched ("good defaults… to simply click the Create button").
- **Embedded**: the radio group, the presets and the copy picker.
- **Split panel**: "Edit policy" opens a bottom split panel with the editor
  of 5.3 (Monaco, the JSON/Rego switch, Validate, the problems pane), the
  form still visible above. "Use this policy" closes the panel and fills the
  line beside the radio ("JSON · 14 lines · valid"): the sub-resource create
  pattern's "populate the related resource selection input". Nothing is
  saved until Create session.
- "Managed as code" asks for the link (required, `https`) and creates the
  session with no restrictions and `mode: iac`; the policy then arrives by
  token. The helper text says the session is unrestricted until it does.
- Create with the panel open and unconfirmed changes in it, or Cancel with
  anything entered, raises the Leave page modal.
- On success: the session's details page with a flashbar, "Session
  brave-otter created", and the state `starting` while the policy is applied.
  A policy the pod refuses fails the creation and returns to the form with
  the error on the Policy section.

### 5.2 Session details (`/sessions/:id`)

```
browserjs sessions > brave-otter

brave-otter                        [ Stop ] [ Rename ] [ Delete ]
+-- Summary ---------------------------------------------------------------+
| State  running      MCP URL  https://s-abcde.sessions…/mcp  [copy]       |
| Created  2 h ago    Policy   JSON, v3, applied                           |
+--------------------------------------------------------------------------+
[ Browser ] [ Policy ]

Policy                                 [ Copy from session ] [ Remove ] [ Edit ]
+--------------------------------------------------------------------------+
| Kind  JSON     Version  3     Status  applied     sha256  9f2c…          |
| Managed in  this editor                                    [ Change ]    |
| Last saved  2 h ago, in the UI                                           |
+-- policy.json (read-only) ------------+-- Generated Rego ----------------+
| {                                     | package browserjs.policy         |
|   "version": 1,                       | import rego.v1                   |
|   …                                   | …                                |
+---------------------------------------+----------------------------------+
```

- The Browser tab is today's page (the VNC pane). The Policy tab shows the
  policy read-only, or an empty state, "No restrictions: an agent may use
  every browser operation", with "Add policy".
- Status `pending` reads "saved; applies when the session next starts";
  `failed` is an error alert with the pod's message and "Edit".
- "Change" (managed in) opens a small modal with the two modes and the link
  field: a two-field setting, which is what a modal is for.

### 5.3 Edit policy (`/sessions/:id/policy/edit`)

```
browserjs sessions > brave-otter > Edit policy

Edit policy
+--------------------------------------------------------------------------+
| Format  [ JSON | Rego ]                      [ Copy from session v ]     |
+-- policy.json (Monaco) ---------------+-- Generated Rego (read-only) ----+
| {                                     | package browserjs.policy         |
|   "version": 1,                       | import rego.v1                   |
|   "allow": {                          | allow_tool_call if {             |
|     "operations": ["clik"],           |   …                              |
|       ~~~~~~                          |                                  |
+---------------------------------------+----------------------------------+
| JSON   Ln 4, Col 21     x 1 error   ! 0 warnings                         |
| 4:21  unknown operation "clik"; did you mean "click"?                    |
+--------------------------------------------------------------------------+
+-- Test ------------------------------------------------------------------+
| Sample call  [ navigate to a URL v ]                          [ Run ]    |
| { "server": "browser", "tool": "browser_execute", "arguments": …         |
| Result  DENY                                                             |
+--------------------------------------------------------------------------+
                                           [ Cancel ]  [ Save and apply ]
```

- The format switch on a new policy picks the kind. On an existing JSON
  policy, Rego shows the generated module with "Convert to Rego (cannot be
  undone)". In Rego there is one pane.
- JSON problems come from the schema in the browser as you type; Rego
  problems from the server's `validate`, debounced.
- Test runs in the backend with OPA and says so: "the session's own engine
  has the last word when you save".
- **Save and apply** on a running session confirms first: "Saving restarts
  this session's MCP server. Calls that are running (2) are interrupted. The
  browser is not affected." Then it returns to the Policy tab with a
  flashbar: "Policy applied (v4)", or stays on the page with the pod's error
  if the session refused it. On a stopped or sleeping session there is no
  confirmation and the flashbar reads "Policy saved; it applies when the
  session starts."
- A save answered `412`: "This policy changed since you opened it", with
  Reload.
- Cancel or leaving with changes: the Leave page modal.

### 5.4 Managed as code

On the Policy tab:

```
+--------------------------------------------------------------------------+
| (i) This policy is managed as code.                                      |
|     Edit it at  https://github.com/me/infra/blob/main/browserjs/main.tf  |
|     Changes made here would be overwritten.        [ Manage here instead ]|
+--------------------------------------------------------------------------+
Policy                                                    [ View source ]
| Kind  Rego   Version  7   Status  applied   Last saved  by token "ci"    |
```

- Edit, Remove and Copy from session are absent. The source and generated
  Rego stay visible, read-only. "View source" opens the edit page's layout
  with Monaco read-only, a "Read-only: managed as code" strip in place of
  the buttons, and Test still working (it saves nothing).
- "Manage here instead" is the mode modal of 5.2; choosing the editor
  returns the write actions. It is the only way out, and it is one click.
- The sessions list shows a small "as code" tag beside such a session's
  policy column.
- Going to `/sessions/:id/policy/edit` directly while in this mode shows the
  read-only view, not an error.

---

## 6. API

### 6.1 Additions

The same handlers serve the UI (cookie, on the app's host under `/api`) and
tokens (on the API host under `/v1`); the table in 2.3 says who may write.

| Method and path | Purpose |
|---|---|
| `POST /sessions` | accepts `policy: {kind, source}` and `policy_management: {mode, managed_url}` |
| `GET /sessions`, `GET /sessions/{id}` | add `policy: {kind, version, sha256, status, management}`, without the source |
| `GET /sessions/{id}/policy` | the whole policy, with `source` and `rego`; `404` when there is none |
| `PUT /sessions/{id}/policy` | `{kind, source, management?}`; saves and applies (section 4.3). `If-Match: <version>` optional; `?wait=30s` optional. `200` applied, `202` saved and pending, `409` wrong mode, `412` version, `422` invalid or refused by the session, with `errors[]` |
| `DELETE /sessions/{id}/policy` | back to the built-in policy and `editor` mode; applies like a save |
| `PUT /sessions/{id}/policy/management` | `{mode, managed_url}` |
| `POST /policies/validate` | `{kind, source}` → `{ok, rego?, errors[], warnings[]}`; saves nothing |
| `POST /policies/evaluate` | `{kind, source, input}` → `{allow}`; saves nothing |
| `GET /policy-schema.json` | the JSON Schema of the JSON format |
| `GET /tokens`, `POST /tokens`, `DELETE /tokens/{id}` | cookie only; the token is shown once, on creation |

Errors keep the existing shape, `{"error": "…"}`, plus `errors[]` with `row`,
`col`, `code`, `message` for validation. An OpenAPI document for all of it is
the first deliverable of the plan (section 9).

### 6.2 API tokens

- **Form**: `bjs_<token id>_<43 characters of base64url randomness>`. The
  prefix makes it findable by secret scanners; the id lets the backend look
  up one record and compare a SHA-256 in constant time. Only the hash is
  stored.
- **Belongs to a user** and acts as that user: that user's sessions and
  their policies, nothing else. A token never has admin rights, even an
  admin's.
- **Scopes**: `sessions` and `policies`, each `read` or `write`. A token
  cannot create or revoke tokens.
- **Expiry is mandatory**: 90 days by default, at most a year. The UI shows
  last use, to the hour.
- **Storage**: tokens are the one thing that does not belong to a session.
  A single Secret, `api-tokens`, in the sessions namespace, one key a token
  (owner, name, scopes, expiry, hash), with a Role naming that one Secret
  (`resourceNames`), so the backend gains access to nothing else.
- **Route**: a host of its own, `api.<domain>`, to the backend with
  `allow_public_unauthenticated_access: true` and no identity headers. The
  backend already tells requests apart by host; on this host it accepts only
  `Authorization: Bearer bjs_…` and serves only `/v1/…`. No cookie is ever
  sent there, so there is no CSRF surface. Costs one DNS record and one
  certificate name.
- **Who may still use the product.** Pomerium's allow-list of email
  addresses is not consulted on this route. Without care, a user removed
  from it would keep API access until their tokens expire. The backend
  therefore needs the same list (`ALLOWED_EMAILS`, from the same kustomize
  value as Pomerium's policy) and checks the token's owner against it on
  every request; admins can list and revoke anyone's tokens.
- Failed attempts are rate-limited per source address; the answer for a bad,
  expired or revoked token is the same 401.

---

## 7. Security review

### 7.1 What a policy is for

The author of a policy is the owner of the session, who can already do
anything with it, at the screen or over MCP. So a policy is not a boundary
against its author. It protects the owner's interests against:

- **The agent connected over MCP**, which the owner has delegated to and
  which may be wrong, over-eager, or steered by text it read on a page
  (prompt injection). The browser is logged in to the owner's accounts. The
  policy is the owner's statement of how far the delegation goes.
- **Web pages in the browser**, to the extent they act through the agent: a
  page cannot call MCP, but it can try to talk the agent into it, and the
  policy bounds what a successfully injected agent can then do through
  mcp-js.

It does not protect against the owner at the VNC view, or constrain the
browser itself (decision 1).

So the agent must not be able to change, weaken or step around the policy
through anything the session offers:

- the management API is not reachable over MCP, and API tokens exist only
  where the owner puts them. A token pasted into a page, into
  `/data/memory` or into an agent's prompt hands the agent the policy. The
  token page says so;
- `run_js` has no `fetch` in v1, so code in the session cannot call the API
  host even with a token;
- the policy on the disk is out of `run_js`'s reach (the filesystem policy),
  and the supervisor takes a new one only when it is signed by the backend
  for this session with a higher version (section 4.2).

And some things stay outside the owner's control whatever they write:

- the platform layer: only the `browser` server and `browser_execute`,
  filesystem only under `/data/memory`, no subprocess; later, no loopback or
  private addresses for `fetch`. The owner's policy is intersected with it
  and can only narrow;
- isolation between sessions and users (NetworkPolicy, a host per session,
  gVisor): not expressible in a policy;
- the signing key, and who may sign in.

### 7.2 A way around the policy: callers of the browser MCP server

`images/browser/browser/server.js` listens on `:8081` and serves any caller.
From outside, only the backend reaches the pod, and only on its allowed
ports. Inside, everything shares loopback, including Chromium. An agent
allowed `navigate` and `evaluate` can open `http://127.0.0.1:8081/healthz`
and, from that origin, `fetch("/mcp", …)` a `browser_execute` call that
mcp-js, and so the policy, never sees. A hostile page might try the same
without the agent (whether Chromium's local-network protections stop it is
UNVERIFIED). This comes from reading the code; it has not been tried.

Today it does not matter, because the policy allows everything. With owner
policies it does: "everything except `screenshot`" means nothing if
`evaluate` is allowed. It is not about governing the browser; it is the
agent leaving mcp-js's jurisdiction. Fix, in the browser image:

- the browser MCP server refuses any request carrying an `Origin` or
  `Sec-Fetch-Site` header (a browser always sends one on such a request;
  mcp-js does not), and listens on `127.0.0.1` rather than `::`;
- better, a secret that the supervisor generates at start, passes to
  `mcp-v8` for the upstream and that the server requires. Whether
  `MCP_V8_MCP_CONFIG` can carry request headers is UNVERIFIED, and the two
  containers would need a shared `emptyDir` for it;
- until it is fixed, the validator warns on a policy that allows `evaluate`
  together with unconstrained `navigate` while denying something else.

The supervisor's own port has the same exposure and is why bundles are
signed.

### 7.3 The supervisor

- It is new code running as PID 1 in every session. It is small (an HTTP
  server with three routes, a signature check, a child process) and has no
  secrets: only a public key.
- `PUT` and `DELETE` are useless without the backend's signature;
  `GET /policy` reveals a version and a hash. Request bodies are bounded.
- A lost signing key lets whoever has it, and can reach a pod's port 8082,
  replace policies: only the backend can reach it from outside, so in
  practice a page inside the same pod. Rotating is a template change plus
  the key list of 4.2.
- Replay: another session's bundle fails the session ID check; an older one
  fails the version check; the version on disk is on the session disk and
  survives restarts and restores.

### 7.4 The owner's Rego

It runs in the owner's own pod, in regorus, inside a container with a CPU
and memory limit. A policy that loops or allocates hurts that session's
mcp-js and nothing else, which is a better place for it than the shared
backend where the first draft put it.

The backend still parses, compiles and test-evaluates what users type
(`validate`, `evaluate`). There: OPA capabilities without `http.send`,
`net.lookup_ip_addr` and `opa.runtime`; a one-second deadline; 64 KiB of
source; a bound on input size. Users are an allow-list, not the public.
Regular expressions are RE2-style, linear time, in both engines (UNVERIFIED
for regorus's regex builtin).

### 7.5 Other points

- **URLs in policies** are matched by an anchored expression that refuses
  anything unusual (section 3.3), rather than parsed. Unit tests carry the
  known confusions (userinfo, backslashes, whitespace, IDNs, trailing dots).
- **`managed_url` is shown as a link.** `https` only, rendered as text with
  `rel="noopener noreferrer"`; it is the user's own setting shown to
  themselves and to admins.
- **Admins** can read every session's policy, as they can already see every
  session. Whether they can edit them follows whatever they can do to
  sessions today.
- **Two engines.** The backend validates with OPA and the pod enforces with
  regorus. The design never trusts the first for enforcement: a save is
  confirmed by the pod, and the gate compares hashes of what was loaded.
- **Fail closed** everywhere (4.4).

---

## 8. Terraform provider

### 8.1 Outline

- Go, `terraform-plugin-framework`, protocol 6; works with Terraform and
  OpenTofu. Provider type `browserjs`.
- Configuration: `endpoint` (default `https://api.browserjs.com`,
  `BROWSERJS_ENDPOINT`) and `token` (sensitive, `BROWSERJS_TOKEN`).
- `browserjs_session`: `name`; computed `id`, `mcp_url`, `url`, `state`.
  Destroying one deletes its disk and the browser's logins; the docs
  recommend `prevent_destroy`. Import by id.
- `browserjs_session_policy`: `session_id`, exactly one of `json` or `rego`,
  and `managed_url` (required); computed `version`, `sha256`,
  `compiled_rego`, `status`. Validated at plan time with
  `POST /policies/validate`, as `tailscale_acl` is. JSON is compared
  semantically, so reformatting is not a diff. Import by session id.
- Data sources: `browserjs_session` (by name or id), `browserjs_sessions`,
  and `browserjs_policy_document`, which builds the JSON format from HCL
  blocks in the manner of `aws_iam_policy_document`.
- Tests: unit tests against a fake API from the OpenAPI document;
  acceptance tests (`TF_ACC`) against the local kind deployment.

**A separate resource, not a block on the session.** Recommended because:

- **The resource is the mode.** Creating `browserjs_session_policy` saves
  the policy with `mode: iac` and the `managed_url` in one call; destroying
  it removes the policy and returns the session to `editor`. "Managed as
  code" is then exactly "there is a Terraform resource for it", with no
  third resource for the switch. Tailscale needs two
  (`tailscale_acl` and `tailscale_tailnet_settings`) and does not tie them
  together.
- **Sessions are pets.** A session made in the UI, with its logins, can have
  its policy taken over by code by naming its `session_id`, without
  importing the session or risking a plan that replaces it.
- **A policy change can never be read as a session change.** With a nested
  block, one wrong `ForceNew` and an edit to a policy destroys a disk.

The cost is a moment between the two creates when the new session has the
built-in policy. Nothing has the session's MCP URL before the apply returns,
so nothing can use that moment; if that is not good enough, `POST /sessions`
already takes a policy and the provider can grow an optional block later.

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

locals {
  managed_url = "https://github.com/r33drichards/infra/tree/main/browserjs"
}

resource "browserjs_session" "research" {
  name = "research"

  lifecycle {
    prevent_destroy = true
  }
}

resource "browserjs_session_policy" "research" {
  session_id  = browserjs_session.research.id
  managed_url = local.managed_url

  json = jsonencode({
    version = 1
    allow   = { operations = ["*"] }
    deny    = { operations = ["evaluate", "setContent"] }
  })
}

# The same Rego on two more sessions: reuse is the configuration's job.
resource "browserjs_session" "worker" {
  for_each = toset(["worker-a", "worker-b"])
  name     = each.key
}

resource "browserjs_session_policy" "worker" {
  for_each    = browserjs_session.worker
  session_id  = each.value.id
  managed_url = local.managed_url
  rego        = file("${path.module}/one-site.rego")
}

output "mcp_url" {
  value = browserjs_session.research.mcp_url
}
```

An apply that changes a policy restarts that session's mcp-js; the resource's
documentation says so, and `status` shows `pending` for a session that was
not running.

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

1. `docs/api/openapi.yaml` for section 6, the JSON Schema of section 3, and
   the supervisor's contract: the bundle format, what is signed, the three
   routes and their answers. Everything else is built against these.
2. Spike, in kind, with the real mcp-js image: stop and start `mcp-v8` under
   a parent process with a second `mcp_tools` source and a custom `rule`;
   how long it takes; what a bad Rego file does at start; what a connected
   MCP client sees, and whether its session ID survives.
3. Spike: each Rego construct the translator emits, loaded by that image.
4. Spike: a 64 KiB annotation on a Sandbox, cold and through a
   `SandboxClaim`; that it stays off the pod; a snapshot, a policy change,
   a restore.

### Phase 1: policies on browser operations (tracks in parallel)

| Track | Scope | Depends on |
|---|---|---|
| A. Policy core (backend) | policy on the Sandbox (create, adopt, claim recovery); JSON-to-Rego translator with golden tests; validate and evaluate; the handlers of 6.1; mode rules of 2.3 | phase 0 contracts |
| B. Supervisor and image | `mcp-supervisor` (start, verify, apply, report), replacing `start.sh`; its tests; mcp-js image; pod templates (public key, port) and `deploy_test.go`; NetworkPolicy port | supervisor contract |
| C. Apply and gate (backend) | signing; apply after save and after adoption; the gate in the waker; holding requests during a restart; rollback on `422`; e2e in kind: a denied operation is denied, a save applies, a change while asleep applies at wake, a bad policy fails closed | A's storage, B's contract (a fake supervisor first) |
| D. UI | `/sessions/create`; details page with tabs; policy edit page with Monaco, schema and server diagnostics, Rego grammar, test; managed-as-code state; unsaved-changes modals | phase 0 contracts (mock server) |
| E. API tokens | token storage and page, the `api.` host route in Pomerium's config, bearer authenticator, `ALLOWED_EMAILS`, rate limit | phase 0 contracts |
| F. Terraform provider | section 8, against a fake from the OpenAPI document, then acceptance tests against kind | phase 0 contracts; E for acceptance tests |
| G. Browser MCP caller check | section 7.2, in the browser image, with a test that a page cannot call `/mcp` | nothing |

Done when: a user can create a session with a policy on the new page, see an
agent's call denied, edit the policy and have it apply with a restart; put a
session to sleep, change its policy, and find it enforced at the first call
after waking; switch a policy to managed-as-code, see the UI lock with the
link, and apply the example of 8.2.

### Phase 2

- `fetch`, WebSocket and module imports as policy sections, behind the
  platform layer, after track G.
- Denials that explain themselves to the agent (a `pre` hook that returns a
  `reason`), and a decision log if mcp-js grows one.
- Provider release pipeline and the registry mirror repo.
- A CRD for policies, if history or richer status is wanted.

---

## 10. Open questions

Each with the default this document assumes. Questions the product owner has
already answered are at the top of the document and are not repeated.

1. **JSON compiled to Rego, not cua-driver's two engines.** cua-driver's
   simple format is YAML evaluated natively. The request describes JSON
   translated to Rego. *Default: JSON, compiled to Rego, shown read-only.*
   Accept YAML as well (the same schema)? *Default: no.*
2. **How hard is the managed-as-code lock?** Tailscale lets an admin "Edit
   anyway". *Default: a hard lock; "Manage here instead" is the escape
   hatch.* And is the exclusion two-way, so that tokens cannot save a policy
   that is in `editor` mode unless they take it over in the same request?
   *Default: yes, as "mutually exclusive" reads.*
3. **Mode scope.** *Default: per session's policy*, which matches
   Tailscale's one switch for one policy file. The alternative is one switch
   for a user's whole account.
4. **Reuse.** *Default: by copy in the UI and by Terraform; no shared
   policies and no account default.* Is a default for new sessions wanted
   after all (it would be a preset remembered per user, copied at creation)?
5. **What v1 governs within mcp-js.** *Default: upstream tool calls, that
   is `browser_execute` operations. `fetch` and module imports in phase 2.*
6. **Close the way around (7.2) in the browser image?** It touches the
   browser container, though only to keep the agent inside mcp-js. *Default:
   yes, as track G, in phase 1.*
7. **Storage.** *Default: annotations on the Sandbox, 64 KiB a policy.*
   A CRD is the alternative if the spike finds annotations unsuitable.
8. **Process or container restart.** *Default: the supervisor restarts
   `mcp-v8` inside the container (no back-off, no restart count).* A true
   container restart is a one-line variant.
9. **A save the session refuses.** *Default: the save fails as a whole and
   the previous policy stays, for a running session; for a policy saved
   while asleep and refused at wake, MCP stays closed until it is fixed.*
10. **Interrupting running calls.** *Default: confirm in the UI, interrupt
    at once; the API can wait with `?wait=`.*
11. **API host and the allow-list.** A separate `api.` host, or a path on the
    app's host? *Default: separate host.* Mirror Pomerium's allow-list into
    the backend so tokens of removed users stop at once? *Default: yes.*
    Token lifetime? *Default: 90 days, at most a year.*
12. **Sessions that predate the feature** cannot be given a policy.
    *Default: mark them, ask the user to recreate; no migration.*
13. **Terraform shape.** *Default: `browserjs_session` and a separate
    `browserjs_session_policy` whose existence is the managed-as-code mode.*
    Distribution: *in-repo source with `dev_overrides` and a network mirror
    first; registries when someone outside needs it.*
14. **Editor.** Monaco at about 850 KB gzipped on the edit page and the
    create page's split panel only, or CodeMirror 6 at about 120 KB with
    weaker Rego support? *Default: Monaco, bundled (no CDN), lazy-loaded,
    with a Rego grammar written here.*
