# Rego contract

## What a tenant policy is

One Rego module (Rego v1 syntax), at most 65536 bytes, which:

- declares exactly `package browserjs.policy`;
- defines `allow_tool_call`. The call is allowed only when it evaluates to
  `true`; undefined, `false`, or any other value denies;
- may define any other rules and functions in that package for its own use.

For `kind: json` the module is the output of `json-to-rego.md`. For
`kind: rego` it is `spec.source` as written. Both go through the same checks.

Reserved rule names, which a v1 policy may define but nothing reads:
`allow_fetch`, `allow_module`.

## The input

mcp-js v0.21.0-rc.3 builds it in `server/src/engine/mcp_client.rs`
(`McpToolPolicyInput`), for each `mcp.callTool(server, tool, arguments)` made
from `run_js`:

| Field | Value |
|---|---|
| `operation` | always `"mcp_call_tool"` |
| `server` | the upstream server's name; in a session, `"browser"` |
| `tool` | the tool's name; in a session, `"browser_execute"` |
| `arguments` | the arguments object as the agent's code passed it, or `null` when it passed none |

For `browser_execute`, `arguments` is `{operations: [{type, params?}], tab?,
close?}` (`images/browser/browser/server.js`). Nothing validates it before
the policy sees it: every field can be missing or of any type.
`input-sample.json` is a sample; the `examples/*.cases.json` files hold many
more, hostile ones included.

There is nothing in the input that identifies the session, the user or the
MCP client.

## How mcp-js asks

`POST {url}/v1/data/{policy_path}` with body `{"input": <input>}`, no
headers of its own, 5 second timeout (`server/src/engine/opa.rs`). It allows
only when the answer is 2xx and `result.allow` is `true`. A missing
`result`, a non-2xx status, an unparsable body or a timeout denies.

In a session pod:

- `url`: `http://opa.browserjs-sessions.svc:8181`
- `policy_path`: `browserjs/decision/<session id>/mcp_tools`

so the request is
`POST http://opa.browserjs-sessions.svc:8181/v1/data/browserjs/decision/<session id>/mcp_tools`.

## What is in OPA

| Document | Owner | Content |
|---|---|---|
| `data.browserjs.tenant["<session id>"]` | generated from the tenant's module | the module, with its package clause replaced |
| `data.browserjs.decision["<session id>"].mcp_tools` | platform | `decision-module.rego.tmpl` with `{{SESSION_ID}}` replaced; its `allow` is what mcp-js reads |
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
