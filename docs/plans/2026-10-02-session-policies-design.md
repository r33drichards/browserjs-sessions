# Session policies: design

Status: **approved 2026-10-02** by the product owner. The decisions are
recorded in section 10; nothing is left open. Phase 0 (contracts and the
spikes that need no cluster) is done: the contracts are in
[`docs/contracts/policy/`](../contracts/policy/README.md), the spike results
in section 11, and the phase 1 tracks in
[2026-10-02-session-policies-tracks.md](2026-10-02-session-policies-tracks.md).
No product code is written and nothing is deployed.

> **Note, 2026-10-02 (later the same day): policies are Rego only.** The
> product owner decided to remove the JSON policy format: "remove JSON
> policies and only allow the Rego ones." By then a session had three ways
> in (`browser_execute`, `desktop_execute`, and a shell through the `exec`
> server), and the JSON format of section 3 spoke of the first alone, so
> every JSON policy, the unrestricted one included, denied the desktop.
> Rather than grow the format, it was removed: `SessionPolicy.spec.kind` is
> `rego` only, the translator, the schema, `GET /policy-schema.json`, the
> editor's JSON mode and the provider's `json` argument and
> `browserjs_policy_document` data source are gone, and the presets are
> Rego modules. No migration was needed: enforcement had never been on and
> no `SessionPolicy` existed in production. Sections 3, 5, 6 and 8 below
> describe the JSON form as it was designed and are kept as history; the
> reference is [`contracts/policy/rego-contract.md`](../contracts/policy/rego-contract.md),
> which also covers what was not in this design: a decision module that
> refuses unknown servers and tools, the input of every tool, and the
> warnings for policies whose rules another tool can walk around.

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

## Decisions that shaped the design

Settled by the product owner while this document was being written (the
full record is section 10):

