# Deployment contract

Names, labels, ports, images, secrets and the two changes to existing
manifests. Everything is in the namespace `browserjs-sessions`.

## One namespace, and what that means

The shared OPA (a Deployment and a Service, not a sidecar), the operator,
every `SessionPolicy` and every `APIToken` live in `browserjs-sessions`,
the namespace of the session Sandboxes, the backend, Pomerium and Dex. A
`SessionPolicy` has to be there: an `ownerReference` to its Sandbox only
works within a namespace.

Consequences, since Pomerium's and Dex's ConfigMaps and the Secrets are in
the same namespace:

- **No ServiceAccount gains rights on `configmaps` or `secrets`.** The
  backend's Role grows by two custom resources of `browserjs.dev` and
  nothing else; the operator's Role names `sessionpolicies`, `events` and
  `endpointslices`. RBAC cannot select objects by label, so a rule on
  `configmaps` here would reach Pomerium's routes; that is why policy state
  is a custom resource and not a ConfigMap.
- OPA's own ConfigMap (`opa-config`) and Secret (`policy-tokens`) are
  mounted by the kubelet. OPA's ServiceAccount has no token and no Role.
- Session pods can already reach nothing in the cluster; they gain one
  destination, OPA on 8181. They cannot reach the operator, the backend,
  Pomerium or Dex, in this namespace or any other.
- Pod labels are what the NetworkPolicies select on. `app: opa` and
  `app: policy-operator` must not be used by anything else in the namespace.

## Workloads

| | OPA | Policy operator |
|---|---|---|
| Deployment | `opa` | `policy-operator` |
| Pod label | `app: opa` | `app: policy-operator` |
| Replicas | 2 (1 in `deploy/local`) | 1, `strategy: Recreate` |
| Service | `opa`, port 8181 | `policy-operator`, port 8080 |
| ServiceAccount | `opa`, no token mounted | `policy-operator` |
| Image | `openpolicyagent/opa`, version 1.9.0, static variant, pinned by digest | `browserjs/policy-operator` (in the registry: `us-west1-docker.pkg.dev/browserjs-sessions/browserjs/policy-operator`), pinned by `hack/pin-images.sh` |
| Source | none | `images/policy-operator/` (Dockerfile, Python package, tests) |
| Command | `opa run --server --addr=:8181 --authentication=token --authorization=basic --config-file=/config/opa-config.yaml /config/system-authz.rego` | `kopf run --standalone --namespace=browserjs-sessions --liveness=http://0.0.0.0:8081/healthz -m policy_operator` |
| Readiness | `GET /health?bundles` on 8181 | `GET /readyz` on 8080 |
| Liveness | `GET /health` on 8181 | kopf's, on 8081 |
| Scheduling (GKE) | the system pool, as the backend; `topologySpreadConstraints` over `kubernetes.io/hostname` | the system pool |
| Disruption | PodDisruptionBudget `opa`, `minAvailable: 1`; `RollingUpdate`, `maxUnavailable: 0` | none |
| Resources (requests) | 100m CPU, 256Mi | 50m CPU, 128Mi |
| Volumes | ConfigMap `opa-config` at `/config`; `emptyDir` at `/var/opa` (bundle persistence) | none |

The operator image contains the `opa` binary of the same version as the OPA
image (copied from it in the Dockerfile) and `capabilities.json`,
`decision-module.rego.tmpl` and `json-policy.schema.json` from this
directory, copied at build time, not duplicated in the source.

## ConfigMap and Secret

| Object | Keys | Used by |
|---|---|---|
| ConfigMap `opa-config` | `opa-config.yaml`, `system-authz.rego` (the files of this directory, by `configMapGenerator`) | OPA |
| Secret `policy-tokens` | `bundle-token`, `opa-token`, `operator-api-token`: three independent random strings, 32 bytes of base64url each | see below |

| Key | Env var, container | Meaning |
|---|---|---|
| `bundle-token` | `BUNDLE_TOKEN`, OPA; `BUNDLE_TOKEN`, operator | OPA fetching the bundle from the operator |
| `opa-token` | `OPERATOR_TOKEN`, OPA; `OPA_TOKEN`, operator | the operator reading `browserjs/loaded` from each OPA replica |
| `operator-api-token` | `OPERATOR_API_TOKEN`, operator; `OPERATOR_API_TOKEN`, backend | the backend calling validate and evaluate |

