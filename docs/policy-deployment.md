# Session policies: deployment

What track B of the session-policies plan deploys, how it is switched on in
stages, and what has and has not been checked on a cluster. Design:
[plans/2026-10-02-session-policies-design.md](plans/2026-10-02-session-policies-design.md).
Contract: [contracts/policy/deploy.md](contracts/policy/deploy.md).

**On `main`, `deploy/gke` is enforcing and `deploy/local` is off.** New
production sessions receive a policy and ask OPA on every browser, desktop
and shell tool call. Sessions created before enforcement keep their stored
template and remain unrestricted; see "serving to enforcing" below.

The successful [production deploy of `b5a267c`](https://github.com/r33drichards/computer-use/actions/runs/37088963148)
(started October 2, 2026 at 7:12 p.m. America/Los_Angeles) checked the enforcing
stage and successfully rolled out the policy operator and OPA. This records
rollout evidence, not a fresh audit of all running sessions or the functional
GKE checks below. API tokens are enabled as well.

The stages below remain the procedure for a new deployment or rollback;
they do not describe an outstanding production rollout.

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
| `off` | 0 replicas | no `POLICY_OPERATOR_URL`: policies are off in the API and the UI | as before | nothing; mcp-js's own file policy, as before |
| `serving` | running, with an empty bundle | the same as `off` | as before | nothing. Nothing a user or a session can notice has changed |
| `enforcing` | running | keeps a `SessionPolicy` for every session it creates | mcp-js asks OPA on every browser, desktop and shell tool call | each new session's own policy; **a session with no `SessionPolicy` is denied every browser, desktop and shell tool call** |

What the script changes:

- `off`: `patch-policy-off.yaml` (both Deployments at 0 replicas) and
  `patch-policy-backend-off.yaml` (the backend without `POLICY_OPERATOR_URL`)
  are the last two patches of the overlay's `kustomization.yaml`.
- `serving`: the first is commented out.
- `enforcing`: both are commented out, and `MCP_V8_POLICIES_JSON` is written
  into the overlay's pod templates, directly below `MCP_V8_PUBLIC_URL`: for
  `gke`, `deploy/gke/blueprint.yaml` and `deploy/gke/warmpool.yaml`; for
  `local`, `deploy/local/blueprint.yaml` and `deploy/base/blueprint.yaml`.
  The value is the contract's. `backend/internal/sessions/deploy_test.go`
  checks it, and that the files of an overlay have it together.

**Why the backend and the pod templates switch together.** A backend that
keeps policies asks for one on every warm pod it adopts, and gives back a
pod that does not ask OPA (`adoptPolicy`, `backend/internal/sessions/policy.go`):
with the backend on and the templates unchanged, every new session would
miss the warm pool and start cold. The script refuses that combination.

### off to serving

```
hack/pin-images.sh policy-operator=sha256:…    # from the "images" run on main
hack/policy-stage.sh gke serving
```

Needs first: the `images` workflow has published `policy-operator` (it does
on the push to `main` that merges this change, since `images.yml` changed).
`hack/pin-images.sh --check`, and so the deploy, refuses an unpinned
operator in any stage but `off`.

What the deploy does:

- OPA (two pods) and the operator start on the system node. The deploy
  workflow waits for both. OPA is ready only when it has the operator's
  bundle, so "ready" means the operator reached the API server, built a
  bundle, and OPA fetched it through the NetworkPolicies.
- The backend is not touched and does not restart.
- **Sessions, existing or new, warm pods included: nothing.** No pod
  template changed, the warm pool is not recreated, no pod asks OPA.

This stage exists to see the operator, the bundle and OPA working in
production while nothing depends on them.

### serving to enforcing

```
hack/policy-stage.sh gke enforcing
```

Needs first: **the pinned backend is one with the policy API** (track C,
on `main` since #48). An older backend ignores `POLICY_OPERATOR_URL` and
would make sessions with no policy, which are denied every browser, desktop
and shell tool call.

What the deploy does:

- The backend gains `POLICY_OPERATOR_URL` and a new blueprint, and
  restarts. From then on the UI shows the Policy section and tab.
- The `SandboxTemplate` changes, and the warm pool (`updateStrategy:
  Recreate`) replaces the seven Sandboxes that are waiting. For a minute or
  two the pool is empty or short; a session created then starts cold (about
  100 s), which is the path that already exists. Sessions in use are not
  touched.
- **Sessions created from now on** (cold, or adopted from the new warm
  pods) get a `SessionPolicy` (the unrestricted one unless another was
  asked for) and ask OPA at `browserjs/decision/<session id>/mcp_tools` on
  every browser, desktop and shell tool call. A session shows `starting`
  until its policy is loaded (measured below: about 0.2 s from the object
  to `Ready`).
- **A warm pod nobody has adopted** has no `SessionPolicy`, so it is denied
  everything. Nothing can call it either.
- **Sessions that existed before this deploy never become enforcing.**
  A Sandbox keeps the pod template it was created with: a wake from a
  snapshot restores the old process, and a cold wake makes a pod from the
  same stored template. They stay unrestricted, show
  `policy: {"state": "unsupported"}`, and cannot be given a policy. To put
  a policy on one, delete it and create it again.

From this stage on, OPA is in the path of every browser, desktop and shell
tool call of the new sessions: see "Checked on kind" for what its absence looks like.

### Going back

- `enforcing` to `serving`: the backend stops keeping policies and new
  sessions stop asking OPA; the warm pool is replaced again. **Sessions
  created while enforcing keep asking OPA for as long as they live**, and
  keep the policy they have (the operator still serves it); it can no
  longer be edited, the API being off.
- To `off`: only when no session that asks OPA is left. With OPA at 0
  replicas every browser, desktop and shell tool call of such a session is
  denied, after 5 seconds each. `hack/gke-status.sh` (the deploy summary and the "cluster info"
  workflow) lists which Sandboxes ask OPA (`asks-opa=yes`). The script
  cannot know this; it is the operator's check to make.

### Local

`deploy/local` is `off` as well. `hack/local-up.sh` creates the Secrets,
builds `browserjs/policy-operator:dev` (context: the repository root),
loads it, and waits for both Deployments, which is immediate while they
have no pods. Then `hack/policy-stage.sh local enforcing`, and
`hack/local-up.sh` again.

## Desktop control under enforcement

`desktop_execute` (the mouse, keyboard and screen of the session's display)
goes the same way as `browser_execute`: mcp-js asks its own file policy,
which allows both tools, and then OPA, at the same decision path, with
`input.tool` naming the tool. So a session's policy can allow or deny it.

What a policy does with it is the policy's own text: policies are Rego
([`contracts/policy/rego-contract.md`](contracts/policy/rego-contract.md)),
`allow_tool_call` is asked for every call, and the decision module refuses
any server or tool it does not list. The unrestricted policy, which a new
session gets, allows the desktop (and the shell). Every preset that
restricts the browser denies both, because either can drive the browser
around its rules: with the mouse and keyboard, or with a command that
reaches the browser's control ports on loopback. A policy that does
otherwise is saved with a warning (`browser_bypass_desktop`,
`browser_bypass_shell`, `shell_bypass_desktop`, and for a list of programs
that is not one, `shell_launcher_allowed` and `shell_env_allowed`). The desktop under the
unrestricted policy and under `no-scripting` is checked on kind (item 5).

The shell is a second upstream server of mcp-js, `exec` (mcp-exec in the
browser container), with the tools `exec`, `stream_logs`, `search_logs` and
`kill`. The decision module lists them.

## Rolling out on production, from workflows only

Each step is a pull request made with the commands shown, merged, and then
the `deploy` workflow on `main` (confirm: `deploy`). `cluster info` is the
read-only workflow; its summary has a "Session policies" section.

| Step | Pull request | After `deploy`, in `cluster info` | In the UI |
|---|---|---|---|
| 0. Install | this one | `opa` and `policy-operator`: WANTED 0. Both CRDs `established=True`. Secrets `policy-tokens` (3 keys) and `api-tokens` (1 key) exist. No `POLICY_OPERATOR_URL`. Every template and Sandbox `asks-opa=no`. Warm pool unchanged (7 waiting, same ages) | nothing new; create a session, it is taken warm and works |
| 1. Serving | `hack/pin-images.sh policy-operator=sha256:…` (digest from the `images` run on `main` after step 0) and `hack/policy-stage.sh gke serving` | `policy-operator` READY 1, `opa` READY 2 (ready means the bundle is active), the `opa` EndpointSlice with two ready addresses, pods on the system node, no restarts. Still no `POLICY_OPERATOR_URL`, still `asks-opa=no`, warm pool untouched | nothing new |
| 2. Enforcing | `hack/policy-stage.sh gke enforcing`; the pinned backend must have the policy API | `POLICY_OPERATOR_URL` set. `SandboxTemplate/session asks-opa=yes`. The 7 warm Sandboxes are new (ages) and `asks-opa=yes`; older Sandboxes `asks-opa=no`. After creating a session: a `SessionPolicy` of its name, Ready `True`, Loaded naming 2 replicas | create a session: Policy section on the create page, Policy tab on the session; the browser works; saving a policy that denies `evaluate` makes that call fail. A session from before: policy `unsupported`, works as before |
| 3. API tokens | `ALLOWED_EMAILS` in `deploy/gke/patch-backend.yaml` (above) | `API_URL` and `ALLOWED_EMAILS` both shown | the Tokens page makes a token; `curl -H "Authorization: Bearer …"` against the API host lists sessions |

Rollback, each a pull request and a `deploy`:

- From 1: `hack/policy-stage.sh gke off`. Nothing depended on the pods.
- From 2: `hack/policy-stage.sh gke serving`. New sessions are as before
  the feature; the warm pool is replaced once more. **Do not go to `off`
  while `cluster info` lists a Sandbox with `asks-opa=yes`**: delete those
  sessions first, or their browser calls are denied (5 s each).
- From 3: remove `ALLOWED_EMAILS`. Tokens are refused; the objects stay.
- If step 1 or 2 fails in the `deploy` run itself (the step "Policy
  operator and OPA" prints the Deployment and its log): at step 1 nothing
  uses the pods, so revert at leisure. At step 2 the pod templates are
  already applied: revert to `serving` and deploy at once.

The recorded production deploy confirms that the operator and OPA roll out
successfully on GKE. Record separate functional evidence that a gVisor
session pod reaches OPA under Dataplane V2 (an allowed tool call answers
quickly, rather than a 5 s denial); rollout success alone does not prove it.

## API tokens

Separate from the stages above, and independent of them except that a
token's `policies` scopes are useful only when enforcing. The deployment
includes the `APIToken` CRD, the backend's Role rules, and the Secret
`api-tokens` (`signing-key`), which the deploy workflow makes once. They are
enabled in `deploy/gke/patch-backend.yaml` (docs/api-tokens.md):

```yaml
            - name: ALLOWED_EMAILS
              value: rwendt1337@gmail.com,browserjs06@gmail.com
```

the same addresses as the policy in `deploy/gke/pomerium-config.yaml`; the
two lists have to be changed together. Turned off again by removing it:
every token is then refused, and none can be made.

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
  only `web/` and `backend/`, so the image has its own
  `images/policy-operator/Dockerfile.dockerignore`. It is rebuilt when
  `images/policy-operator/` or `docs/contracts/policy/` changes.

## Checked on kind

`test/policy/run.sh`, run by the `policy kind` workflow on every pull
request that touches the manifests. Real: the OPA image with the contract's
configuration and `system.authz`, the Services, Roles, Secret wiring and
NetworkPolicies of `deploy/base`, both CRDs, and the mcp-js image
(v0.21.0-rc.4) with the variable `hack/policy-stage.sh` writes. Stand-ins
(`test/policy/stub.py`): the operator (a bundle server publishing bundles
the script builds from the contract's examples with the image's own `opa`),
the browser container (an MCP server that runs nothing) and the backend (a
listener). Session pods are plain Pods with the session label.

Run 37028978340 (2026-10-02, Kubernetes v1.37.0 on kind, kindnet's
NetworkPolicy): 61 checks, all passed. The figures of items 2 to 4 are
those of run 36971833355, the first.

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

5. **The real operator**, built from `images/policy-operator` in the same
   run and put in place of the stand-in, with `deploy/base`'s own
   Deployment, Role and NetworkPolicy. It starts and becomes ready (the
   list at start, the watch, the API server through the NetworkPolicy); a
   `SessionPolicy` is `Ready` 0.22 s after it is created, with `hash`,
   `rego` and `loaded: 2 of 2` in its status (the status subresource, and
   both replicas found through the EndpointSlices and asked by pod
   address); allowed runs, denied does not; an edit judges the next call
   0.25 s later; a source that does not compile gives `Compiled=False`
   with errors while the previous policy stays in force; a delete goes
   through the finalizer and the session is denied 0.18 s later; the
   operator is not restarted over 75 s (kopf's liveness on 8081). Under
   the unrestricted policy `desktop_execute` runs, and under `no-scripting`
   it is denied. (As of the Rego-only presets; see the job's summary for
   the last run.)

Three things this found:

- **The operator's image exits at start as it is**: kopf asks for the
  user's name, and uid 65532 has no entry in the image's `/etc/passwd`
  (`KeyError: getpwuid(): uid not found: 65532`). The Deployment sets
  `USER=policy-operator`, which is what Python reads first. The image
  should have the entry.

- **With OPA away, a browser call is not refused at once: it takes the full
  5 seconds of mcp-js's timeout**, also when the Service simply has no
  address (kind's kube-proxy does not reject the connection). An agent in
  an enforcing session sees every call hang for 5 s and then throw.
- The `preStop` sleep on OPA (10 s, the kubelet's own, since the image has no
  shell) is not in the contract. It keeps a terminating pod answering until
  the Service has stopped sending to it. Nor is the 5 s before a new
  replica's first readiness probe, which keeps it out of the Service until
  session pods can reach it. Both are there so that no call is denied while
  a replica is replaced; [`spec/opa-replacement`](../spec/opa-replacement/README.md)
  has the model, what was seen on kind, and what the two rest on.

## GKE verification

The production deploy linked above confirms the enforcing stage and
successful rollouts of the operator and OPA. The checks below describe
additional evidence to collect; this document has no recorded results for
session-to-OPA reachability, allowed and denied calls, or snapshot restore
under an edited policy. Do not infer those results from a successful deploy.

Items 1 and 2 can be run in either the `serving` or `enforcing` stage:

1. "cluster info" workflow, section "Session policies": `opa` 2/2 and
   `policy-operator` 1/1 ready, the `opa` EndpointSlice with two ready
   addresses, both CRDs established. The operator being ready shows that
   its NetworkPolicy lets it reach the API server on GKE (the `443`/`6443`
   rule), and OPA being ready shows that it reaches the operator.
2. gVisor and Dataplane V2, from a running session's pod. The new egress
   rule applies to every session pod, old ones included, so any session
   will do. The browser container has bash as `/bin/sh` and little else
   (Node is not on its `PATH`), so the request is made by hand. Example
   command (no recorded result in this document):

   ```
   kubectl -n browserjs-sessions exec s-… -c browser -- /bin/sh -c '
     body="{\"input\":{\"server\":\"browser\",\"tool\":\"browser_execute\",\"arguments\":{\"operations\":[]}}}"
     exec 3<>/dev/tcp/opa.browserjs-sessions.svc/8181 || exit 1
     printf "POST /v1/data/browserjs/decision/%s/mcp_tools HTTP/1.1\r\nHost: opa\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s" "$HOSTNAME" "${#body}" "$body" >&3
     read -t 5 -r status <&3; echo "OPA: $status"
     (exec 4<>/dev/tcp/policy-operator.browserjs-sessions.svc/8080) && echo "operator: OPEN, wrong"'
   ```

   Expected: `OPA: HTTP/1.1 200 OK` at once, then nothing for about two
   minutes (the kernel's connect timeout) and an error for the operator.
   If the first connection hangs instead, the rule does not work under
   gVisor with Dataplane V2 (the existing DNS rules needed NodeLocal
   DNSCache's address added for that combination).
After a deploy in the `enforcing` stage, with a session created after it:

3. "cluster info", section "Session policies": the session's
   `SessionPolicy` is listed, `Ready` True, `Loaded` naming both replicas.

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

1. **`deploy.md`, "policy-capable"**: it said "contains
   `/browserjs/decision/`", which the prescribed value does not. Fixed in
   the contract since (#50): "contains `browserjs/decision/`".
2. **`opa-config` "by `configMapGenerator`, not copies"**: kustomize does
   not read files outside a kustomization's directory, so `deploy/base`
   cannot generate it from `docs/contracts/policy`. Added:
   `docs/contracts/policy/kustomization.yaml`, which generates the ConfigMap
   there and is a resource of `deploy/base`. No contract file changed.
3. **The pod template change follows the overlay's stage.** On `main` it
   is present in GKE's templates and absent locally. `hack/policy-stage.sh`
   applies it in the `enforcing` stage, for the reason at the top.
   `deploy_test.go` needed no change to accept the variable (it already
   renders the blueprint with the ID `$(SESSION_ID)`); it gained a test of
   the variable itself.
4. **Operator**: a read-only root filesystem with an `emptyDir` at `/tmp`
   (the contract says no volumes; the operator needs `/tmp`), uid 65532,
   and `USER` in its environment (above). The ClusterRole of the contract
   is installed and unused: the operator runs with kopf's scanning off.
5. **OPA**: the `preStop` sleep, a CPU limit of 500m and a memory limit of
   512Mi (the design says the replicas have CPU limits; the contract gives
   requests only), and the spread constraint as `ScheduleAnyway`.
6. **`deploy.yml`** is not in the track's list of files, but the contract
   says the Secret is created the way the existing ones are: the workflow
   makes `policy-tokens` and `api-tokens` once, checks the stage, waits for
   the CRDs to be established and for the two Deployments.
7. The comments at the top of the two CRD files still say they are not in
   the kustomization. They are contract files, so they were left.
8. **The `serving` stage keeps the backend off.** The contract has one
   switch for the backend (`POLICY_OPERATOR_URL`); the stages tie it to the
   pod templates, for the reason given with them.