1. **Policies govern mcp-js only.** A person at the VNC view, and where
   Chromium navigates on its own, are out of scope ("the VNC is different and
   that's okay").
2. **Enforcement is OPA-native and shared.** mcp-js asks an OPA server for
   each decision, over the data API it already speaks. That server is **one
   multi-tenant OPA Deployment**, not a sidecar in every pod. A policy change
   takes effect without restarting anything.
3. **State lives in a custom resource, reconciled by an operator.** "Don't
   save state in the backend, but create a CRD for it and use an operator
   with kopf." The backend is a thin client of that resource.
4. **A policy is a sub-resource of one session**, set at creation or later.
   Session creation becomes Cloudscape's single page create; the policy
   follows Cloudscape's sub-resource patterns.
5. The browser MCP server's loopback hole (section 7.2) is **fixed** in
   PR #30, merged.

## Summary of the proposal

```
 UI / Terraform ──▶ backend ──▶ SessionPolicy (custom resource, one per session)
                                      │ watch
                                      ▼
                               policy operator (kopf)
                     validate · JSON→Rego · namespace · build bundle
                                      │ bundle (polled)
                                      ▼
 session pod: mcp-js ──POST /v1/data/browserjs/decision/<session>/mcp_tools──▶ OPA ×2
```

- **`SessionPolicy`**, a namespaced custom resource named after its session
  and owned by the session's Sandbox, holds the policy: kind `json` or
  `rego`, the source, and the management mode. Its `status` carries
  validation errors with line and column, and whether OPA has loaded it.
- **The operator** (Python, kopf) turns every `SessionPolicy` into Rego under
  that session's own package, checks it with the same `opa` binary the
  servers run, and publishes one bundle that the OPA replicas poll. It is
  also the one place policies are validated: the backend's "validate"
  endpoint for the editor calls it.
- **OPA**: a Deployment of two replicas, with a Service, in the sessions'
  own namespace (`browserjs-sessions`) beside the operator, on the system
  pool. Each session's mcp-js is
  configured, from the pod's own name, to ask for
  `browserjs/decision/<session id>/mcp_tools`. Nothing is pushed into a pod,
  so warm-adopted and snapshot-restored pods need nothing done to them. No
  policy loaded for a session means deny.
- **Backend**: creates, reads, updates and deletes `SessionPolicy` objects on
  behalf of the signed-in owner (or their API token), and enforces who may
  write in which management mode. It keeps no policy state.
- **Management mode is per session's policy**: `editor` (Monaco in the UI) or
  `iac` (read-only in the UI, with a link; only API tokens write).
- **Terraform/OpenTofu provider** `browserjs`: `browserjs_session` and
  `browserjs_session_policy`, signing in with backend-issued API tokens.
- **UI**: `/sessions/create` as a single page create with a Policy section;
  the session page becomes a details page with tabs; editing the policy is a
  page edit with Monaco.
- Version 1 governs **the upstream MCP tool calls mcp-js makes**, which in a
  session means the operations of `browser_execute` and their parameters.

One finding to read first: cua-driver's simple format is **YAML, not JSON,
and it is not translated to Rego**; it is a second engine evaluated natively
next to a Rego engine (section 1.1). This design does what the request
describes (JSON compiled to Rego) rather than what cua-driver does.

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
- **What lives where** (relevant to option A in 4.1): heap persistence is off (`MCP_V8_HEAP_STORE=none`).
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

**What exactly mcp-js sends to a remote source** (`OpaClient::evaluate`,
`opa.rs`):

- `POST {url}/v1/data/{policy_path}`, with a trailing `/` trimmed from
  `url`. `url` may therefore carry a path prefix; a query string would be
  broken by the concatenation. `policy_path` is configurable per source and
  defaults to `mcp/tools` for `mcp_tools`.
- Body `{"input": <the category's input document>}`. No headers beyond the
  HTTP client's own; there is no setting for a token or a header.
- The answer is read as `result.allow`. A missing `result` (OPA's answer for
  an undefined document) or a missing `allow` is **false**. A non-2xx status,
  a body that does not parse, or no answer in 5 seconds is an error, and "is
  treated as a policy error, not a permit".
- The input does not say which session is asking. The only thing that can
  differ per session is the URL, and so the path.

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
### 1.7 OPA as a shared service, kopf, and kube-mgmt

OPA (openpolicyagent.org/docs, read through a summarising fetch):

- **Bundles.** OPA polls a bundle service (`min_delay_seconds` /
  `max_delay_seconds`, or long polling with
  `long_polling_timeout_seconds`), sends `If-None-Match` with the last
  `Etag`, and the service answers 304 when nothing changed. A bundle declares
  the `roots` it owns. "If activation fails, OPA maintains its previous
  active bundle and reports errors via the Status API." With `persist`, "OPA
  will attempt to read the bundle from disk on startup".
- **Health.** `/health?bundles` "returns 200 if all configured bundles are
  activated; returns 500 otherwise".
- **Undefined.** "The server returns 200 if the path refers to an undefined
  document. In this case, the response will not contain a `result`
  property", which mcp-js reads as deny.
- **Authorization of the API itself.** `--authentication=token
  --authorization=basic` and a `system.authz` policy whose input has
  `identity`, `method`, `path` (as an array), `headers`, `params`, `body`.
  It has no client address.
- **No server-side query timeout** is documented.
- **Capabilities**: the set of built-in functions a policy may use is a file
  given to `opa check` / `opa build` (and to the Go compiler); a policy that
  uses anything else does not compile.

kopf (docs.kopf.dev):

- Deployment: one replica, `strategy.type: Recreate`. "If two or more
  operators run in the cluster for the same objects, they will collide".
- Peering: operators see each other through `KopfPeering` objects and the
  lower priority pauses; `--standalone` turns that off.
- RBAC it wants beyond the operator's own resources: list and watch on
  `customresourcedefinitions` (cluster), and `events` create.
- Testing: `kopf.testing.KopfRunner` "runs an arbitrary operator in the
  background" but "against the currently authenticated cluster"; KMock
  simulates the API without one.

kube-mgmt (github.com/open-policy-agent/kube-mgmt), considered as prior art:
a sidecar beside OPA that loads policies from ConfigMaps labelled
`openpolicyagent.org/policy=rego` and writes the outcome to an annotation,
`openpolicyagent.org/kube-mgmt-status`. It is not used here because the
product owner chose a CRD and a kopf operator, and because it would not do
the three things this design needs between the stored policy and OPA:
compiling JSON to Rego, rewriting each policy into its session's namespace,
and refusing built-ins. A ConfigMap is also untyped, has no `status`, and
would need the backend to hold `configmaps` rights in a namespace that also
contains Pomerium's configuration.

---

## 2. Data model

### 2.1 `SessionPolicy`

A policy is a sub-resource of a session: it belongs to exactly one session,
is reached through that session in the API and UI, and goes when the session
goes. **Every session has one**, created by the backend with the session. A
session whose owner chose nothing gets the unrestricted policy
(`{"version": 1, "allow": {"operations": ["*"]}}`), which is what a session
does today. There is deliberately no "no policy means allow": no policy
means deny (section 4.5), so a pod with no owner has nothing to be allowed
by.

```yaml
apiVersion: browserjs.dev/v1alpha1
kind: SessionPolicy
metadata:
  name: s-abcde                    # the session's ID; one policy per session
  namespace: browserjs-sessions    # beside the Sandbox, so it can be owned by it
  ownerReferences:                 # deleting the session garbage-collects the policy
    - apiVersion: agents.x-k8s.io/v1beta1
      kind: Sandbox
      name: s-abcde
      uid: …
  annotations:
    browserjs.dev/updated-by: ui   # or "token:ci"
spec:
  sessionRef:
    name: s-abcde
  kind: json                       # json | rego
  source: |
    {"version": 1, "allow": {"operations": ["*"]}, "deny": {"operations": ["evaluate"]}}
  management:
    mode: editor                   # editor | iac
    managedURL: ""                 # required, https, when mode is iac
status:
  observedGeneration: 4
  hash: sha256:9f2c…               # of the Rego that was built for this generation
  rego: |                          # the generated module, for kind json (shown read-only in the UI)
    …
  errors:                          # empty when it compiles
    - {row: 4, col: 21, code: rego_parse_error, message: "…"}
  loaded: {replicas: 2, total: 2, revision: "1837"}
  lastAppliedTime: "2026-10-02T12:01:07Z"
  conditions:
    - {type: Compiled, status: "True",  reason: Compiled, observedGeneration: 4}
    - {type: Loaded,   status: "True",  reason: AllReplicas, message: "2/2 replicas", observedGeneration: 4}
    - {type: Ready,    status: "True",  observedGeneration: 4}
```

- **The manifest** is `deploy/base/crd-sessionpolicy.yaml`; where it and
  the sketch above differ, the manifest is right.
- **Group and version**: `browserjs.dev/v1alpha1` (the group the repo's
  annotations already use). Kind `SessionPolicy`, plural `sessionpolicies`,
  short name `spol`. Namespaced. `status` is a subresource: only the operator
  writes it, and a `spec` change bumps `metadata.generation`, which is the
  policy's version and the `If-Match` value in the API.
- **Validation in the schema** (OpenAPI and CEL, so `kubectl` cannot make an
  impossible object either):
  - `spec.source`: required, `maxLength: 65536`;
  - `spec.kind`: enum;
  - `self.metadata.name == self.spec.sessionRef.name`;
  - `self.spec.management.mode != 'iac' || self.spec.management.managedURL.startsWith('https://')`;
  - `spec.sessionRef` is immutable (`self == oldSelf`).
- **Printer columns**: Session, Kind, Mode, Ready, Loaded (`2/2`), Age.
- **Conditions**: `Compiled` (False with the first error as message; the
  full list is in `status.errors`), `Loaded` (every ready OPA replica serves
  this generation's hash), `Ready` (both). Each carries
  `observedGeneration`, so a reader can tell "ready, for the version before
  your edit" from "ready".
- **A policy that does not compile** stays stored (it is what the author
  wrote) with `Compiled=False`. What is enforced for that session is section
  4.5's business: the last good one keeps being served until it is replaced,
  and a session that never had a good one is denied.
- **Finalizer**: kopf's own, so the operator sees the delete and takes the
  session out of the bundle before the object disappears.

### 2.2 Reuse across sessions

There are no shared policy objects and no account-wide default. Reuse is:

- **By copy, in the UI.** The Policy section of the create page, and the
  policy edit page, offer "Copy from a session", which fills the editor with
  that session's source. The copy is independent from then on.
- **By Terraform.** One `file()` or local value referenced by several
  `browserjs_session_policy` resources, or a `for_each`. This is the answer
  for anyone who wants one policy kept the same across sessions.

### 2.3 Management mode

Scoped to **the session's policy**. Tailscale's switch is per tailnet, and a
tailnet has exactly one policy file, so the switch there is in effect per
policy file; per session's policy is the same thing here. It also lets one
user keep a Terraform-managed session beside a scratch session edited by
hand, which a per-account switch would forbid.

The backend enforces it, because it is about which credential is writing;
the custom resource only records it.

| | `editor`, UI (cookie) | `editor`, API token | `iac`, UI (cookie) | `iac`, API token |
|---|---|---|---|---|
| Read the policy, validate, test | yes | yes | yes | yes |
| Save or reset the policy | yes | **no (409)** | **no (409)** | yes |
| Change `mode` and `managedURL` | yes | yes | yes | yes |
| Rename, stop, resume, delete the session | yes | yes | yes | yes |

- A token's save may carry `management` in the same request, so "take this
  policy over as code" is one call (it is what the Terraform resource does on
  create). A token's save to an `editor`-mode policy without it is refused.
- A refused write answers `409 {"error": "this policy is managed
  externally", "managed_url": …}`, or `"…managed in the editor"`.
- Switching back to `editor` in the UI is always possible. That is the
  escape hatch; there is no "Edit anyway". Terraform sees
  the change as drift on its next plan.
- An MCP client (the agent) can do none of this: the MCP route reaches only
  a session's `/mcp`, never the API.
- Someone with `kubectl` and rights on the resource can write past the
  switch. That is a cluster administrator, and is accepted.

### 2.4 Where state is, and is not

| State | Where |
|---|---|
| The policy, its mode and link | `SessionPolicy.spec` |
| Whether it compiled, the errors, the generated Rego, whether OPA has it | `SessionPolicy.status`, written by the operator |
| What OPA enforces | OPA's memory, rebuilt from the bundle; the bundle is rebuilt from the custom resources |
| API tokens (section 6.2) | `APIToken` custom resources holding a hash |
| In the backend | nothing |

The backend's Role gains verbs on `sessionpolicies` (get, list, create,
update, patch, delete) and on `apitokens`, and nothing else. It never writes
`status`.

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

What an author writes, in either kind, ends as one Rego module with this
contract:

- package `browserjs.policy` (the operator rewrites it to the session's own
  package, section 4.4);