`secrets.example.yaml` gains the Secret with placeholder values; on GKE it
is created the way the existing secrets are.

Backend configuration added: `POLICY_OPERATOR_URL`
(`http://policy-operator.browserjs-sessions.svc:8080`), `OPERATOR_API_TOKEN`,
and for tokens `API_URL` (the API host's base URL) and `ALLOWED_EMAILS`.
Policies are off, and the API behaves as today, while `POLICY_OPERATOR_URL`
is unset.

## RBAC

| ServiceAccount | Scope | Rules |
|---|---|---|
| `policy-operator` | Role | `browserjs.dev` `sessionpolicies`: get, list, watch, patch; `sessionpolicies/status`: get, patch; `events`: create; `discovery.k8s.io` `endpointslices`: get, list, watch |
| `policy-operator` | ClusterRole | `apiextensions.k8s.io` `customresourcedefinitions`: list, watch |
| `backend` (added to its Role) | Role | `browserjs.dev` `sessionpolicies`: get, list, watch, create, update, patch, delete; `apitokens`: get, list, create, delete; `apitokens/status`: patch |

The backend never writes `sessionpolicies/status`.

## Labels and annotations on a `SessionPolicy`

| | Value | Set by |
|---|---|---|
| name | the session ID | backend |
| `ownerReferences[0]` | the session's Sandbox (`agents.x-k8s.io/v1beta1`, `Sandbox`), `controller: false`, `blockOwnerDeletion: false` | backend |
| label `browserjs.dev/owner` | the owner hash, as on the Sandbox (`sessions.OwnerLabel`) | backend |
| annotation `browserjs.dev/updated-by` | `ui` or `token:<token name>` | backend, on every write |

On a `SandboxClaim`, the policy a session was asked to be created with is
carried as the annotations `browserjs.dev/policy-kind`,
`browserjs.dev/policy-source`, `browserjs.dev/policy-mode` and
`browserjs.dev/policy-url`, for `RecoverClaims`.

## Change to the session pod template

The mcp-js container gains one env var. The image is not changed: its
`ENV MCP_V8_POLICIES_JSON` stays as the default for running it alone.

In `deploy/gke/warmpool.yaml` (after `SESSION_ID`, which it already has):

```yaml
- name: MCP_V8_POLICIES_JSON
  value: >-
    {"mcp_tools":{"mode":"all","policies":[
    {"url":"file:///etc/mcp/mcp_tools.rego"},
    {"url":"http://opa.browserjs-sessions.svc:8181","policy_path":"browserjs/decision/$(SESSION_ID)/mcp_tools"}]},
    "filesystem":{"policies":[{"url":"file:///etc/mcp/filesystem.rego"}]}}
```

In the three blueprints (`deploy/base`, `deploy/gke`, `deploy/local`), which
the backend renders, the same with `{{ .ID }}` in place of `$(SESSION_ID)`.

`backend/internal/sessions/deploy_test.go` keeps the warm template and the
GKE blueprint in step; it must accept this one difference, as it accepts the
one in `MCP_V8_PUBLIC_URL`.

A session is **policy-capable** exactly when its Sandbox's mcp-js container
has an `MCP_V8_POLICIES_JSON` env var that contains `browserjs/decision/`
(no leading slash: in the value above it follows a quote).
The backend uses that to tell sessions that predate the feature
(`unsupported`).

## NetworkPolicy

Added to `deploy/base/networkpolicy.yaml`.

| Policy | Rule |
|---|---|
| `session-pods` (existing), one more egress rule | to pods `app: opa`, TCP 8181 |
| `opa` (new; ingress and egress) | ingress on 8181 from pods `app: browserjs-session` and `app: policy-operator`; egress to pods `app: policy-operator` on 8080, and DNS |
| `policy-operator` (new; ingress and egress) | ingress on 8080 from pods `app: opa` and `app: backend`; egress to pods `app: opa` on 8181, to the API server, and DNS |
| `backend` (existing) | unchanged: it restricts ingress only |

The kubelet's probes are not subject to NetworkPolicy on GKE Dataplane V2
(they come from the node); if the local cluster's CNI differs, allow the
node's address on the probe ports.
