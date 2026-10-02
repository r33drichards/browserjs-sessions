# Session policies: deployment

What track B of the session-policies plan deploys, how it is switched on in
stages, and what has and has not been checked on a cluster. Design:
[plans/2026-10-02-session-policies-design.md](plans/2026-10-02-session-policies-design.md).
Contract: [contracts/policy/deploy.md](contracts/policy/deploy.md).

**On `main` session policies are off**, in `deploy/gke` and in `deploy/local`.
Deploying `main` installs everything and runs nothing; sessions behave
exactly as before.

## What is deployed

Everything is in the namespace `browserjs-sessions`, from `deploy/base`.

| Object | File | Notes |
|---|---|---|
| CRDs `sessionpolicies.browserjs.dev`, `apitokens.browserjs.dev` | `crd-*.yaml` | now listed in the kustomization |
| OPA: ServiceAccount (no token), Deployment (2 replicas; 1 in `deploy/local`), Service `opa:8181`, PodDisruptionBudget `minAvailable: 1` | `opa.yaml` | `openpolicyagent/opa:1.9.0-static`, pinned by its multi-platform digest |
| ConfigMap `opa-config` | `docs/contracts/policy/kustomization.yaml` | the two contract files themselves (see "Deviations") |
| Policy operator: ServiceAccount, Role, RoleBinding, ClusterRole and binding `browserjs-policy-operator`, Deployment (1 replica, `Recreate`), Service `policy-operator:8080` | `policy-operator.yaml` | image `browserjs/policy-operator`, built from `images/policy-operator` (track A) |
| Backend: Role rules for `sessionpolicies`, `apitokens`, `apitokens/status`; env `POLICY_OPERATOR_URL`, `OPERATOR_API_TOKEN` | `backend.yaml` | no rule names `configmaps` or `secrets` |
| NetworkPolicy: one more egress rule on `session-pods`; new `opa` and `policy-operator` | `networkpolicy.yaml` | table below |
| Secret `policy-tokens` (`bundle-token`, `opa-token`, `operator-api-token`) | not in the repository | made once by the deploy workflow and by `hack/local-up.sh`, as Pomerium's are; placeholders in `secrets.example.yaml` |

On GKE both workloads run on the system pool, as the backend does (the
session pools are tainted; nothing selects them). The system pool has one
node today (cluster info, run 36970157717: 1276m of about 1930m CPU
requested, 66%). OPA and the operator add 250m and 640Mi of requests, which
brings it to about 79%, and 84% while an OPA update has its extra pod. With
one node the two OPA replicas share it: the spread constraint is
`ScheduleAnyway`, so they separate as soon as there is a second node.

### NetworkPolicy

| Pods | Ingress | Egress |
|---|---|---|
| session pods | unchanged (the backend) | unchanged, plus pods `app: opa` on 8181 |
| `app: opa` | 8181 from session pods and from the operator | the operator on 8080; DNS |
| `app: policy-operator` | 8080 from OPA and from the backend | OPA on 8181; DNS; TCP 443 and 6443 to any address |