- `allow_tool_call` decides an upstream MCP tool call; undefined or false
  denies. (`allow_fetch` and `allow_module` are reserved for phase 2.)
- `input` is mcp-js's `mcp_tools` document (section 1.3), unchanged.

The translation from JSON is done by the **operator**, in Python, and nowhere
else: the backend's validate endpoint calls the operator (section 4.4), so
the editor, the API, Terraform's plan and the reconcile all run the same
code against the same `opa` binary.

There is no URL parser in Rego. A `hosts` or `schemes` constraint compiles
to one anchored regular expression over the lower-cased URL that only
matches `scheme://host[:port]` followed by `/`, `?`, `#` or the end, with
the host drawn from `[a-z0-9.-]`. A URL with userinfo, a backslash,
whitespace, or a percent-encoded or non-ASCII host does not match and is
denied. That is stricter than a parser, and has no parser to disagree with
Chromium's.

The exact algorithm is `docs/contracts/policy/json-to-rego.md`. The second
example is `examples/one-site.policy.json` there, and compiles to
`examples/one-site.rego`:

```rego
# Generated from a browserjs JSON policy (version 1). Edit the JSON, not this file.
package browserjs.policy

import rego.v1

allow_tool_call if {
	input.server == "browser"
	input.tool == "browser_execute"
	is_array(input.arguments.operations)
	every op in input.arguments.operations {
		operation_allowed(op)
	}
}

allowed_operations := {"click", "press", "screenshot", "select", "setViewport", "url", "wait"}

operation_allowed(op) if {
	op.type in allowed_operations
}

# allow.rules[0]
operation_allowed(op) if {
	op.type == "navigate"
	is_string(op.params["url"])
	regex.match("^(?:https)://(?:(?:[a-z0-9-]+\\.)+example\\.com|example\\.com)(?::[0-9]+)?(?:[/?#].*)?$", lower(op.params["url"]))
}

# allow.rules[1]
operation_allowed(op) if {
	op.type == "type"
	is_string(op.params["text"])
	count(op.params["text"]) <= 500
}
```

The translator is a pure function with table-driven tests: the contract has
five examples, each with the Rego it must produce byte for byte and a list
of inputs with the decision each must get (86 in all, run in spike 7). The UI shows the generated Rego read-only
(`status.rego`) and offers "Convert to Rego", one way.

---

## 4. Enforcement

### 4.1 The options, side by side

Per-session policy has to reach a pod that is usually already running when
it gets an owner (warm pool), and has to be right after a sleep and a
restore from a Pod Snapshot, where processes resume with the memory they
had. A session pod requests 0.2 CPU and 1280 Mi, and memory decides that
nine fit a node (12097 Mi for sessions; `deploy/gke/warmpool.yaml`).

| | A. Save, then restart mcp-js | B. Backend is the decision point | C1. **One shared OPA** (chosen) | C2. OPA sidecar per pod |
|---|---|---|---|---|
| How a change applies | backend pushes a file into the pod and a supervisor restarts `mcp-v8` | next call; the backend evaluates | next call after the replicas poll the bundle (seconds) | next call after the sidecar polls its bundle |
| Restart, and what it loses | yes: `run_js` in flight, client connections | none | none | none |
| Latency per governed call | none (in-process regorus) | one in-cluster round trip | one in-cluster round trip | loopback |
| Fails closed when | the pod's policy is not the stored one: needs a gate in the proxy | backend unreachable or slow: mcp-js denies | OPA unreachable, slow, or has nothing for the session: mcp-js denies | sidecar down or not yet loaded: mcp-js denies |
| Shared fate | none | backend down: no decisions (but no MCP either, it is the proxy) | OPA down: every governed call in every session is denied | none |
| Cost per pod | a supervisor process, about 10 Mi | none | none | about 50m CPU and 64 to 128 Mi (UNVERIFIED): 1344 Mi a session still packs nine a node, with 1 Mi to spare; 1408 Mi packs eight |
| Cost elsewhere | none | owners' Rego runs in the backend process | two OPA replicas on the system pool, about 100m and 256 Mi each | a bundle endpoint every pod polls |
| Warm-adopted pod | must be pushed to and restarted right after adoption | nothing to do | nothing to do | starts polling when it has an owner; denied until loaded |
| Snapshot-restored pod | holds the policy it had; must be compared and re-pushed at every wake | nothing to do | nothing to do | the sidecar resumes with an old bundle until its next poll |
| How a pod is tied to its own session | bundle signed by the backend for that session ID | source address checked against the Sandbox | the path in the pod's own configuration; the pod never fetches a policy, only asks for decisions | the pod fetches a bundle by name; any pod could ask for another's source unless a per-pod secret exists, and a warm pod has none |
| Engine that validates / enforces | OPA / regorus (two dialects) | OPA / OPA | OPA / OPA | OPA / OPA |
| Effort | supervisor, signing, gate, restart handling: the most moving parts | smallest: one handler, one NetworkPolicy rule | operator, bundle service, OPA Deployment, per-tenant namespacing | C1's operator plus a third container in two pod templates and per-pod bundles |

**Chosen: C1**, by the product owner. It has no restart, keeps the backend
out of the request path and the owners' Rego out of the backend process,
costs the session nodes nothing, and needs nothing done to a warm or
restored pod.

**C2 is the documented alternative**, for when shared fate or tenant
isolation matters more than nodes: the same operator, the same custom
resource, per-session bundles in place of one, and a sidecar in the pod
template. Moving from C1 to C2 changes where OPA runs and one URL.

**Rejected**: A, because it interrupts running work to change a rule, needs
a gate to stay correct across sleep and restore, and enforces with a
different engine from the one that validates. B, because it puts every
decision and every owner's Rego in the single backend process; the product
owner prefers OPA's own machinery to a decision endpoint written here.

### 4.2 The request path

```
MCP client ─ Pomerium ─ backend ─ pod: mcp-js ─ run_js ─ mcp.callTool("browser", "browser_execute", …)
                                        │
                                        │ 1. file:///etc/mcp/mcp_tools.rego         (platform layer, in-process)
                                        │ 2. POST http://opa:8181/v1/data/browserjs/decision/s-abcde/mcp_tools
                                        ▼                                {"input": {…}} → {"result": {"allow": true}}
                                    OPA (2 replicas, system pool)
```

**Pod template** (blueprint and warm-pool template). `MCP_V8_POLICIES_JSON`
moves from the image's `ENV` to the pod's env so that it can name the
session. The warm template already sets `SESSION_ID` from the pod's name,
and a warm pod keeps its name through adoption; the cold blueprint has
`{{ .ID }}`:

```json
{
  "mcp_tools":  { "mode": "all", "policies": [
    { "url": "file:///etc/mcp/mcp_tools.rego" },
    { "url": "http://opa.browserjs-sessions.svc:8181",
      "policy_path": "browserjs/decision/$(SESSION_ID)/mcp_tools" } ] },
  "filesystem": { "policies": [ { "url": "file:///etc/mcp/filesystem.rego" } ] }
}
```

The local file stays first: it is the platform's layer (only `browser` /
`browser_execute`), costs nothing, and the owner's policy can only narrow
it, as cua-driver's managed layer works. `filesystem` is unchanged and not
owner-configurable.

### 4.3 How the session is identified (the crux)

mcp-js sends nothing that names the session (section 1.3): no header can be
configured, and the input is the category's document. The one thing that
differs per pod is the **path**, so the session ID goes there, taken from
the pod's own name. Considered and not used:

