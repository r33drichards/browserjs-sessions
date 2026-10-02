# Session policies: phase 1 tracks

Six tracks, one agent each, built at the same time. Design:
[2026-10-02-session-policies-design.md](2026-10-02-session-policies-design.md).
Contracts: [../contracts/policy/](../contracts/policy/README.md).

## Rules for every track

- **Build against the contracts, not against another track's branch.** Where
  a track needs something another track makes, it uses a fake of the
  contract (named below) until both are merged.
- **A contract is changed only in its own pull request**, which says which
  tracks it affects. If a contract is wrong or silent, stop and raise it;
  do not work around it in code.
- **Stay inside the files the track owns.** The few files two tracks must
  both edit are listed under "Shared files", with who adds what.
- One pull request per track (more if it helps review), tests included,
  no `Co-Authored-By` trailer, nothing merged or deployed by the agent.
- Rust is not involved. Go and Node as the repo already does them; the
  operator's Python and the `opa` binary through the Nix dev shell.

## Open pull requests that change nearby code

Both are unmerged at the time of writing. Rebase on `main` before opening a
pull request, and expect these to have landed or to land under you.

| PR | Changes | Tracks affected |
|---|---|---|
| #28, path-based session URLs (`https://sessions.<domain>/<id>/mcp`) | `backend/cmd/server/main.go`, `backend/internal/config`, `backend/internal/proxy`, `backend/internal/sessions/urls.go` and `deploy_test.go`; every `blueprint.yaml`, `warmpool.yaml` (the `MCP_V8_PUBLIC_URL` lines), every `pomerium-config.yaml`, `deploy/gke/certificate.yaml`, both `kustomization.yaml`, `patch-backend.yaml`; `infra/main/edge.tf`; `web/src/api.ts` | B (pod templates: the new env var goes directly below `MCP_V8_PUBLIC_URL`, which #28 rewrites), C (`main.go`, `config.go`), D (`api.ts`), E (Pomerium routes, certificate, DNS) |
| #31, docs site and a `site` image | `.github/workflows/images.yml`, `hack/pin-images.sh`, `deploy/gke/certificate.yaml`, `infra/main/edge.tf`, a Pomerium route, `site/` | B (`images.yml`, `pin-images.sh`: add the operator beside `site`, do not reorder), E (certificate, DNS) |

To keep the overlap small: put new code in new files, and make each edit to
a file these PRs touch as small as it can be.

## Shared files

