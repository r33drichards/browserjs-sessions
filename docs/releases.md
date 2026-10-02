# Releases

How a change reaches users without breaking them: a canary before the
merge; at the deploy, the new backend checked on standby before users reach
it, the new site on a quarter of its pods, new session images on one
session; a canary after; and an automatic way back. One cluster, no second
environment, no service mesh.

## What does the releasing, and why

| | Released how | By |
|---|---|---|
| **site** | **canary by weight**: 1 pod in 4 is the new version, checked, then 2, then all; a failed check takes the new pods away | Argo Rollouts |
| **backend** | **blue-green**: the new pod on standby beside the old, the whole canary run against it, then the switch; a failed check removes it, and users never met it | Argo Rollouts |
| **session images** (browser, mcp-js) | **one canary session** on the new digests before the warm pool gets them | the backend's `canary` create option, the deploy workflow |
| operators, OPA, Pomerium, Dex, CRDs, routes | applied, then the canary against the whole, then back to the last good commit if it fails | the deploy workflow |

**Why the backend is blue-green and not weighted.** Weighted means two
backends answering users at once, and this backend cannot: a VNC ticket is
in the memory of the process that issued it (the next request, the
websocket, would land on the other one and be refused), and so is idle
tracking (the one that runs the sweep would put to sleep the sessions that
are busy on the other). No router fixes that; it needs those two kept
outside the process (signed tickets, activity on the Sandbox), which is
backend work of its own. Blue-green needs much less: the second backend
must only **do nothing unasked** while it is checked, which it now does
(`backend/cmd/server/active.go`: the idle sweep, the warm pool's claim
recovery and billing's sweeps start when the pod's label
`browserjs.dev/role` says `active`). The cut-over itself is what a restart
has always been (tickets and idle clocks start again), without the gap.

**Why the site's weights need no router.** Its four pods are behind one
Service, which Pomerium sends every visitor to; one new pod among four is a
quarter of the connections. That is a real share of real traffic, and rough:
Pomerium keeps connections open, so it is a quarter of connections, not of
requests. With two users the traffic says nothing either way, so the
decision is made by a check that asks the new pods directly (twenty requests
for the front page and the health path), not by a success rate. A broken
new site is therefore served to about a quarter of visitors for the few
seconds its check takes; that is what a canary by weight is.

### Flagger or Argo Rollouts

Both were read against their current documentation (2026-10-02). Neither
needs the other's ecosystem: Flagger runs without Flux, Argo Rollouts
without Argo CD (only its controller is installed here).

