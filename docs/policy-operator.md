# The policy operator

`images/policy-operator/` is track A of the
[session-policies plan](plans/2026-10-02-session-policies-tracks.md): a
[kopf](https://kopf.readthedocs.io/) operator that turns every
`SessionPolicy` in `browserjs-sessions` into one OPA bundle, serves that
bundle to the shared OPA, and writes back what happened as `status`. Design:
[section 4.6](plans/2026-10-02-session-policies-design.md). Contracts:
[`contracts/policy/`](contracts/policy/README.md).

It is the only place in the product that knows what a valid policy is. The
backend, the editor and the Terraform provider all ask it.

## What is in the directory

| Path | What |
|---|---|
| `policy_operator/translate.py` | `translate(policy) -> (rego, warnings)`: `json-to-rego.md`, to the byte |
| `policy_operator/check.py` | `check(cfg, kind, source, session_id=None) -> Validation`: size, JSON schema, translation, the six tenant checks, the hash. `evaluate(...)` for the editor's Test |
| `policy_operator/opa.py` | every run of the `opa` binary: a temporary directory, a time limit, nothing inherited but `PATH` |
| `policy_operator/bundle.py` | the bundle's directory and `opa build` under the capabilities file |
| `policy_operator/operator.py` | the state: sessions, the published bundle, the first pass, what each replica has loaded |
| `policy_operator/status.py` | `status`, as the CRD describes it, as pure functions |
| `policy_operator/server.py` | the HTTP API of `operator-api.yaml` (aiohttp, port 8080) |
| `policy_operator/handlers.py` | the kopf handlers; `kopf run -m policy_operator` imports them |
| `policy_operator/kube.py` | the one direct call to the API server: the list of policies at start |
| `policy_operator/cli.py` | `python -m policy_operator`: the bundle without a cluster |
| `tests/` | see "Tests" |
| `flake.nix`, `flake.lock` | the dev shell: Python 3.12 with the dependencies, OPA 1.9.0 as released |
| `requirements.in`, `requirements.txt` | what the image installs, locked with hashes |
| `Dockerfile`, `Dockerfile.dockerignore` | the image |

## What it does

**At start.** It starts listening on 8080, lists every `SessionPolicy`,
checks each, builds one bundle of all that pass and publishes it. Until
then `/readyz` and the bundle endpoint answer 503, so a restarting operator
never hands OPA a bundle with sessions missing; the replicas keep what they
have. If the list or the first build fails, the process exits and is
restarted.

**On create, on a change of `spec`, on resume.** `check` runs. If the spec
passes, its module replaces the session's in the bundle and the bundle is
published under the next revision (`<process start>-<n>`). If it does not,
the module in `status.rego` stays in force: it is checked again (status is
a record, and OPA or the capabilities may have changed since), and if there
is none, or it no longer passes, the session is left out, which OPA answers
as undefined and mcp-js treats as a denial. The handler then waits up to
five seconds for every ready OPA replica to report the hash, and writes
`status`.

**Every five seconds, per resource.** A timer compares what the ready
replicas of Service `opa` report (`GET /v1/data/browserjs/loaded`, read once
a second at most for all resources together) with `status.hash` and writes
`Loaded`, `Ready`, `loaded` and `lastAppliedTime` when they change. This is
what notices a new or restarted replica.

**On delete.** The session leaves the bundle, and kopf's finalizer
(`browserjs.dev/policy-operator`) lets the resource go.

A bundle is published only when it differs from the one before: a resume, or
an edit that compiles to the same module, publishes nothing.

### `status`

| Field | Written |
|---|---|
| `observedGeneration` | on every reconcile |
| `rego`, `hash`, `regoGeneration` | only when the spec compiles; otherwise left as they are |
| `errors` | the spec's errors (at most 50), `[]` when it compiles |
| `warnings` | the translator's (at most 50), `[]` when the spec does not compile |
| `loaded.replicas`, `loaded.total` | ready replicas serving `status.hash`, and ready replicas |
| `loaded.revision` | the bundle revision that first carried `status.hash`; kept across operator restarts |
| `lastAppliedTime` | when `Loaded` last became `True`, or stayed `True` for a new hash |

| Condition | `True` | `False`, with reason |
|---|---|---|
| `Compiled` | `Compiled` | `CompileError` (message: the first error); `BundleBuildFailed` (the spec passed alone but `opa build` refused the whole bundle; the previous bundle stays published) |
| `Loaded` | `AllReplicas`, message `2/2 replicas` | `Pending` (`1/2 replicas`); `NoReplicas`; `NoPolicy` (nothing in force: the session is denied) |
| `Ready` | `Ready` | the reason of whichever of the two is false, `Compiled`'s first |

Every condition carries `observedGeneration` and `lastTransitionTime`,
which moves only when the condition's `status` changes. A spec that does
not compile over a policy that did is `Compiled=False`, `Loaded=True`,
`Ready=False`: the last good policy is on every replica, and it is not the
one in `spec`.

kopf's own bookkeeping is in annotations prefixed
`policy-operator.browserjs.dev/`, not in `status`. The annotation that
remembers the last handled spec holds a SHA-256 of `spec.source` in place
of the source, so a 64 KiB policy cannot push the resource over the limit
on annotations.

## Configuration

| Variable | Default | |
|---|---|---|
| `BUNDLE_TOKEN`, `OPA_TOKEN`, `OPERATOR_API_TOKEN` | none; the operator refuses to start without all three | Secret `policy-tokens` (`deploy.md`) |
| `POLICY_NAMESPACE` | `browserjs-sessions` | where the policies are listed at start; `kopf run --namespace` must name the same |
| `OPA_SERVICE`, `OPA_PORT` | `opa`, `8181` | whose EndpointSlices are OPA's replicas; the port is the slice's own when it names one |
| `HTTP_PORT` | `8080` | |
| `OPA_BIN` | `opa` (`/usr/local/bin/opa` in the image) | |
| `POLICY_CONTRACT_DIR` | `/contracts` in the image, `docs/contracts/policy` in a checkout | `capabilities.json`, `decision-module.rego.tmpl`, `json-policy.schema.json` |

What the Deployment (track B) has to provide beyond `deploy.md`:

- **A writable `/tmp`.** Every run of `opa` works in a temporary directory.
  With `readOnlyRootFilesystem: true` the pod needs an `emptyDir` at `/tmp`
  (or `TMPDIR` pointing at one).
- **The CRD before the operator.** kopf's scanning is turned off (below), so
  the resources are looked up once, at start. If `sessionpolicies.browserjs.dev`
  does not exist yet the list at start fails with 404, the process exits, and
  the next start finds it.
- The image runs as `65532:65532`.

## Tests

```
cd images/policy-operator
nix develop -c pytest
```

193 tests, about fifteen seconds, no cluster and no Docker. The dev shell
takes OPA 1.9.0 from the release (checksums in `flake.nix`), because
`nixpkgs#open-policy-agent` does not build here (design, section 11).

| File | What it shows |
|---|---|
| `test_translate.py` | each `examples/<name>.policy.json` becomes `examples/<name>.rego` byte for byte; ordering, sets, the URL expression, warnings |
| `test_check.py` | all 86 cases of the five examples through the real `opa`, as a tenant package behind the decision module; the corpus of `spike/tenant-guard.py` and more (a second package clause, `data` in a rule head, a default, a function argument, an `every`, an `else`, a comprehension, `with` on a built-in, the built-ins that are not on the allow-list…) refused; legitimate modules accepted and rewritten; positions of errors; the hash; `evaluate`, its timeout included |
| `test_bundle.py` | the layout, the `loaded` document, byte stability, the build under the capabilities file |
| `test_operator.py` | 503-until-first-pass, revisions, last good from memory and from `status.rego`, a hostile `status.rego`, removal, a build that fails; the loaded check against fake OPA replicas |
| `test_status.py` | every field and condition of `status` |
| `test_server.py` | the HTTP API with aiohttp's test client: tokens, 503, ETag, 304, long polling, validate, evaluate, schema, 400 and 413 |
| `test_handlers.py` | the kopf handlers called as functions with fake `spec`, `status`, `patch` |
| `test_startup.py` | the list at start (pagination, failure), kopf's settings, missing tokens |
| `test_integration.py` | two real `opa run --server` processes, with the contract's `opa-config.yaml` and `system-authz.rego`, long-polling the operator's API: not ready before the first pass, an edit in force with nothing restarted, a broken edit, a delete |
| `test_kopf_run.py` | the whole operator under `kopf run`, against an API server faked in memory (`fake_kube.py`: discovery, list, watch, patches, the status subresource, finalizers) and two real OPA servers |

## Without a cluster

Given a directory of `SessionPolicy` YAML files, this prints the bundle the
operator would publish for them (who is in it, with errors and warnings,
then every file) and writes it:

```
cd images/policy-operator
nix develop
python -m policy_operator bundle <directory> -o browserjs.tar.gz
```

To see a second OPA answer the contract's example cases from it:

```
python -m policy_operator example-resources /tmp/policies     # the five examples, as s-exam1 … s-exam5
python -m policy_operator bundle /tmp/policies -o /tmp/browserjs.tar.gz
opa run --server --addr 127.0.0.1:8181 -b /tmp/browserjs.tar.gz &
python -m policy_operator run-cases http://127.0.0.1:8181
86/86 cases pass
```

`run-cases` asks as mcp-js does (`POST
/v1/data/browserjs/decision/<session>/mcp_tools`, allowed only when
`result.allow` is `true`). `test_integration.py` runs exactly this.

## The image

```
docker build -f images/policy-operator/Dockerfile .
```

The build context is the **repository root**: the contract files are copied
from `docs/contracts/policy/` into `/contracts`, not duplicated in the
source. `images/policy-operator/Dockerfile.dockerignore` replaces the root
`.dockerignore` for this build. In `images.yml` (track B) the entry
therefore needs `context: .` and
`file: images/policy-operator/Dockerfile`; the existing entries have no
`file`.

- Base: `python:3.12-slim` by digest.
- Dependencies: `requirements.txt`, installed with `--require-hashes`.
  Regenerate with the command at the top of `requirements.in`. The versions
  are the dev shell's, so the tests run what the image runs.
- OPA: `/opa` copied from `openpolicyagent/opa:1.9.0-static` by digest. It
  must be the version the `opa` Deployment runs; change the two together and
  regenerate `capabilities.json` (contracts README).
- The last build step runs `check` on a policy inside the image, so an image
  whose OPA, contract files and package do not fit together does not build.

## Decisions kopf forced

- **Scanning is off** (`settings.scanning.disabled`). Left on, kopf lists
  and watches the cluster's namespaces, which the operator's Role does not
  allow. kopf 1.44.6 retries that 403 nine times before falling back to the
  namespace on its command line, and handles nothing meanwhile:
  `test_kopf_run.py` showed no status written for over thirty seconds. With
  scanning off, the `customresourcedefinitions` ClusterRole of `deploy.md`
  is not used; it does no harm.
- **The first pass is the operator's own list**, not kopf's resume
  handlers: kopf has no signal for "every existing object has been resumed",
  and the bundle endpoint needs one. The resume handlers still run, find the
  spec they already checked, publish nothing, and write `status`.
- kopf logs every handler call of every resource at INFO, the timer's
  included; `kopf.objects` is set to WARNING.

## Contract deviations

None changes a contract file. Each is the smallest thing that worked, and
each wants a decision in a contract pull request.

1. **`json-to-rego.md`, a policy that allows nothing.** The contract writes
   no `operation_allowed` when `allow.operations` and `allow.rules` are both
   empty, and says `allow_empty` is a warning that does not stop a save. The
   module it describes does not compile (`rego_type_error: undefined function
   operation_allowed`), so `{"version": 1}` could not be saved. The
   translator appends one block in that case and no other:

   ```
   # Nothing is allowed.
   operation_allowed(_) := false
   ```

   Every call with an operation is denied and an empty list of operations is
   allowed, as the contract's semantics have it. The five examples are
   unaffected.
2. **`json-to-rego.md`, `denied_and_allowed` and `"*"`.** Read literally, an
   operation in `deny.operations` is "in allow" whenever `allow.operations`
   holds `"*"`, and `examples/no-scripting` would warn twice. The translator
   warns only when the operation is named in `allow.operations` or has a
   rule. `rule_shadowed` does count `"*"`.
3. **`operator-api.yaml`, `/v1/evaluate` sizes.** The contract gives no
   413 for evaluate. A body over 1 MiB + 128 KiB is answered 413; an `input`
   over 1 MiB inside a smaller body, 400.
4. **`operator-api.yaml`, evaluation errors.** OPA's own codes
   (`eval_conflict_error`, …) are reported as `eval_error` with OPA's
   message, and a run past the deadline as `eval_timeout`.
5. **`rego-contract.md`, the bundle's file names.** The operator builds from
   the layout in the contract; `opa build` writes the modules to the archive
   as `/tenant/<id>.rego` and `/decision/<id>.rego`, reformatted, with the
   data merged into `/data.json` and `rego_version` added to `.manifest`.
   What OPA loads is the same.
6. **Size.** `size_error` counts UTF-8 bytes (`rego-contract.md`: 65536
   bytes); the CRD's `maxLength` counts characters. A source of fewer than
   65536 characters and more than 65536 bytes is admitted by the API server
   and refused by the operator.
7. **Diagnostics of generated Rego.** For `kind: json`, an error OPA finds
   in the generated module has no `row` and `col` (they would be positions
   in Rego the author never saw) and its message begins "in the generated
   Rego:". Nothing the schema admits is known to cause one. An invalid
   regular expression in `pattern` is not one: `opa check` accepts it, and
   at evaluation the rule does not match, so the operation is denied.

## Not verified without a cluster

- The CRD on a real API server: that kopf patches `status` through the
  subresource, and that the API server accepts the patch against the CRD's
  schema. Both are shown only against `fake_kube.py`.
- RBAC: that the Role of `deploy.md` is enough (list at start, watch, patch
  for the finalizer and annotations, `status` patch, EndpointSlices, events).
- EndpointSlices as a real cluster writes them, and the operator reaching
  the replicas' pod addresses through the NetworkPolicy.
- The image: it has not been built. The Dockerfile's parts were checked
  separately (the lock file resolves for Python 3.12 on linux/amd64, both
  digests exist, the OPA image has `/opa`).
- kopf's liveness endpoint on 8081, and the behaviour of a pod restart with
  thousands of policies.

## Known limits

- A `SessionPolicy` that has no finalizer yet and is deleted in the instant
  between the operator's list at start and kopf's first watch stays in the
  bundle until the operator restarts. The session it names no longer exists.
- If `opa build` refuses the first bundle although every policy passed
  alone, the operator does not become ready and exits; OPA keeps what it has.
  Policies are in separate packages and cannot name each other, so nothing
  is known to cause this.
- A new or restarted OPA replica shows in `Loaded` within five seconds, not
  at once.