| File | Who adds what |
|---|---|
| `backend/internal/auth/auth.go` | The `Token` field of `auth.User`, exactly as below. Whichever of C and E lands first adds it; the other finds it there. E owns everything else in the package. |
| `backend/cmd/server/main.go`, `backend/internal/config/config.go` | C: `POLICY_OPERATOR_URL`, `OPERATOR_API_TOKEN` and the wiring of the policy package. E: `API_URL`, `ALLOWED_EMAILS` and the API host's handler. Separate blocks; no refactoring of what is there. |
| `backend/internal/api/api.go` | C: `policy` on the session view, `policy` in the create body, registration of the policy routes (handlers live in C's own file). E: nothing; its routes register from its own package. |
| `deploy/base/backend.yaml` | B: the Role's new rules (both resources, as `deploy.md` lists them) and the env vars `POLICY_OPERATOR_URL`, `OPERATOR_API_TOKEN`. E: the env vars `API_URL`, `ALLOWED_EMAILS`. |
| `deploy/base/kustomization.yaml` | B: everything it adds, including `crd-apitoken.yaml`. E: nothing. |
| `docs/` | Each track adds its own page; nobody edits the design or the contracts. |

```go
// In auth.User. Nil when the caller signed in through Pomerium (the UI).
Token *TokenInfo

// TokenInfo is the API token a request was made with.
type TokenInfo struct {
	Name   string   // what its owner called it
	Scopes []string // "sessions:read", "sessions:write", "policies:read", "policies:write"
}
```

A request made with a token never has `Admin` set.

---

## Track A: policy operator

**Owns**: `images/policy-operator/` (all of it: `Dockerfile`, the Python
package `policy_operator`, `tests/`, lock file), and `docs/policy-operator.md`.

**Consumes**: `rego-contract.md`, `json-to-rego.md`, `json-policy.schema.json`,
`examples/`, `decision-module.rego.tmpl`, `capabilities.json`,
`operator-api.yaml`, `deploy/base/crd-sessionpolicy.yaml`, the operator
column of `deploy.md`.

**Builds**

- `translate(policy) -> (rego, warnings)`: `json-to-rego.md`.
- `check(kind, source, session_id?) -> Validation`: schema, translation,
  the six tenant checks, hash. The one function behind the reconcile and
  `POST /v1/validate`.
- The bundle builder (`opa build` under the capabilities file, the layout
  of `rego-contract.md`, last-good fallback from `status.rego`) and the
  bundle endpoint (ETag, 304, long polling, the bundles content type, 503
  until the first pass is complete).
- kopf handlers: create, update and resume of `SessionPolicy`; delete; the
  loaded check against every ready OPA replica (EndpointSlices of Service
  `opa`); `status` exactly as the CRD describes it, conditions with
  `observedGeneration` and `lastTransitionTime`.
- `POST /v1/evaluate`, `GET /v1/schema`, `/healthz`, `/readyz`, bearer
  tokens.
- The image: Python base pinned by digest, dependencies locked with hashes,
  the `opa` binary copied from the pinned OPA image, the contract files
  copied in at build time from `docs/contracts/policy/` (the build context
  is the repository root, or the files are staged by the workflow; say
  which in the Dockerfile).

**Tests**

- Unit: every `examples/<name>.policy.json` translates to
  `examples/<name>.rego` byte for byte; every case of every
  `examples/<name>.cases.json` gets its decision through the real `opa`;
  the tenant checks refuse the corpus of `spike/tenant-guard.py` and more
  (at least: a second package clause, `data` in a rule head, in a default,
  in a function argument, in an `every`, `with` on a built-in); the bundle
  is byte-stable for the same input; the hash matches `status.rego`.
- Handler: the kopf handlers called as functions with fake `spec`,
  `status`, `patch`; the HTTP endpoints with the framework's test client,
  including long polling and 503 before the first pass.
- Without a cluster otherwise. The run against kind is track B's.

**Done when**: `nix develop -c pytest` passes in the directory; the image
builds; given a directory of `SessionPolicy` YAML files, a documented
command prints the bundle that would be published and a second `opa run`
started on it answers the example cases correctly.

**Must not touch**: `deploy/`, `backend/`, `web/`, `.github/workflows/`
(B adds the image to the workflow), the contracts.

**Fakes it needs**: none. OPA replicas are faked by a small HTTP server in
tests.

---

## Track B: OPA and deployment

**Owns**: `deploy/base/opa.yaml`, `deploy/base/policy-operator.yaml` (new);
the edits to `deploy/base/kustomization.yaml`, `networkpolicy.yaml`,
`secrets.example.yaml`, `backend.yaml` (Role and two env vars), every
`blueprint.yaml`, `deploy/gke/warmpool.yaml`, the `deploy/gke` and
`deploy/local` overlays; the operator's entry in `.github/workflows/images.yml`
and `hack/pin-images.sh`; `backend/internal/sessions/deploy_test.go` (only
to accept the new env var); `hack/` and `test/` scripts for the integration
run; `docs/policy-deployment.md`.

**Consumes**: `deploy.md`, `opa-config.yaml`, `system-authz.rego`,
`capabilities.json`, both CRD manifests, `rego-contract.md` (the request
mcp-js makes), `operator-api.yaml` (the bundle endpoint).

**Builds**

- The CRDs applied by kustomize; OPA (Deployment, Service, PDB, ConfigMap
  generated from the two contract files, not copies of them); the
  operator's Deployment, Service, ServiceAccount, Role, ClusterRole and
  bindings; the Secret; NetworkPolicy as `deploy.md` has it.
- The pod template change in the warm template and the three blueprints.
- The OPA image pinned by digest. If its version is not 1.9.0: regenerate
  `capabilities.json` (README of the contracts), re-run `run-cases.py` and
  the spikes of the design's section 11, in a contract pull request.
- The operator image in the build workflow and the pinning script.

**Tests, and the spikes that needed a cluster** (first, on kind, with a
stub bundle server if A's image is not ready):

1. Both CRDs are accepted by the API server; each CEL rule refuses what it
   should (a name that is not the session's, `iac` without an https URL, a
   changed `sessionRef`, a changed `APIToken` spec).
2. The real mcp-js image, with the new env var, asks OPA at
   `browserjs/decision/<pod name>/mcp_tools`; an allowed
   `browser_execute` runs and a denied one fails. Record what the agent's
   code sees on a denial, on OPA being down, and how long each takes.
3. From a session pod: OPA's port is reachable; the operator's and the
   backend's are not. From OPA: only the operator.
4. Kill one OPA pod: no call fails. Kill both: calls are denied, and
   recover when one is back. A new OPA pod is not ready until it has the
   bundle.
5. On staging (GKE): the same reachability under gVisor and Dataplane V2;
   a session restored from a snapshot is judged by the current policy.

**Done when**: `kubectl kustomize deploy/local` and `deploy/gke` render;
the kind run above passes in CI or as a documented script; the results of
steps 2 and 5 are written into `docs/policy-deployment.md`.

**Must not touch**: `backend/` other than `deploy_test.go`, `web/`,
`images/` (A owns the operator's directory; the mcp-js and browser images
do not change), Pomerium's configuration, certificates, `infra/`.

**Fakes it needs**: `spike/bundle-stub.py` in place of the operator, until
A's image exists.

---

## Track C: backend

**Owns**: `backend/internal/policy/` (new: the `SessionPolicy` client, the
operator client, the mode rules, the HTTP handlers), `backend/internal/sessions`
(creation with a policy: cold, warm adoption, claim recovery; the
policy-capable check), the edits to `api.go`, `main.go`, `config.go` named
under "Shared files", `docs/policy-api.md`.

**Consumes**: `backend-api.yaml` (everything except `/tokens`),
`operator-api.yaml` (validate, evaluate, schema), the `SessionPolicy` CRD,
`deploy.md` (labels, annotations, ownerReference, claim annotations, the
policy-capable test, configuration names), `examples/` (presets).

**Builds**

- Create: a `SessionPolicy` with every session, unrestricted when none is
  given; validated through the operator before anything is created; the
  order of the design's section 4.7; `starting` until `ready`.
- `GET`, `PUT`, `DELETE` of a session's policy and `PUT` of its management,
  with `If-Match`, the wait of up to 10 seconds, and the state mapping of
  `backend-api.yaml`.
- The mode rule: cookie or token, `editor` or `iac`, the 409 bodies. Token
  scopes are checked from `auth.User.Token`.
- `validate`, `evaluate`, `policy-schema.json` proxied to the operator;
  `policy-presets` from the example files embedded in the binary.
- `unsupported` for sessions that are not policy-capable.
- Everything off, and today's behaviour kept, when `POLICY_OPERATOR_URL` is
  unset.

**Tests**: unit tests in the style of the existing packages, with the
dynamic client's fake for the custom resource and an `httptest` server for
the operator: owner, admin and stranger on every route; every cell of the
mode table; `If-Match`; the 200/202 split; create with a valid, an invalid
and no policy; warm adoption and claim recovery keep the requested policy;
a session without the env var is `unsupported`.

**Done when**: `go test ./...` passes; the handlers match `backend-api.yaml`
field for field (a test decodes each response into the schema's shape).

**Must not touch**: `backend/internal/auth` beyond the shared field,
`backend/internal/proxy`, `deploy/`, `web/`, token storage and the API
host (E).

**Fakes it needs**: the operator (an `httptest` server answering as
`operator-api.yaml`), and `status` written by the test in place of the
operator.

---

## Track D: UI

**Owns**: `web/` (new pages and components; `App.tsx` routes; `api.ts`
additions, preferably in a new `policyApi.ts`), `docs/` screenshots if any.

**Consumes**: `backend-api.yaml` (all of it, tokens included),
`json-policy.schema.json`, `examples/`, `input-sample.json`, the design's
section 5.

**Builds**

- `/sessions/create`: single page create; the name with its pet-name
  placeholder; the Policy section (presets from `/policy-presets`, copy
  from a session, write one in a split panel, managed as code with its
  link); the Leave page modal. The modal on the list page goes.
- Session details as a page with tabs (Browser, Policy) under a summary;
  the Policy tab read-only, with its state line from `policy.state` and
  `loaded`.
- `/sessions/:id/policy/edit`: Monaco bundled from npm (no CDN), loaded as
  a lazy chunk; JSON diagnostics from the schema; server diagnostics from
  `/policies/validate` as markers; a Monarch grammar for Rego; the
  generated Rego pane; Test against `/policies/evaluate`; `If-Match` and
  the 412 message; unsaved-changes handling.
- Managed as code: the alert with the link, the read-only view, "Manage
  here instead"; the tag on the list.
- `unsupported` sessions: the message, no edit.
- `/tokens`: list, create (the secret shown once, with the warning of the
  design's section 7.1), revoke.

**Tests**: vitest for the API client and the state mapping; component
tests for the create form (each policy choice produces the right request),
the mode states, and the editor's handling of `errors[]`; the existing
tests keep passing. A mock server from `backend-api.yaml` for development.

**Done when**: `npm test` and `npm run build` pass; the editor chunk is
separate from the main bundle (the build output shows it); every state of
section 5 can be reached against the mock.

**Must not touch**: `backend/`, `deploy/`, the contracts.

**Fakes it needs**: a mock of `backend-api.yaml`.

---

## Track E: API tokens

**Owns**: `backend/internal/tokens/` (new: storage on `APIToken`, creation,
verification, the `/tokens` handlers), `backend/internal/auth` (the bearer
authenticator, the API host's middleware, the allow-list check), the edits
to `main.go` and `config.go` named under "Shared files", the `api.` host
in every `pomerium-config.yaml`, `deploy/gke/certificate.yaml`,
`infra/main/edge.tf` and its tests, two env vars in `deploy/base/backend.yaml`,
`docs/api-tokens.md`.

**Consumes**: `backend-api.yaml` (the introduction and `/tokens`),
`deploy/base/crd-apitoken.yaml`, the design's section 6.2.

**Builds**

- Token form `bjs_<id>_<secret>`: `id` twelve base32 characters, `secret`
  43 characters of base64url (32 random bytes). Stored: the SHA-256 of the
  whole string. Compared in constant time.
- `/tokens` (cookie only): list, create with scopes and expiry (90 days by
  default, 365 at most, 20 tokens a user), revoke.
- The API host: only `/v1/…`, only bearer tokens; the same API mux as
  `/api`, with `auth.User` built from the token (never admin); scopes
  enforced for sessions here, for policies by C through `auth.User.Token`;
  one 401 for every kind of bad token; a rate limit on failures per source
  address; `ALLOWED_EMAILS` checked on every request; `lastUsedTime` at
  most hourly.
- The route: `api.<domain>` to the backend,
  `allow_public_unauthenticated_access: true`, no identity headers; DNS
  and the certificate name. Do this part last, after #28 and #31.

**Tests**: unit tests for creation, verification, expiry, revocation,
scopes, the allow-list, the rate limit; that the API host ignores a
Pomerium assertion and the app host ignores a bearer token; that `/tokens`
refuses a token.

**Done when**: `go test ./...` passes; `curl` with a token against the
local deployment lists sessions, and without one gets 401.

**Must not touch**: `backend/internal/policy` (C), `web/` (the token page
is D's), the session-policy manifests (B).

**Fakes it needs**: the dynamic client's fake for `APIToken`.

---

## Track F: Terraform provider

**Owns**: `terraform-provider-browserjs/` (a Go module of its own),
`docs/terraform-provider.md`, a CI job for it if it adds one (a new
workflow file, not an edit to an existing one).

**Consumes**: `terraform-provider.md`, `backend-api.yaml`, `examples/`.

**Builds**: the provider, the two resources and three data sources of the
contract, plan-time validation, import, the local-installation
instructions, an `examples/` directory with the configuration of the
design's section 8.2.

**Tests**: unit tests against a fake of `backend-api.yaml` (an `httptest`
server): create, read, update, delete, import, drift when the mode is
switched in the UI, 202 then ready, invalid at plan time with row and
column, the unsupported-session error. Acceptance tests (`TF_ACC=1`)
written and runnable against a local deployment once C and E are merged;
skipped otherwise.

**Done when**: `go test ./...` passes in the module; `terraform plan` with
`dev_overrides` against the fake shows the expected plan for the example.

**Must not touch**: anything outside its directory and its docs page.

**Fakes it needs**: the backend, from `backend-api.yaml`.

---

## After the tracks: integration

When A, B and C are merged: the end-to-end run on kind (create with a policy, a denied call, an edit applied
with nothing restarted, managed-as-code lock), then on staging the sleep,
change, wake case. When E and F are merged as well: the provider's
acceptance tests and the example of section 8.2.