The last rule is for the API server, which a NetworkPolicy cannot name: it
is an address outside the pod network (private on GKE, the node's on kind).
It is wider than it needs to be; narrowing it to the control plane's address
on GKE needs that address as the cluster reports it (`hack/gke-status.sh
--full` now prints the `kubernetes` EndpointSlice), and is not done.

## The three stages

`hack/policy-stage.sh` shows and sets the stage of an overlay (`gke`,
`local`). It only edits files; the change is reviewed, merged and deployed
like any other. The deploy workflow runs `hack/policy-stage.sh --check`
first and refuses files that disagree with each other.

| Stage | OPA, operator | Backend | New session pods | What is enforced |
|---|---|---|---|---|
| `off` (now) | 0 replicas | no `POLICY_OPERATOR_URL`: policies are off in the API | as before | nothing; mcp-js's own file policy, as before |
| `serving` | running | keeps policies (`SessionPolicy` objects), validates through the operator | as before: they do not ask OPA | nothing. Policies are compiled and loaded, and judge no call |
| `enforcing` | running | the same | mcp-js asks OPA on every browser call | each new session's own policy; **a session with no `SessionPolicy` is denied every browser call** |

What the script changes:

- `off`: `patch-policy-off.yaml` is listed (last) in the overlay's
  `kustomization.yaml`. It sets both Deployments to 0 replicas and removes
  `POLICY_OPERATOR_URL` from the backend.
- `serving`: that line is commented out.
- `enforcing`: also, `MCP_V8_POLICIES_JSON` is written into the overlay's pod
  templates, directly below `MCP_V8_PUBLIC_URL`: for `gke`,
  `deploy/gke/blueprint.yaml` and `deploy/gke/warmpool.yaml`; for `local`,
  `deploy/local/blueprint.yaml` and `deploy/base/blueprint.yaml`. The value
  is the contract's. `backend/internal/sessions/deploy_test.go` checks it,
  and that the files of an overlay have it together.

### off to serving

```
hack/pin-images.sh policy-operator=sha256:…    # from the "images" run
hack/policy-stage.sh gke serving
```

Needs first:

1. Track A merged, and the `images` workflow has published
   `policy-operator`. `hack/pin-images.sh --check` (and so the deploy)
   refuses an unpinned operator in any stage but `off`.
2. The backend that is pinned should be one with track C. An older backend
   ignores the two new variables, which is harmless: OPA and the operator
   then run with an empty bundle.

What the deploy does:

- OPA (two pods) and the operator start on the system node. The deploy
  workflow waits for both; OPA is ready only when it has the operator's
  bundle, so "ready" already means the bundle path works end to end.
- The backend's Deployment changes (one variable), so the backend restarts,
  as on any backend deploy.
- **Existing sessions, running, sleeping or stopped: nothing.** No session
  pod and no warm pod is touched: the pod templates are unchanged, so the
  warm pool is not recreated.
- No session asks OPA. A policy saved through the API is compiled and
  loaded and has no effect. Every session is, in the contract's term, not
  policy-capable, and the API reports its policy as `unsupported`.

This stage exists to see the operator, the bundle and OPA working in
production while nothing depends on them.

### serving to enforcing

```
hack/policy-stage.sh gke enforcing
```

Needs first, and this one is a hard requirement: **the deployed backend
creates a `SessionPolicy` for every session it creates or adopts** (track
C). With a backend that does not, every session created from then on has
no policy, and all its browser calls are denied.

What the deploy does:

- The blueprint ConfigMap changes, so the backend restarts.
- The `SandboxTemplate` changes, and the warm pool (`updateStrategy:
  Recreate`) replaces the Sandboxes that are waiting. For a minute or two
  the pool is empty or short and a new session starts cold, which is the
  path that already exists. Sessions in use are not touched.
- **Sessions created from now on** (cold, or adopted from the new warm
  pods) ask OPA at `browserjs/decision/<session id>/mcp_tools` on every
  browser call.
- **A warm pod nobody has adopted** has no `SessionPolicy`, so it is denied
  everything. Nothing can call it either. After adoption it is denied until
  the backend has created its policy and OPA has loaded it (measured below:
  a published bundle judges calls about 0.2 s later).
- **Sessions that existed before this deploy never become enforcing.**
  A Sandbox keeps the pod template it was created with: a wake from a
  snapshot restores the old process, and a cold wake makes a pod from the
  same stored template. They stay unrestricted, and `unsupported` in the
  API, until they are deleted. To put a policy on one, delete it and create
  it again.

From this stage on, OPA is in the path of every browser call of the new
sessions: see "Failing closed" below for what its absence looks like.

### Going back

- `enforcing` to `serving`: new sessions stop asking OPA. Sessions created
  while enforcing keep asking for as long as they live.
- To `off`: only when no session that asks OPA is left. With OPA at 0
  replicas every browser call of such a session is denied, after 5 seconds
  each. `hack/gke-status.sh` (the deploy summary and the "cluster info"
  workflow) lists which Sandboxes ask OPA (`asks-opa=yes`). The script
  cannot know this; it is the operator's check to make.

### Local

`deploy/local` is `off` as well, for the same reason: without tracks A and
C a local session would be denied everything. `hack/local-up.sh` creates the
Secret, builds `browserjs/policy-operator:dev` once
`images/policy-operator/Dockerfile` exists (context: the repository root),
loads it, and waits for both Deployments, which is immediate while they
have no pods. Then `hack/policy-stage.sh local serving` or `enforcing`, and
`hack/local-up.sh` again.

## Images

- **OPA** is named in `deploy/base/opa.yaml` with tag and digest. The tag
  `1.9.0-static` and the digest were read from Docker Hub's registry API on
  2026-10-01 (an index with `linux/amd64` and `linux/arm64`; user
  `1000:1000`, entrypoint `/opa`). 1.9.0 is the version the contracts were
  checked with, so `capabilities.json` is unchanged. The newest release then
  was 1.21.1. To move: change the tag and digest here and the copy in the
  operator's Dockerfile together, regenerate `capabilities.json` and re-run
  the cases and spikes (the contracts' README), in a contract pull request.
  The `policy kind` workflow takes the `opa` binary out of this image and
  runs the contract's cases with it.
- **The operator** is the fourth image of `.github/workflows/images.yml` and
  of `hack/pin-images.sh`. It is built with the repository root as context
  and `images/policy-operator/Dockerfile` as the file, because it copies
  files of `docs/contracts/policy`. The root `.dockerignore` lets through
  only `web/` and `backend/`, so the image needs its own
  `images/policy-operator/Dockerfile.dockerignore`. It is skipped, with a
  notice, while the Dockerfile does not exist. It is rebuilt when
  `images/policy-operator/` or `docs/contracts/policy/` changes.

## Checked on kind

`test/policy/run.sh`, run by the `policy kind` workflow on every pull
request that touches the manifests. Real: the OPA image with the contract's
configuration and `system.authz`, the Services, Roles, Secret wiring and
NetworkPolicies of `deploy/base`, both CRDs, and the mcp-js image
(v0.21.0-rc.3) with the variable `hack/policy-stage.sh` writes. Stand-ins
(`test/policy/stub.py`): the operator (a bundle server publishing bundles
the script builds from the contract's examples with the image's own `opa`),
the browser container (an MCP server that runs nothing) and the backend (a
listener). Session pods are plain Pods with the session label.

Run 36971833355 (2026-10-02, Kubernetes v1.37.0 on kind, kindnet's
NetworkPolicy): 46 checks, all passed.

1. **The CRDs.** Both are accepted. Refused, each with its own message: a
   `SessionPolicy` whose name is not its session's, `iac` without a URL or
   with an `http` URL, a name that is not a session ID, a changed
   `sessionRef`; an `APIToken` not named `tok-<id>`, a changed `APIToken`
   spec. Accepted: `iac` with an `https` URL, a changed `source`, a write
   to an `APIToken`'s status. `management` defaults to `editor`.
2. **Decisions through the real mcp-js.** It asks
   `POST http://opa.browserjs-sessions.svc:8181/v1/data/browserjs/decision/<pod name>/mcp_tools`,
   the path built from `$(SESSION_ID)`.

   | Situation | What the agent's code sees from `mcp.callTool` | Time |
   |---|---|---|
   | allowed (`url` under `no-scripting`) | the tool's result | 4 ms in the call; 54 ms the whole `run_js` |
   | denied by the policy (`evaluate` under `no-scripting`) | throws `mcp.callTool denied by policy: browser.browser_execute is not allowed` | 3 ms; 54 ms |
   | the session has no policy in the bundle | the same message as a denial; the browser is never called | 8 ms; 54 ms |
   | no OPA replica is ready, or there is no OPA pod | throws `mcp.callTool: hook chain error: OPA request failed: error sending request for url (…)` | 5.0 s, every call |
   | OPA's packets are dropped | the same | 5.0 s, every call |

   A denial does not say which rule denied, or that a policy is missing.
   A changed bundle judged the next call 0.23 s after it was published,
   with no pod restarted. From a session pod OPA's API answers a decision
   request with 200 and everything else with 401 (`?explain`, policies,
   `loaded`, data, a write, a tenant document).
3. **Reachability.** A session pod reaches OPA on 8181 (by Service and by
   pod address) and the internet; not the operator, not the backend, not
   another session, not the API server. OPA reaches the operator and
   nothing else (not the backend, a session, the API server or the
   internet). The operator reaches OPA and the API server; not the backend
   or a session. Another pod of the namespace reaches neither OPA nor the
   operator.
4. **Replicas.** While one of the two OPA pods was deleted and replaced, 367
   calls in 20 s all ran (slowest 0.13 s). With no OPA pod every call was
   denied, and allowed again once a pod was back. A new OPA pod is running
   and not ready, and the Service has no address, until it has a bundle.

Two things this found:

- **With OPA away, a browser call is not refused at once: it takes the full
  5 seconds of mcp-js's timeout**, also when the Service simply has no
  address (kind's kube-proxy does not reject the connection). An agent in
  an enforcing session sees every call hang for 5 s and then throw.
- The `preStop` sleep on OPA (5 s, the kubelet's own, since the image has no
  shell) is not in the contract. It keeps a terminating pod answering until
  the Service has stopped sending to it.

## Not yet checked: GKE

Step 5 of the track needs the production cluster (there is no separate
staging cluster) and was not run: this track does not deploy. Nothing below
needs the `enforcing` stage except the last item.

After a deploy in the `serving` stage:

1. "cluster info" workflow, section "Session policies": `opa` 2/2 and
   `policy-operator` 1/1 ready, the `opa` EndpointSlice with two ready
   addresses, both CRDs established. The operator being ready shows that
   its NetworkPolicy lets it reach the API server on GKE (the `443`/`6443`
   rule), and OPA being ready shows that it reaches the operator.
2. gVisor and Dataplane V2, from a running session's pod. The new egress
   rule applies to every session pod, old ones included, so any session
   will do. With kubectl (the browser container has Node):

   ```
   kubectl -n browserjs-sessions exec s-… -c browser -- node -e '
     const ask = (u, o) => fetch(u, { signal: AbortSignal.timeout(4000), ...o }).then(r => r.status, e => String(e.cause?.code ?? e));
     const body = JSON.stringify({ input: { server: "browser", tool: "browser_execute", arguments: { operations: [] } } });
     Promise.all([
       ask("http://opa.browserjs-sessions.svc:8181/v1/data/browserjs/decision/" + process.env.HOSTNAME + "/mcp_tools", { method: "POST", body }),
       ask("http://opa.browserjs-sessions.svc:8181/v1/policies"),
       ask("http://policy-operator.browserjs-sessions.svc:8080/healthz"),
       ask("http://backend.browserjs-sessions.svc/healthz"),
     ]).then(r => console.log(r.join(" ")))'
   ```

   Expected: `200 401` and two timeouts. A timeout in the first place means
   the rule does not work under gVisor with Dataplane V2 (the existing DNS
   rules needed NodeLocal DNSCache's address added for the same
   combination).
3. `kubectl -n browserjs-sessions get sessionpolicies` after saving a
   policy in the UI: `Ready` True, `Loaded` naming both replicas.

After a deploy in the `enforcing` stage, with a session created after it:

4. An allowed and a denied call, as on kind.
5. A session restored from a snapshot is judged by the current policy:
   let the session sleep (idle), change its policy to deny what it allowed,
   wake it, and make the call. Expected: denied, with nothing done to the
   pod. `hack/gke-status.sh` shows whether the pod was restored
   (`PodRestored`).

## Failing closed

As the design's section 4.5, with what was measured: an unreachable OPA
denies after 5 s a call; a session without a policy is denied at once; a
replica without a bundle is not behind the Service; one replica can go
without a call failing.

## Deviations from the contract

1. **`deploy.md`, "policy-capable"**: the test is written as "contains
   `/browserjs/decision/`". The value the same contract prescribes has
   `"policy_path":"browserjs/decision/…"`, with no slash before
   `browserjs`. The test has to be "contains `browserjs/decision/`".
   Affects track C. `hack/gke-status.sh` uses the corrected form.
2. **`opa-config` "by `configMapGenerator`, not copies"**: kustomize does
   not read files outside a kustomization's directory, so `deploy/base`
   cannot generate it from `docs/contracts/policy`. Added:
   `docs/contracts/policy/kustomization.yaml`, which generates the ConfigMap
   there and is a resource of `deploy/base`. No contract file changed.
3. **The pod template change is not in the files on `main`.** It is the
   `enforcing` stage, applied by `hack/policy-stage.sh`, for the reason at
   the top. `deploy_test.go` needed no change to accept the variable (it
   already renders the blueprint with the ID `$(SESSION_ID)`); it gained a
   test of the variable itself.
4. **Operator, "Volumes: none"**: the Deployment has a read-only root
   filesystem and an `emptyDir` at `/tmp`, and runs as uid 65532. If the
   image needs to write elsewhere, that is where to change it.
5. **OPA**: the `preStop` sleep, a CPU limit of 500m and a memory limit of
   512Mi (the design says the replicas have CPU limits; the contract gives
   requests only), and the spread constraint as `ScheduleAnyway`.
6. **`deploy.yml`** is not in the track's list of files, but the contract
   says the Secret is created the way the existing ones are: the workflow
   makes `policy-tokens` once, checks the stage, and waits for the two
   Deployments.
7. The comments at the top of the two CRD files still say they are not in
   the kustomization. They are contract files, so they were left.