| Way | Why not |
|---|---|
| A per-session secret in the URL | a warm pod is running before it has a session; its env cannot be changed afterwards, and the secret would have to exist for every pod in the pool |
| A header or token | mcp-js has no setting for one |
| Source address, mapped by something that knows pod IPs | OPA's authorization input has no client address; it would take a proxy in front of OPA, which is option B again |

**The trust argument.** What matters is that a restricted agent cannot get
its own calls judged by a different policy.

- Which path a session's mcp-js asks is fixed in the pod spec, written by
  the Sandbox controller from a template; neither the owner nor the agent
  sets it. The agent's code runs inside `run_js`: it cannot change mcp-js's
  configuration, and in v1 it has no `fetch`, no subprocess, and a
  filesystem of `/data/memory` only. Whether `run_js` can read the process
  environment is UNVERIFIED, and does not matter: the session ID is in the
  session's hostname already.
- The decision for a call is made by mcp-js asking its own path. Nothing an
  agent sends to OPA changes that; OPA's API is read-only to it (below).
- **What an agent could do**: session pods can reach OPA's port (the
  NetworkPolicy allows the pod, and Chromium is in the pod), so a page, or
  an agent allowed `evaluate`, could `POST` to another session's decision
  path and learn, one yes or no at a time, what that session's policy
  allows. Warm-pool IDs are five characters, so they can be guessed. That
  is a disclosure of another user's policy by probing, not a way to act in
  their session or change anyone's enforcement. It is accepted for v1 and
  is one of the things C2 would remove.
- **What it could not do**: read policy sources (`GET /v1/policies` and the
  rest of the API are refused by `system.authz`), write policies or data
  (refused, and the bundle owns those paths), reach the operator's bundle
  endpoint (NetworkPolicy), or reach another session's pod.

**OPA's own API is locked down** with `--authentication=token
--authorization=basic` and a `system.authz` policy mounted from a
ConfigMap:

```rego
package system.authz

import rego.v1

default allow := false

# A session asking for a decision. No identity: mcp-js cannot send one.
allow if {
	input.method == "POST"
	count(input.path) == 6
	array.slice(input.path, 0, 4) == ["v1", "data", "browserjs", "decision"]
	# No ?explain, ?instrument, ?provenance, ?metrics: they describe the policy.
	object.keys(input.params) == set()
}

# Kubelet probes.
allow if {
	input.method == "GET"
	input.path == ["health"]
}

# The operator, reading what a replica has loaded.
allow if {
	input.identity == opa.runtime().env.OPERATOR_TOKEN
	input.identity != ""
	input.method == "GET"
	input.path == ["v1", "data", "browserjs", "loaded"]
}
```

The copy that is deployed is `docs/contracts/policy/system-authz.rego`.
Spike 4 (section 11) ran it against sixteen kinds of request.

**NetworkPolicy**, both ways:

| From | To | Port |
|---|---|---|
| session pods (egress, added) | OPA pods | 8181 |
| OPA (ingress) | from session pods and the operator only | 8181 |
| OPA (egress) | the operator | 8080 (bundles) |
| operator (ingress) | from OPA (bundles) and the backend (validate) | 8080 |
| operator (egress) | the API server; OPA pods | 443; 8181 |
| backend (egress, unrestricted today) | the operator | 8080 |

The session pods' rule is additive to "the internet, but not the cluster":
one more destination, selected by pod label, one port.

### 4.4 Multi-tenancy: how policies become Rego in OPA

For each `SessionPolicy` the operator builds two modules.

**The tenant module**, from the owner's source (compiled from JSON first if
needed). The operator parses it to an AST with `opa parse --format json` and
refuses it unless:

- its package is exactly `browserjs.policy`;
- it has no reference to `data` at all, anywhere (rules in the same package
  are called by their own names, so nothing legitimate needs `data`), and no
  `with`; imports are limited to `rego.v1`, `future.keywords.*` and `input`;
- it compiles under the **capabilities file**, which is an allow-list of
  built-ins, not a deny-list: comparison, arithmetic, strings, regex,
  aggregates, sets, objects, type checks, JSON and base64 codecs, `time`
  reads. Not on it: `http.send`, `net.*`, `opa.runtime`, `rego.*`
  metadata, `trace`, `print`, anything that reads the environment, and the
  generators that turn a small input into a large collection
  (`numbers.range`, `numbers.range_step`);
- its source is within the size limit (64 KiB, also in the CRD schema).

It then replaces the package clause, by its position in the AST, with the
session's own: `package browserjs.tenant["s-abcde"]`. Because the module
cannot name `data`, it cannot read another tenant's rules or the platform's,
whatever package it sits in.

**The decision module**, generated, platform-owned, the "dispatcher" at the
path mcp-js asks:

```rego
package browserjs.decision["s-abcde"].mcp_tools

import rego.v1

default allow := false

