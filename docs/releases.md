# Releases

How a change reaches users without breaking them: a canary before the
merge, a canary before and after the deploy, and an automatic way back. One
cluster, no second environment, no service mesh; what that buys and what it
does not is at the end.

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
   3. **applies** `deploy/gke`, and waits for the rollouts as before;
   4. checks that **what is pinned is what runs**, and waits for the warm
      pool to be replaced;
   5. runs **the canary** against the result, on an ordinary session;
   6. on success **records** the commit as the last good release; on any
      failure from the apply on, **rolls back** to the last good release and
      runs the canary again to say whether the product is whole.
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
| not set, or `off` | any | applies and verifies nothing afterwards, as before this existed, with a warning that says so |
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

## The backend, the operators and the site

| | Rollout | If the new one does not come up |
|---|---|---|
| site | `maxUnavailable: 0`: the new pod is ready before the old one goes | the old one keeps serving; the run fails at its rollout and rolls back |
| OPA | rolling, two replicas, a disruption budget (unchanged) | the same |
| backend | `Recreate` (unchanged) | **down** from the moment the old pod stops until the rollback has the old one back: the rollout's five minutes at worst, plus the rollback |
| policy operator, billing operator | `Recreate` (unchanged) | policies keep being enforced from OPA's last bundle; usage is not sent (free time) |

**Why the backend is not rolled out with `maxUnavailable: 0`.** That needs
two backends at once, and the backend must be exactly one: VNC tickets and
idle tracking are in its memory, the per-user create lock and the warm
pool's claim recovery assume a single process
(`backend/cmd/server/main.go`). Two for a minute would hand out a ticket
one of them cannot redeem and could adopt one warm pod twice. Making it
safe means moving that state out of the process; until then a backend that
fails to start costs minutes of downtime, bounded by the automatic
rollback. What protects users from a backend that starts and is wrong is
the canary after the apply.

**A traffic-split canary** (a second backend behind a canary host that only
the canary token uses) was considered and not built: it is the same two
backends at once, plus a route, a DNS name and a certificate name in
`infra/main`. It becomes cheap once the backend can run twice.

## Rolling back

**Automatically**, when the canary is on and anything fails from the apply
on: `hack/release.sh rollback <commit>` with the commit in the ConfigMap
`release` (the last release the canary passed; `cluster info` shows it under
"Release"). It takes `deploy/` as that commit has it (`git archive`, so
nothing of the failed commit is used), checks its pins, applies
`deploy/gke`, waits for every Deployment, checks that each runs that
commit's digests, and runs the canary. The run fails either way; its
summary says "rolled back, and the canary passes" or "rolled back, and the
canary still fails: needs a person".

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

The canary script and the kind gate have run (in CI, on kind). These have
run nowhere, because nothing here may deploy to production:

1. `test/canary.py` against production: the app's redirect status, sleep
   with `stateSaved`, and the page's memory surviving a restore.
2. The canary session on GKE: a cold start with other digests under gVisor
   and the admission policies, and **whether a session node has room** for
   one more pod beside the warm pool of 7 (if not, it stays pending and the
   check "it runs" fails after 5 minutes: the release stops with nothing
   changed, and the warm pool's size or the quota is what to look at).
3. `hack/release.sh verify-deployments`, `verify-session` and
   `wait-warm-pool` against real objects (the image IDs GKE reports; that
   the pool replaces its pods on a template change, as
   `updateStrategy: Recreate` says).
4. The rollback, end to end.
5. `hack/release.sh` with no arguments (the pin from the registry needs
   `gcloud`; the rest needs a pull request it may merge).
6. The time it all takes inside the job's 55 minutes; the worst case (a
   canary session, a failed apply, a rollback, and a canary) is near it.