| | Flagger 1.45 | Argo Rollouts 1.10 |
|---|---|---|
| Weighted traffic needs | one of its providers: "Istio, Linkerd, App Mesh, NGINX, Skipper, Contour, Gloo Edge, Traefik, Kuma, Gateway API, Apache APISIX, Knative" ([deployment strategies](https://docs.flagger.app/usage/deployment-strategies)). Pomerium is not one | a traffic router (Gateway API by a plugin, and others), **or none**: "the Rollout makes a best effort attempt to achieve the percentage listed in the last `setWeight` step between the new and old version" by the ratio of pods ([canary](https://argo-rollouts.readthedocs.io/en/stable/features/canary/)) |
| Without a router | blue-green only, "with Kubernetes L4 networking" | blue-green, or the canary by pod ratio used for the site |
| Its check | webhooks, called by Flagger; for a command, its load tester has to be deployed ([blue/green tutorial](https://docs.flagger.app/tutorials/kubernetes-blue-green)); metric checks are Prometheus queries, and its Gateway API tutorial installs Prometheus for them ([Gateway API](https://docs.flagger.app/tutorials/gatewayapi-progressive-delivery)) | an `AnalysisTemplate` whose metric is a Kubernetes **Job**: our canary script, as it is, in a pod. No Prometheus |
| What it does to the workload | makes `deployment/<name>-primary` and Services `<name>`, `<name>-primary`, `<name>-canary`; "the target deployment is scaled to zero"; the pods users reach are labelled `app=<name>-primary` ([how it works](https://docs.flagger.app/usage/how-it-works)) | a `Rollout` that takes its template from the Deployment (`workloadRef`, [migrating](https://argo-rollouts.readthedocs.io/en/stable/migrating/)); pods keep their labels; the Services keep their names and gain a selector |
| For this repository | every NetworkPolicy that says `app: backend` (the sessions' ingress, the policy operator's), `cluster info`, and the checks would have to learn `backend-primary`; the Services would be Flagger's, not the manifests' | the base, `deploy/local` and the kind tests are untouched: the Deployments stay, and only `deploy/gke` adds Rollouts beside them |
| Telling the new pod it is not serving yet | nothing built in | `previewMetadata` and `activeMetadata`: labels put on the pods and changed in place at promotion ([ephemeral metadata](https://argo-rollouts.readthedocs.io/en/stable/features/ephemeral-metadata/)), which is how the backend knows to stand by |
| A failed check | "the green version is scaled to zero and the rollout is marked as failed" | before promotion the switch never happens ([blue-green](https://argo-rollouts.readthedocs.io/en/stable/features/bluegreen/)); the Rollout is `Degraded` |
| SandboxTemplate, SandboxWarmPool | not a kind it handles | not a kind it handles |
| Footprint | the controller, the load tester, and Prometheus for metric checks | one controller pod (25m CPU and 96Mi asked for here) |

**Argo Rollouts**, for four reasons that are all about this system: the
check we have is a script, and a Job runs it as it is; it can weight the
site with no router; it leaves names, labels and the other overlays alone;
and it can tell a pod whether it is the one serving, which the backend
needs.

**A router for true weights on the backend's side** was looked at in the
order asked, and none is built, because the backend cannot use one yet:

1. *GKE's Gateway API, internal class, between Pomerium and the backend.*
   Flagger and Argo Rollouts can both drive its HTTPRoute weights. It needs
   a proxy-only subnet and an internal load balancer in `infra/main`, the
   NetworkPolicy of the backend opened to that subnet instead of to
   Pomerium's pods, and backend timeouts raised for MCP streams and the VNC
   websocket. Its cost and its behaviour with our long streams were **not
   measured**.
2. *An in-cluster Gateway (Envoy Gateway, Contour, NGINX Gateway Fabric,
   Traefik).* A controller and a proxy on the one system node, which is
   already about four fifths asked for, in the path of every request.
3. *Pomerium's own upstream weights.* A route can name several upstreams
   with weights, in its configuration file only
   ([load balancing](https://www.pomerium.com/docs/reference/routes/load-balancing));
   nothing can change them but an edit of that file, and here a changed
   file is a new ConfigMap and a restart of Pomerium. Driving it would be a
   controller of our own, which is the hand-rolling this was meant to avoid.

When the backend can run twice, (3) with Argo's pod-ratio canary (no router
at all, as the site) is the first thing to try, then (1).

**What it costs.** One more controller pod. During a release: a second
backend pod for the minutes of its check (100m CPU, 128Mi), four site pods
at all times instead of one (10m and 32Mi each), and the check's own pod.
On the system node that is roughly 55m more CPU asked for at rest and 165m
during a release. Whether that still fits beside OPA and the operators on
today's one node is **not verified** (see the end).

## The flow

```
hack/release.sh            # or: hack/release.sh backend=sha256:… site=sha256:…
```

1. **Pin.** The digests the registry's `main` tags point at (or the ones
   given) are written into `deploy/gke` (`hack/pin-images.sh`), on a branch.
2. **Pull request.** Opened with `gh`. Its checks are the pre-merge gate:
   among them **canary kind**, the canary against the whole system built
   from the branch on a kind cluster.
3. **Merge**, when every check has passed. A failed check stops here.
4. **Deploy workflow**, started on `main`. With the canary on, it:
   1. prints **what changes**: per image, what runs and what is pinned;
   2. tries **new session images on one canary session**, before anything
      is applied. A failure stops the run with nothing changed;
   3. **applies** `deploy/gke`. Argo Rollouts then releases the **backend**
      (standby, the canary against it, the switch) and the **site** (a
      quarter, a check, half, all); a version that fails is taken away;
   4. checks that **what is pinned is what runs**, and waits for the warm
      pool to be replaced;
   5. runs **the canary** against the result through the edge, on an
      ordinary session;
   6. on success **records** the commit as the last good release; on any
      failure from the apply on, **rolls back** the rest to the last good
      release and runs the canary again to say whether the product is whole.
5. `hack/release.sh` follows the run and says released or failed. The run's
   summary has the table of what changed, each canary check, and what was
   promoted or rolled back.

`hack/release.sh --no-pin` releases `main` as it is (a manifest change with
no new image). `DRY_RUN=1` pins locally and stops. Steps 1 to 3 can be done
by hand, and step 4 is then Actions, **deploy**, Run workflow, confirm
`deploy`: the canary is the workflow's, not the script's.

## Turning it on: the owner's one step

1. In the app, signed in as an admin of the deployment (an address in
   `ADMIN_EMAILS`): **API tokens**, new token, with the scopes
   `sessions:read`, `sessions:write`, `sessions:connect`, `policies:read`
   and `policies:write`, 365 days.
2. Repository Settings, Secrets and variables, Actions: the secret
   **`CANARY_API_TOKEN`** = that token.
3. The same page, Variables: **`CANARY`** = `on`.

The token must be an admin's: the canary session of step 4.2 is an option
only the deployment's admins have. It expires: the canary then fails at its
first check ("the token is exchanged"), which says so; make another.

| `CANARY` | `CANARY_API_TOKEN` | A deploy |
|---|---|---|
| not set, or `off` | any | the rollouts still happen (a backend that does not become ready, or a site that does not answer, is still taken away), but the backend's check passes without checking, nothing is verified afterwards and nothing is rolled back; a warning says so |
| `on` | not set | refused at once, before anything is touched, naming the secret |
| `on` | set | the flow above |

Nothing in a log or a summary is secret: the token is never printed, and
the canary's output has email addresses and tokens removed from it. The
repository is public; so are its logs.

**The first release with the canary on** has no last good release to go
back to, and its running backend predates the canary session, so step 4.2
is left out and a failure is not rolled back (the summary says both). It
records itself; from the second release on, everything applies.

## The canary: `test/canary.py`

One script, Python's standard library only, for the workflow and for a
person:

```
CANARY_API_TOKEN=bjs_… test/canary.py                 # production
CANARY_API_TOKEN=bjs_… DOMAIN=example.org test/canary.py
test/canary-kind.sh                                   # the local cluster; makes its own token
```

Everything goes through the public API host with the token, as a client's
calls do. In order:

| Check | What it shows |
|---|---|
| the site answers; the app redirects to sign-in | the edge, the certificate, Pomerium, the two front doors |
| a request with no token is refused | the API host is not open |
| the token is exchanged for an access token | `/oauth/token`, the token store, the signing key; the token has its five scopes |
| sessions are listed; a session is created; it runs | the backend, the cluster API, Agent Sandbox, the pod, the disk, the session's policy made and loaded |
| `run_js` prints | the MCP route, the proxy, mcp-js |
| `browser_execute` loads a page and reads it | the browser image, Chromium, the browser's MCP server |
| `exec` runs a program given as `{bin, args}` | mcp-exec |
| the policy `browser-only` denies `exec` and allows the browser; put back, `exec` runs again | the policy API, the operator, OPA, mcp-js asking OPA on each call |
| sleep, with `stateSaved` | Pod Snapshots |
| wake, and a value left in the page's memory is still there | the restore: only a pod that came back from its snapshot has it |
| the session is deleted | cleanup; also done when anything above fails |

A session it makes is named `release-canary-…`; one left by a run that was
killed is deleted by the next. It needs one free session of the token
owner's limit, and one place on a session node while it runs.

Options (the top of the script has all): `CANARY_DIGESTS` for a canary
session, `EXPECT_STATE_SAVED=0` and `EXPECT_POLICIES=0` for clusters
without snapshots or policies, `SESSION_HOOK` for a command given the
session's ID (the workflow checks the pod's images with it).

## Session images: one canary session first

The browser and mcp-js images run in every session, and the warm pool
restarts all its pods when they change. So new ones are tried on one
session before the pool has them:

- The backend's create takes `"canary": {"browser": "sha256:…", "mcp-js":
  "sha256:…"}`. The session is started **cold** from the running blueprint
  with those digests in place of the blueprint's, in the blueprint's own
  repositories: the option can only choose another build of the same two
  images. It is honoured for the deployment's admins (by address, so an
  admin's API token has it) and is `403` for anyone else. The Sandbox
  carries the annotation `browserjs.dev/canary`.
- The workflow creates one with the pinned digests, checks that its pod
  really runs them, and puts it through the whole canary.
- Fails: the run stops. The warm pool, every running session and the
  backend are as they were. Passes: the apply updates the blueprint and the
  SandboxTemplate, and the pool is replaced.

There is no second SandboxTemplate and no second pool: a canary of one
needs neither, and a pool of one would hold a CPU of the quota for nothing.

What this does not cover: a change to the **blueprint itself** (an
environment variable, a probe, a volume) comes with the apply, not with the
canary session, which uses the running blueprint. If new session images
need the new blueprint or the new backend, the canary session cannot start
them: run the deploy with `session_canary: skip`. The canary after the
apply still runs, and still rolls back.

## The backend and the site: Argo Rollouts

`deploy/gke/argo-rollouts/` is the controller (the release's manifest,
vendored, applied before the rest). `deploy/gke/rollouts.yaml` is the two
Rollouts, their extra Services, the two checks and the checks'
NetworkPolicy. `deploy/gke/patch-rollouts.yaml` leaves the two Deployments
without pods of their own and gives the backend its labels as a file. The
base, `deploy/local` and the kind tests still run plain Deployments.

**Backend.** A change to the Deployment's template (an image, a variable, a
ConfigMap's name, a `rollout restart`) makes the Rollout:

1. start one pod of the new template, labelled `browserjs.dev/role:
   preview`, behind the Service `backend-preview`. No route of Pomerium's
   leads there; the NetworkPolicy lets in only the check. The pod answers
   requests and does nothing unasked;
2. run `test/canary.py` in a Job against `backend-preview`, as the API host
   (the `Host` header), with the token of the Secret `release-canary`: a
   real session, created, driven, restricted, slept, woken and deleted
   through the new backend;
3. passed: switch the Service `backend` to the new pod and relabel it
   `active` (it then starts its sweeps); the old pod goes 30 seconds later,
   so calls in flight on it can finish. Viewers reconnect and tickets are
   issued anew, as at any restart;
4. failed, or the pod not ready within ten minutes: remove the new pod.
   The Rollout is `Degraded`, `hack/release.sh rollout-status backend`
   fails with the check's output, and users are where they were.

The deploy workflow writes the script (ConfigMap `release-canary`, from
`test/canary.py` of the commit being deployed) and the token (Secret
`release-canary`, from `CANARY_API_TOKEN`) before the apply. Without the
Secret the Job passes, saying that it checked nothing.

**Site.** Four pods. A change makes the Rollout bring up one new pod (a
quarter), wait 20 seconds for the Service `site-canary` to settle on the
new pods (on kind a check that started at once was answered by an old pod
and passed a broken site), run the check `site-answers` against them, go to
half, wait 30 seconds, and finish. A failed
check takes the new pods away; never fewer than four serve.

**The rest** is unchanged: OPA rolls with two replicas and a disruption
budget; the two operators are `Recreate` (while one is away, policies keep
being enforced from OPA's last bundle, and usage is not sent).

To look: `cluster info`, section "Release" (the Rollouts' phase, the pods'
roles, the analysis runs). By hand: `kubectl -n browserjs-sessions get
rollouts,analysisruns`. To try a release's mechanics without a cluster that
matters: `test/release/run.sh` on kind.

## Rolling back

**The backend and the site roll themselves back**, in the sense that a
version that fails its check is never promoted: there is nothing to undo.

**The rest, automatically**, when the canary is on and anything fails from
the apply on (a failed rollout included): `hack/release.sh rollback
<commit>` with the commit in the ConfigMap `release` (the last release the
canary passed; `cluster info` shows it under "Release"). It takes `deploy/`
as that commit has it (`git archive`, so nothing of the failed commit is
used), checks its pins, applies `deploy/gke`, waits for every Deployment and
Rollout (the backend's goes back through its check, on the old version),
checks that each runs that commit's digests, and runs the canary. The run
fails either way; its summary says "rolled back, and the canary passes" or
"rolled back, and the canary still fails: needs a person".

It is deterministic because a commit of `main` names every image by digest.
What it does not undo:

- an object the failed release **added** stays (nothing is pruned);
- a **CRD** whose schema the failed release changed gets the old schema
  back, which is right for the old backend, and may drop fields objects
  written in between carry;
- Secrets and their checks (`hack/billing-secrets.sh`) are not re-run;
- the stage switches go back **with the files**: a release that moved
  billing from `off` to `meter` and failed is back at `off`.

`main` still has the failed commit: revert it, or fix forward. Until then
the next deploy tries it again.

**By hand**, from GitHub only:

1. `git revert` the pin (or the change) in a pull request and merge it.
2. Actions, **deploy**, confirm `deploy`.

**By hand, with a kubeconfig**, when GitHub is not an option:

```
kubectl -n browserjs-sessions get configmap release -o jsonpath='{.data.commit}'
hack/release.sh rollback <that commit>
CANARY_API_TOKEN=bjs_… test/canary.py
```

## Before the merge: `canary kind`

`.github/workflows/canary-kind.yml`, on every pull request that touches the
backend, the web app, an image, `deploy/` or `hack/` (a pin among them):
`hack/local-up.sh` builds every image from the branch and brings the whole
system up on kind, with session policies at `enforcing`; then
`test/canary-kind.sh` makes an API token for the local admin and runs
`test/canary.py` through Pomerium and the API host.

| Covered on kind | Not covered there |
|---|---|
| the backend, the web app in it, the API host, token exchange | **the pinned digests**: the registry is private and a pull request cannot pull from it. The same sources are built instead: "this code works", not "these bytes work" |
| create, run, `run_js`, `browser_execute`, `exec` with the real browser and mcp-js images | gVisor: the pods run under runc |
| the policy path, enforcing: the operator, OPA, mcp-js | Pod Snapshots: sleep saves nothing (`stateSaved` false) and wake starts fresh |
| sleep and wake as state changes; delete | the warm pool, and the canary create option (the local blueprint names images by tag) |
| Pomerium's routes for the API host | the real certificate, DNS, the load balancer, the site |

The script is run twice there: through Pomerium and the API host, and
straight at the backend with `API_HOST`, which is how the backend's Rollout
runs it on GKE.

**`release kind`** (`test/release/run.sh`) is the other half: Argo Rollouts
as `deploy/gke` installs it and `deploy/gke/rollouts.yaml` as it is, with
stand-ins for the two programs and for the canary. It shows that a backend
that fails its check is never switched to and is removed, that one that
passes is promoted and told it is active without a restart, that without
the token the check passes saying so, that the site goes out one pod in
four first and comes back whole when its check fails, and that only the
check's pod reaches the standby backend.

So a pin of images that were built from a `main` this check passed on is
covered twice (source here, digests in the deploy's canary); a pin of
digests that are **not** the latest build of `main` is covered only by the
deploy's canary.

## What the canary catches, and what it does not

Catches: a session that cannot be created, started, driven, restricted,
slept, woken or deleted through the API; a browser, mcp-js or mcp-exec that
does not answer; a policy that does not bind; a snapshot that does not
restore; a token endpoint or API host that is down; a Deployment that did
not roll out, or runs something else than what was pinned; a site or an
app that does not answer; a release that changes no image (the table says
"No image changes").

Does not catch:

- **The UI.** Nothing signs in: the app is checked for its redirect only.
  The web tests in CI and a person are what cover it.
- **The sessions' own host** (`sessions.<domain>`: MCP with a signed-in
  user, the screen over VNC, file transfer, uploads). The canary uses the
  API host.
- **The desktop tools**, downloads, the clipboard, anything in the browser
  image beyond loading and reading a page and running a program.
- **Billing, Stripe, Metronome.** Billing is off in production.
- **Load, and time.** One session, a few minutes: not a leak, not the idle
  sweep, not what happens at the eleventh session.
- **Sessions that existed before the release.** A running session keeps its
  old pod; one that is asleep wakes into the new backend with its old
  images. Neither is exercised.
- **Pins that are stale.** The check is "what runs is what is pinned", not
  "what is pinned is the newest build". The table of what changes is where a
  person sees that nothing did.

## Not verified until its first real run

The canary script, the kind gate and the rollouts' mechanics have run (in
CI, on kind). These have run nowhere, because nothing here may deploy to
production:

1. **The first deploy with the Rollouts.** The Deployments of the backend
   and the site lose their pods and the Rollouts' first pods come up beside
   that: a gap of the length of a backend restart, once. The first revision
   of a Rollout is not checked (there is nothing to compare it with).
2. **Room on the system node** for the controller, three more site pods
   and, during a release, a second backend and the check's pod. If the
   standby backend cannot be scheduled it is given up on after ten minutes
   and the release fails, with users untouched.
3. The backend's check **inside the GKE cluster**: the Job's pod reaching
   `backend-preview` under Dataplane V2, pulling `python:3.13-alpine` from
   Docker Hub, and a session created through a standby backend (its warm
   pool claim, its policy) while the active one serves.
4. `test/canary.py` against production: the app's redirect status, sleep
   with `stateSaved`, and the page's memory surviving a restore.
5. The canary session on GKE: a cold start with other digests under gVisor
   and the admission policies, and **whether a session node has room** for
   one more pod beside the warm pool of 7 (if not, it stays pending and the
   check "it runs" fails after 5 minutes: the release stops with nothing
   changed, and the warm pool's size or the quota is what to look at).
6. `hack/release.sh verify-deployments`, `verify-session` and
   `wait-warm-pool` against real objects (the image IDs GKE reports; that
   the pool replaces its pods on a template change, as
   `updateStrategy: Recreate` says).
7. The rollback, end to end; and a rollback to a commit from **before** the
   Rollouts (its Deployments would get pods again beside the Rollouts':
   do that one by hand, deleting the two Rollouts first).
8. `hack/release.sh` with no arguments (the pin from the registry needs
   `gcloud`; the rest needs a pull request it may merge).
9. The time it all takes inside the job's 58 minutes: a canary session, the
   backend's check, the canary after, and at worst a rollback with the
   backend's check again are close to it.
10. `billing-apply.yml` restarts the backend with `kubectl rollout restart
    deployment/backend` and waits on the Deployment, which now returns at
    once: the restart still happens (through the Rollout, with its check),
    but that workflow no longer waits for it. It is not this change's file.