allow if data.browserjs.tenant["s-abcde"].allow_tool_call == true
```

It is where the platform can add conditions for everyone later without
touching tenant code, and why a tenant rule that returns something other
than `true` is a deny. One generic dispatcher for all sessions is not
possible, because the session is only in the URL path, which Rego cannot
see. (Session IDs contain a hyphen, so the packages use the bracketed form,
which OPA accepts in a package clause: spike 1.)

Both go into **one bundle** with root `browserjs`, with a data document
`browserjs.loaded` mapping each session ID to its policy hash, and a
manifest `revision`.

**Why one bundle, polled, and not pushes or per-session bundles.**

| | One bundle, polled (chosen) | REST pushes to every replica | Per-session bundles through discovery |
|---|---|---|---|
| New or restarted replica | pulls the whole state itself; not ready until it has (`/health?bundles`) | the operator must notice it and replay everything; until then it serves nothing, or worse, something partial | pulls, one request per session |
| Consistency between replicas | same bundle, same `revision`; each is whole or previous | each `PUT` can fail separately; replicas differ until retried | same per bundle |
| A tenant's bad policy | cannot enter: the operator builds the bundle with `opa build` and leaves a tenant out if it does not compile alone; a bundle that fails anyway is not activated and the previous stays | fails that one `PUT` | fails that one bundle |
| Growth | one download of everything on any change. a thousand sessions at a few KiB each is a few MiB before compression; fine to thousands of sessions | one small request per change | one small download per change, but as many polls as sessions, per replica |
| Operator down | replicas keep the last bundle; changes wait | same | same |

Per-session bundles are what C2 uses, and what C1 would move to if the one
bundle grew past tens of megabytes.

**How long a change takes.** The watch event reaches the operator in well
under a second; building and checking the bundle is a run of `opa build`;
the replicas long-poll the operator, so they fetch as soon as the ETag
changes. Measured in spike 3: 10 to 20 milliseconds from a published bundle
to both replicas with long polling, and 0.2 to 1.8 seconds with the plain
polling it falls back to (`min_delay_seconds: 1`, `max_delay_seconds: 2`).
The operator then reads `browserjs/loaded` from each
ready replica and sets `Loaded` when all report this generation's hash.
Between a save and that moment a call may still be judged by the previous
policy; the API's save waits for `Ready` (up to ten seconds) before
answering, so "saved" in the UI means "in force".

**Limits.** 64 KiB a policy. No recursion exists in Rego, and the
allow-list removes the built-ins that manufacture work, so evaluation cost
is bounded by the size of the input (an MCP request is at most 1 MiB at
Pomerium). OPA has no server-side query timeout; mcp-js gives up after
5 seconds, and whether OPA stops evaluating when the client goes away is
UNVERIFIED. The replicas have CPU limits. A user who sets out to write an
expensive policy can slow decisions for others: users are an allow-list,
this is accepted for v1, and it is the other thing C2 would remove.

### 4.5 Failing closed

| Situation | What OPA answers | Result |
|---|---|---|
| OPA unreachable, erroring, or slower than 5 s | nothing usable | mcp-js denies |
| Warm pod with no owner | no `SessionPolicy`, so no decision module: undefined, no `result` | deny (and nothing can call it anyway) |
| Session just created, bundle not yet polled | undefined | deny for a second or two; the session shows `starting` until `Ready` |
| Session whose `SessionPolicy` was deleted by hand | undefined | deny; the backend recreates nothing on its own |
| Policy saved but it does not compile | the previous good one, if there was one; else undefined | previous policy, or deny; `Compiled=False` with the errors |
| Pod restored from a snapshot | whatever is current for its ID | correct, with nothing done to the pod |
| A replica that has just started | not ready until the bundle is active, so not behind the Service | no partial answers |
| Operator down | replicas keep the last bundle | enforcement continues; changes wait |
| Both replicas down | nothing | every `browser_execute` in every session is denied until one returns |

The last row is the price of sharing. It is kept small by: two replicas
spread over nodes (`topologySpreadConstraints`), a PodDisruptionBudget with
`minAvailable: 1`, readiness on `/health?bundles`, liveness on `/health`, a
`RollingUpdate` with `maxUnavailable: 0`, and `persist: true` on an
`emptyDir` so a restarted container can serve its last bundle while the
operator is away. The backend's readiness page shows it.

**Latency**: one HTTP request inside the cluster and an evaluation of a
small module per `browser_execute` call. Over loopback that is 0.2 ms at
the median and 0.7 ms at p99 (spike 3); in the cluster, add the network.
A browser operation takes tens to hundreds of milliseconds.

### 4.6 The operator

A kopf operator, `policy-operator`, in `images/policy-operator/` (Python
3.12), where `images.yml` already looks for images.

**What it does**

| Handler | Work |
|---|---|
| `on.create`, `on.update` (field `spec`), `on.resume` of `SessionPolicy` | compile (JSON to Rego), check, namespace (4.4); keep the result in memory keyed by session; rebuild and publish the bundle; write `status` (`Compiled`, `errors`, `rego`, `hash`, `observedGeneration`) |
| `on.delete` (kopf's finalizer) | remove the session from the bundle, publish |
| a timer per resource, and a watch on OPA's EndpointSlice | ask each ready replica what it has loaded; write `Loaded`, `Ready`, `loaded.replicas`, `lastAppliedTime` |
| HTTP, port 8080 | `GET /bundles/browserjs.tar.gz` (ETag, long polling) for OPA; `POST /validate` and `POST /evaluate` for the backend |

- **One implementation of "is this policy valid".** `POST /validate` runs
  the same function the reconcile runs and returns the same `errors` and
  `rego` that would land in `status`, without storing anything. The backend
  calls it for the editor's diagnostics, the API's validate, and
  Terraform's plan. `POST /evaluate` runs `opa eval` on a source and a
  sample input for the editor's Test. The backend has no Rego code and does
  not import OPA.
- **The same OPA.** The operator image contains the `opa` binary at the
  version the Deployment runs, pinned together, and the capabilities file.
  There is one engine in the whole design.
- **Idempotent, and stateless across restarts.** Everything in memory is
  derived from the custom resources. On start, kopf's `on.resume` runs for
  every existing object and the index is rebuilt. Until that first pass is
  complete the bundle endpoint answers `503`, so a restarting operator
  never publishes a bundle with sessions missing; the replicas keep what
  they have.
- **One active operator.** One replica, `strategy: Recreate`, `--standalone`,
  `--namespace browserjs-sessions`. That is what kopf's documentation
  recommends, and it is enough: the operator being away delays changes and
  never weakens enforcement. kopf's peering (`KopfPeering`) is the route to
  a standby if that is ever wanted.
- **Admission webhook, later.** kopf can also serve a validating webhook,
  which would refuse an invalid `SessionPolicy` at `kubectl apply` time.
  It needs a certificate and a webhook configuration; the validate endpoint
  covers the product's own paths without it.

**RBAC** (ServiceAccount `policy-operator`):

| Scope | Rule |
|---|---|
| Role, sessions namespace | `sessionpolicies`: get, list, watch, patch; `sessionpolicies/status`: patch |
| | `events`: create |
| | `endpointslices` (`discovery.k8s.io`): get, list, watch (to find OPA's replicas) |
| ClusterRole | `customresourcedefinitions`: list, watch (kopf requires it) |

It cannot read Secrets, Sandboxes, or anything of Pomerium's.

**Packaging and deployment**

- Image `policy-operator`: a Dockerfile on a `python` base pinned by digest,
  dependencies from a lock file with hashes, the `opa` binary copied from
  the pinned OPA image. Built by `images.yml` and pinned in the kustomize
  `images:` block by `hack/pin-images.sh`, like the other two. A Nix
  dev shell entry for running its tests locally.
- `deploy/base`: the CRD, the operator's ServiceAccount, Role, ClusterRole
  and bindings, its Deployment and Service; the OPA Deployment, Service,
  PodDisruptionBudget, `system.authz` ConfigMap, the operator-token Secret
  (in `secrets.example.yaml`); the NetworkPolicy rules of 4.3; the backend
  Role's new verbs.
- `deploy/gke`: node selector and tolerations for the system pool on both
  Deployments, the images by digest, and the env change in `blueprint.yaml`
  and `warmpool.yaml` (with `deploy_test.go` keeping the two in step).
- `deploy/local`: the same on kind, one OPA replica.

**Testing**

| Level | What | Needs |
|---|---|---|
| Unit | the translator (golden Rego, decisions through `opa eval`), the AST checks and package rewrite (a corpus of hostile modules: `data` references, `with`, forbidden built-ins, wrong package), bundle building, the status computation. All plain functions that take a spec and return a result | Python and the `opa` binary |
| Handler | the kopf handlers called directly with fake `spec`, `status`, `patch` objects; the HTTP endpoints with aiohttp's test client | nothing else |
| API simulation | create, update, delete, operator restart with existing objects, against KMock | no cluster |
| Integration | `kopf.testing.KopfRunner` with a real OPA, on kind: a policy is enforced, an edit applies, a bad policy keeps the previous one, a new OPA pod loads everything, a deleted session's policy leaves the bundle | kind (in CI) |
| End to end | through the backend and a session pod: a denied operation is denied; a warm-adopted session gets its policy; a session restored from a snapshot is judged by the current policy | kind; staging for snapshots |

### 4.7 Creating a session with a policy

1. Cold session: the backend creates the Sandbox, then the `SessionPolicy`
   named after it, owned by it. Warm session: the claim binds a Sandbox;
   the backend creates the `SessionPolicy` for that Sandbox's name, then
   writes the owner onto the Sandbox as today. Until the owner is written
   nobody can use the session, so there is no moment when it is usable and
   unrestricted.
2. The claim carries the intended policy in an annotation, as it carries the
   owner, so `RecoverClaims` can finish after a crash without replacing a
   restrictive policy with the default.
3. The session's state stays `starting` until its policy is `Ready`. A
   policy that does not compile fails the creation: the backend validates
   first, and deletes what it made if the operator still says no.
4. A `SessionPolicy` is never created for a session that predates the
   feature (below).

### 4.8 Sessions that exist before this ships

A Sandbox's pod template is fixed when it is created, and a snapshot
restores the old process. Sessions created before the rollout have the
image's static configuration and never ask OPA. The API reports their policy
as `unsupported`, saving one is refused, and the UI says "created before
policies; recreate the session to give it one". The warm pool replaces its
waiting pods on a template change (`updateStrategy: Recreate`), so new
sessions are covered from the deploy on.

### 4.9 Not covered

- Where the browser goes, and the person at the VNC view: out of scope by
  decision 1.
- `fetch`, WebSocket and module imports from `run_js`: off today, off in v1.
  Phase 2 adds `allow_fetch` and `allow_module` the same way (a second
  decision module and one more remote source in the template), with a
  platform file in front that denies loopback, private and cluster
  addresses, the OPA Service among them.
- Subprocess, the filesystem outside `/data/memory`, `run_js_file`: the
  platform's, not offered to owners.
- The top-level MCP tools (`run_js` itself, artifacts): not an mcp-js policy
  category.
- A list of recent decisions in the UI. OPA can log every decision; turning
  that into a per-session view (and masking what agents typed) is phase 2.

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
| (•) No restrictions            the unrestricted policy                   |
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
- Behind every choice is the same thing: a `SessionPolicy` for the new
  session. "No restrictions" is the unrestricted JSON policy, not the
  absence of one.
- Create with the panel open and unconfirmed changes in it, or Cancel with
  anything entered, raises the Leave page modal.
- On success: the session's details page with a flashbar, "Session
  brave-otter created", and the state `starting` until the policy is loaded
  (a second or two). A policy that does not compile fails the creation and
  returns to the form with the errors on the Policy section.

### 5.2 Session details (`/sessions/:id`)

```
browserjs sessions > brave-otter

brave-otter                        [ Stop ] [ Rename ] [ Delete ]
+-- Summary ---------------------------------------------------------------+
| State  running      MCP URL  https://s-abcde.sessions…/mcp  [copy]       |
| Created  2 h ago    Policy   JSON, v3, in force                          |
+--------------------------------------------------------------------------+
[ Browser ] [ Policy ]

Policy                                  [ Copy from session ] [ Reset ] [ Edit ]
+--------------------------------------------------------------------------+
| Kind  JSON     Version  3     Status  in force (2/2)   sha256  9f2c…     |
| Managed in  this editor                                    [ Change ]    |
| Last saved  2 h ago, in the UI                                           |
+-- policy.json (read-only) ------------+-- Generated Rego ----------------+
| {                                     | package browserjs.policy         |
|   "version": 1,                       | import rego.v1                   |
|   …                                   | …                                |
+---------------------------------------+----------------------------------+
```

- The Browser tab is today's page (the VNC pane). The Policy tab shows the
  policy read-only. The unrestricted policy is shown as what it is, with a
  line above it: "No restrictions: an agent may use every browser
  operation". "Reset" returns to it.
- Everything on the tab is read from the `SessionPolicy`: the status line
  from its conditions (`in force (2/2)`; `loading…` while `Loaded` is not
  yet true for this version; an error alert with the messages and "Edit"
  when `Compiled` is false), the generated Rego from `status.rego`.
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
                                                    [ Cancel ]  [ Save ]
```

- The format switch on a new policy picks the kind. On an existing JSON
  policy, Rego shows the generated module with "Convert to Rego (cannot be
  undone)". In Rego there is one pane.
- JSON problems come from the schema in the browser as you type; all other
  problems from the server's `validate`, debounced, which is the operator's
  own check, so what the editor shows is what a save would get.
- Test runs the policy against a sample call with the same OPA and the same
  restrictions as the real thing.
- **Save** needs no confirmation: nothing restarts and no running call is
  interrupted. It returns to the Policy tab with a flashbar, "Policy saved
  and in force (v4)", or, if the replicas have not all loaded it within ten
  seconds, "Policy saved; loading". Errors keep the user on the page with
  the markers in the editor. The session need not be running.
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
| Kind  Rego   Version  7   Status  in force   Last saved  by token "ci"   |
```

- Edit, Reset and Copy from session are absent. The source and generated
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
Each is a thin translation to the custom resource, after the owner check the
backend already does for sessions.

| Method and path | Purpose | On the cluster |
|---|---|---|
| `POST /sessions` | accepts `policy: {kind, source}` and `policy_management: {mode, managed_url}` | creates the Sandbox and its `SessionPolicy` (4.7) |
| `GET /sessions`, `GET /sessions/{id}` | add `policy: {kind, version, hash, status, management}`, without the source | one list of `sessionpolicies` by owner label |
| `GET /sessions/{id}/policy` | the whole policy: `source`, `rego`, `errors`, `status` (`ready`, `pending`, `invalid`, `unsupported`), `loaded` | get |
| `PUT /sessions/{id}/policy` | `{kind, source, management?}`. Validates through the operator first; then updates `spec` and waits up to 10 s for `Ready` at the new generation. `If-Match: <version>` optional. `200` in force, `202` saved and not yet loaded, `409` wrong mode, `412` version, `422` invalid, with `errors[]` | update |
| `DELETE /sessions/{id}/policy` | resets to the unrestricted policy and `editor` mode | update (the object stays; no policy would mean deny) |
| `PUT /sessions/{id}/policy/management` | `{mode, managed_url}` | patch |
| `POST /policies/validate` | `{kind, source}` → `{ok, rego?, errors[], warnings[]}`; saves nothing | operator `POST /validate` |
| `POST /policies/evaluate` | `{kind, source, input}` → `{allow}`; saves nothing | operator `POST /evaluate` |
| `GET /policy-schema.json` | the JSON Schema of the JSON format | served from the operator's copy |
| `GET /tokens`, `POST /tokens`, `DELETE /tokens/{id}` | cookie only; the token is shown once, on creation | `apitokens` |

`version` is the resource's `metadata.generation`. Errors keep the existing
shape, `{"error": "…"}`, plus `errors[]` with `row`, `col`, `code`,
`message`. An OpenAPI document for all of it is the first deliverable of the
plan (section 9).

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
- **Storage**: an `APIToken` custom resource (`browserjs.dev/v1alpha1`) per
  token: owner, name, scopes, expiry, and the hash. No operator reconciles
  it; it is storage with a schema, in keeping with "no state in the
  backend", and the backend's Role names that resource and nothing broader.
  The schema is `deploy/base/crd-apitoken.yaml`.
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
- which policy judges a session is fixed by the pod's configuration, and
  OPA's API gives a session pod nothing but decisions (4.3);
- the session pods have no Kubernetes credentials
  (`automountServiceAccountToken: false`), so the custom resources are out
  of their reach.

And some things stay outside the owner's control whatever they write:

- the platform layer: only the `browser` server and `browser_execute`,
  filesystem only under `/data/memory`, no subprocess. The owner's policy is
  intersected with it and can only narrow;
- the decision module and the capabilities allow-list (4.4);
- isolation between sessions and users (NetworkPolicy, a host per session,
  gVisor): not expressible in a policy;
- who may sign in.

### 7.2 The way around the policy, closed in PR #30

The browser MCP server on `:8081` used to serve any caller, and Chromium
shares the pod's loopback: an agent allowed `navigate` and `evaluate` could
open a page on that origin and call `browser_execute` directly, past mcp-js
and so past any policy. PR #30 (merged) makes the server refuse requests
that carry `Origin` or `Sec-Fetch-*` headers, a non-JSON content type or a
non-loopback `Host`. This design depends on that fix and treats it as done.
mcp-js's own port, `:8080`, is also on loopback and checks no token; calls
made that way are still judged by the policy, so it is not a way around.

### 7.3 Tenants sharing one OPA

- **Isolation of code**: a tenant module cannot name `data`, so it cannot
  read another tenant's rules; it lives in its own package, so it cannot
  redefine anyone's; the bundle is built by the operator, so nothing a
  tenant writes chooses its package. The corpus of hostile modules in the
  operator's tests (4.6) is the evidence for this and should be reviewed as
  security-critical code.
- **Isolation of effect**: no network or environment built-ins exist for
  tenant code. A tenant's rules run only when its own session asks.
- **Not isolated**: CPU. An expensive policy slows the replicas for
  everyone (4.4). And decisions: any session pod can ask for another
  session's decisions by ID (4.3).
- **Capabilities are not enforced by the OPA server** (spike 5): `opa run`
  would load a module that calls `http.send`. They hold because the
  operator is the only thing that builds a bundle, and OPA's API accepts no
  policy from anyone, the operator included (spike 4).
- **Query parameters on a decision request are refused** (spike 4):
  `?explain=full` returned the evaluation trace, which shows another
  tenant's rules.
- **The bundle** contains every tenant's Rego. Only OPA can fetch it
  (NetworkPolicy), and it can additionally be signed and require a bearer
  token; both are small and are in the plan.
- **OPA's token** for the operator is a Secret; the session pods cannot read
  Secrets and cannot use the API paths it opens anyway.

### 7.4 The operator and the custom resource

- The operator holds no credentials of value: rights on `sessionpolicies`
  and read on OPA's endpoints. Compromising it lets an attacker publish any
  bundle, which is every session's policy; it is small, has two inbound
  callers (OPA, the backend), and runs untrusted text only through
  `opa parse`, `opa build` and `opa eval` as subprocesses with a timeout
  and a size bound.
- The validate and evaluate endpoints run user text on demand. They are
  reachable only from the backend, are bounded the same way, and
  `evaluate` uses the same capabilities, so a Test cannot reach the network
  either.
- The backend can now write `sessionpolicies`. It could already create and
  delete every session, so this adds no power over users it did not have.
- Anyone with rights on the resource in the cluster can read and change
  every policy. Today that is the cluster's administrators.

### 7.5 Other points

- **URLs in policies** are matched by an anchored expression that refuses
  anything unusual (section 3.3), rather than parsed. Unit tests carry the
  known confusions (userinfo, backslashes, whitespace, IDNs, trailing dots).
- **`managedURL` is shown as a link.** `https` only (enforced in the CRD),
  rendered as text with `rel="noopener noreferrer"`.
- **Decisions travel in clear text** inside the cluster, as the backend's
  traffic to the pods already does.
- **Admins** can read every session's policy, as they can already see every
  session.
- **Fail closed** everywhere (4.5).

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
  it resets the session to the unrestricted policy and to `editor`. "Managed as
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
unrestricted policy. Nothing has the session's MCP URL before the apply returns,
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

An apply that changes a policy restarts nothing. The resource waits for the
policy to be in force before it reports success, and fails the apply with
the compile errors, line and column, when it is not valid (which the plan
will usually have caught already).

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

**Phase 0 is done** in the pull request that approved this document: the
contracts are in [`docs/contracts/policy/`](../contracts/policy/README.md)
and the two CRD manifests in `deploy/base/`, and the spikes that need no
cluster are in section 11.

**Phase 1** is six tracks that can be built at the same time by separate
agents, each against the contracts and none against another's code. What
each owns, consumes, must not touch, and its definition of done:
[2026-10-02-session-policies-tracks.md](2026-10-02-session-policies-tracks.md).

| Track | Scope |
|---|---|
| A. Operator | `images/policy-operator/`: translator, tenant checks, bundle builder and server, kopf handlers, status, validate and evaluate |
| B. OPA and deploy | the CRDs in kustomize, OPA, the operator's manifests and RBAC, NetworkPolicy, the pod template env, overlays, image build and pinning |
| C. Backend | the `SessionPolicy` client, mode rules, the handlers of `backend-api.yaml`, create-with-policy |
| D. UI | create page, details tabs, policy edit page with Monaco, managed-as-code state |
| E. API tokens | `APIToken`, the token page's API, the `api.` host, the bearer authenticator |
| F. Terraform provider | `terraform-provider-browserjs/` |

**Phase 2**

- `fetch`, WebSocket and module imports as policy sections.
- Recent decisions per session, from OPA's decision log, and denials that
  explain themselves to the agent (a `pre` hook that returns a `reason`).
- A validating admission webhook from the operator; bundle signing.
- Provider release pipeline, the registry mirror repo, the registries.
- The sidecar variant (C2), if shared fate or tenant isolation asks for it.

---

## 10. Decisions

Approved by the product owner on 2026-10-02. Nothing is left open; this is
the record.

| Question | Decision |
|---|---|
| What policies govern | mcp-js only. In v1, browser actions: the operations of `browser_execute`. `fetch` and module imports in phase 2. The VNC view and Chromium's own navigation are out of scope |
| Enforcement | one shared multi-tenant OPA Deployment that mcp-js asks; its trade-offs (decisions of another session can be probed by ID; CPU is shared between tenants) are accepted for v1 |
| State | `SessionPolicy` custom resources, reconciled by a kopf operator; none in the backend |
| Policy kinds | `json` compiles to Rego; `rego` is used as written, after the tenant checks. JSON only, no YAML |
| Every session has a `SessionPolicy`; none means deny | yes |
| A policy that does not compile | refused by the API before saving; if one arrives anyway the previous good policy stays in force and `status` shows the errors |
| How a save answers | waits up to 10 seconds for every replica, then 200; otherwise 202 |
| Managed-as-code lock | hard: no "Edit anyway". The mode is switched back to the editor in the browser. A token cannot write an editor-mode policy unless the same request takes it over |
| Mode scope | per session's policy |
| Reuse | by copy in the UI, or by Terraform. No shared and no default policy |
| Sessions that predate the feature | marked `unsupported`; recreate |
| API tokens | backend-issued, on an `api.` host; `APIToken` custom resources holding a hash; 90 days by default, a year at most; the backend mirrors Pomerium's allow-list |
| Terraform | `browserjs_session` and a separate `browserjs_session_policy`; built in this repo and installed locally (`dev_overrides`) first, registries later |
| Editor | Monaco, bundled (no CDN), lazy-loaded, with a Rego grammar written here |
| API group | `browserjs.dev` |
| Browser MCP loopback hole | fixed in PR #30 |

---

## 11. Phase 0 spikes

Run locally on 2026-10-02 with OPA 1.9.0 (`Rego Version: v1`,
darwin/arm64), no cluster. The scripts are in
[`docs/contracts/policy/spike/`](../contracts/policy/spike/).

Getting OPA: `nix shell nixpkgs#open-policy-agent` did not work. On this
machine the package is built from source and its test phase fails
(`v1/server/compile_handler_test.go: undefined: fixture`), on nixos-25.05
(1.6.0) as well. It builds with the tests skipped:

```
nix build --impure --expr '(builtins.getFlake "nixpkgs").legacyPackages.${builtins.currentSystem}.open-policy-agent.overrideAttrs (_: { doCheck = false; })' -o opa-result
```

Track A's dev shell and CI should take the binary from the pinned OPA
image or a release download instead.

### Spike 1: session IDs in package names and paths

Modules with `package browserjs.tenant["s-ab2cd"]`,
`package browserjs.decision["s-ab2cd"].mcp_tools`, and the same for
`s-abcdefghij`.

| Command | Result |
|---|---|
| `opa check <dir>` | accepted |
| `opa check --capabilities capabilities.json <dir>` | accepted |
| `opa eval -d <dir> -i in_ok.json 'data.browserjs.decision["s-ab2cd"].mcp_tools'` | `{"allow":true}` |
| the same with an `evaluate` operation | `{"allow":false}` |
| `POST /v1/data/browserjs/decision/s-ab2cd/mcp_tools` on a server | `{"result":{"allow":true}}` |
| `opa fmt` on the tenant module | keeps `package browserjs.tenant["s-ab2cd"]` |

**The bracketed, hyphenated form works; the fallback (generated
identifiers) is not needed.** `opa parse --format json` gives the package
path as terms (`data`, `browserjs`, `tenant`, `s-ab2cd`), which is what the
rewrite and the checks read.

### Spike 2: an undefined decision

Two `opa run --server` processes with `system-authz.rego` and
`opa-config.yaml` (addresses changed to loopback), and `bundle-stub.py`.

| Situation | Answer to the decision POST |
|---|---|
| before any bundle is active | `200 {}` |
| a session that is not in the bundle (`s-zzzzz`) | `200 {}` |
| a session in the bundle, allowed call | `200 {"result":{"allow":true}}` |
| a session in the bundle, denied call | `200 {"result":{"allow":false}}` |

`{}` has no `result`, which mcp-js reads as deny (`opa.rs`: `.result
.and_then(|r| r.allow).unwrap_or(false)`). **A missing tenant is denied**,
as designed.

### Spike 3: bundle polling, activation, health

| Situation | Result |
|---|---|
| bundle server answers 503, OPA just started | `/health` 200, `/health?bundles` **500** |
| after the first bundle is active | `/health?bundles` 200 |
| a bundle that does not parse is published | OPA logs "Bundle load failed", keeps serving the previous bundle, `/health?bundles` stays 200 |
| the bundle server is stopped | decisions continue from the loaded bundle; `/health?bundles` stays 200 |

Time from publishing a new bundle to **both** replicas reporting its hash
(`GET /v1/data/browserjs/loaded`), six runs each:

| Polling | Seconds, per replica |
|---|---|
| `min_delay_seconds: 1`, `max_delay_seconds: 2` | 0.19 to 1.84 |
| the same plus `long_polling_timeout_seconds: 30`, stub answering `Content-Type: application/gzip` | 0.61 to 1.80: **long polling was not used** |
| the same, stub answering `Content-Type: application/vnd.openpolicyagent.bundles` | first activation 0.57 and 0.90; afterwards **0.010 to 0.019** |

**Changes to the design**: the bundle endpoint must answer with
`Content-Type: application/vnd.openpolicyagent.bundles`, or OPA silently
falls back to its polling delays (now in `operator-api.yaml`). With that,
a change is active on both replicas in tens of milliseconds, and the
"one to three seconds" of section 4.4 is the fallback, not the norm.

Also measured: a decision over loopback, a new connection each time, 500
requests: p50 0.22 ms, p95 0.39 ms, p99 0.67 ms. Each idle `opa run
--server` process: 33 to 36 MiB resident. In the cluster, add the network.

### Spike 4: `system.authz`

`--authentication=token --authorization=basic`, the policy in
`system-authz.rego`, the operator's token in the environment variable
`OPERATOR_TOKEN`.

| Request | Status |
|---|---|
| `POST /v1/data/browserjs/decision/s-ab2cd/mcp_tools` | 200 |
| the same with `?explain=full` | **401** (after the change below) |
| `GET` on a decision path | 401 |
| `POST /v1/data/browserjs/tenant/s-ab2cd` | 401 |
| `POST /v1/data/browserjs/decision/s-ab2cd` (five segments) | 401 |
| `POST /v1/data` | 401 |
| `GET /v1/policies` | 401 |
| `PUT /v1/policies/x` | 401 |
| `PUT /v1/data/browserjs/loaded` | 401 |
| `POST /v1/query` | 401 |
| `POST /` | 401 |
| `GET /metrics` | 401 |
| `GET /v1/data/browserjs/loaded`, no token or a wrong one | 401 |
| `GET /v1/data/browserjs/loaded`, the operator's token | 200 |
| `PUT /v1/policies/x`, the operator's token | 401 |
| `GET /health`, `GET /health?bundles` | 200 |

**Change to the design**: the first version of the policy allowed any
anonymous POST on a decision path, and `?explain=full` on such a request
returned the evaluation trace, which shows the tenant's rules. That would
have let any session pod read the logic of another session's policy. The
policy now refuses decision requests that carry any query parameter. The
token is read with `opa.runtime().env.OPERATOR_TOKEN` rather than from a
data document.

### Spike 5: the capabilities allow-list

`capabilities.json`: 112 of OPA 1.9.0's 201 built-ins.
`opa check --capabilities capabilities.json` on a module that uses each of
the following fails with `rego_type_error: undefined function …`:
`http.send`, `net.lookup_ip_addr`, `opa.runtime`, `numbers.range`, `print`
(reported as `internal.print`), `rego.metadata.rule`, `trace`,
`crypto.sha256`, `walk`. All five example policies pass under it.
`opa check --format json` gives each error as `{message, code, location:
{file, row, col}}`, which is the shape `status.errors` and the API use.

**Finding**: `opa run` has no capabilities option, so the OPA server would
load a module that calls `http.send`. The restriction is enforced where
bundles are built (the operator, `opa build --capabilities`) and by OPA's
API accepting no policies from anyone (spike 4). Section 7.3 says so.

### Spike 6: the tenant checks

`tenant-guard.py`, a sketch of checks 1 to 3 of `rego-contract.md` on the
AST from `opa parse --format json`:

| Module | Verdict |
|---|---|
| a rule calling a helper in its own package | accepted |
| `data.browserjs.tenant["s-other"].allow_tool_call` | refused: reference to data |
| `d := data; d.browserjs` | refused |
| `import data.browserjs.tenant as t` | refused |
| `helper with input as {…}` | refused: with |
| `helper with data.x as 1` | refused |
| `package browserjs.decision["s-other"].mcp_tools` | refused: package |
| `package system.authz` | refused: package |
| `data` inside a comprehension | refused |
| a rule head `data.x := 1` | refused |
| `input["data"] == 1` (a string, not the document) | accepted |

### Spike 7: the examples

`run-cases.py` over the five examples: **86 of 86 cases pass**, each
evaluated through a tenant package and the decision module under the
capabilities file. The cases include the URL confusions of section 7.5
(userinfo, backslash, whitespace, line break, percent-encoded and
non-ASCII hosts, trailing dot, suffix and prefix tricks), all denied.

### Not done here, because they need a cluster

They are the first steps of track B, and are listed in the tracks
document: the real mcp-js image asking a real OPA with `policy_path` built
from `$(SESSION_ID)`; what an agent sees on a denial and on a timeout; a
gVisor session pod reaching the OPA Service through the new NetworkPolicy
rule; a restore from a snapshot followed by a decision; the CRDs accepted
by an API server (their CEL rules have only been read, not run).
