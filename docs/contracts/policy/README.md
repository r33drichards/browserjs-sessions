# Session policy contracts

What the six tracks of the session-policies plan build against. Design:
[../../plans/2026-10-02-session-policies-design.md](../../plans/2026-10-02-session-policies-design.md).
Tracks: [../../plans/2026-10-02-session-policies-tracks.md](../../plans/2026-10-02-session-policies-tracks.md).

A change to anything here is a change to a contract: make it in its own pull
request, and say which tracks it affects.

| File | What it fixes | Consumed by |
|---|---|---|
| [`deploy/base/crd-sessionpolicy.yaml`](../../../deploy/base/crd-sessionpolicy.yaml) | the `SessionPolicy` custom resource | A, B, C |
| [`deploy/base/crd-apitoken.yaml`](../../../deploy/base/crd-apitoken.yaml) | the `APIToken` custom resource | E |
| [`json-policy.schema.json`](json-policy.schema.json) | the JSON policy format | A, D, F |
| [`json-to-rego.md`](json-to-rego.md) | how JSON becomes Rego, exactly | A |
| [`examples/`](examples/) | five policies; for each the Rego it compiles to and decisions it must give | A (tests), D (presets), F (docs) |
| [`rego-contract.md`](rego-contract.md) | package, rule, input, decision path, tenant checks, bundle layout | A, B |
| [`input-sample.json`](input-sample.json) | an `mcp_tools` input as mcp-js sends it | A, D |
| [`shell-execute-input.md`](shell-execute-input.md), [`tools/`](tools/) | the `mcp_tools` input for `shell_execute` and `shell_process`: argument schemas, five Rego policies with their cases | A, D, F |
| [`decision-module.rego.tmpl`](decision-module.rego.tmpl) | the platform module generated per session | A |
| [`system-authz.rego`](system-authz.rego) | who may call OPA's API | B |
| [`capabilities.json`](capabilities.json), [`capabilities-allowlist.txt`](capabilities-allowlist.txt) | the built-ins tenant Rego may use | A, B |
| [`opa-config.yaml`](opa-config.yaml) | OPA's configuration | B |
| [`operator-api.yaml`](operator-api.yaml) | the operator's HTTP API | A, B (OPA), C |
| [`backend-api.yaml`](backend-api.yaml) | the backend's HTTP API additions | C, D, E, F |
| [`deploy.md`](deploy.md) | workload names, labels, ports, images, secrets, pod template change, NetworkPolicy | A, B, C, E |
| [`terraform-provider.md`](terraform-provider.md) | provider configuration and resource schemas | F |
| [`spike/`](spike/) | the scripts of the phase 0 spikes | anyone re-running them |

## Checking the examples

Every `examples/<name>.cases.json` is a list of `{name, input, allow}`.
`spike/run-cases.py` rewrites `examples/<name>.rego` into a tenant package,
adds the decision module, checks both under the capabilities file, and
evaluates every case the way OPA will be asked:

```
python3 docs/contracts/policy/spike/run-cases.py "$(command -v opa)" docs/contracts/policy
86/86 cases pass
```

The Rego policies for the shell tools (`tools/examples/`, written by hand, no
JSON form) have a runner of their own, which also applies the operator's
tenant checks:

```
python3 docs/contracts/policy/tools/run-cases.py "$(command -v opa)" docs/contracts/policy
139/139 cases pass
```

Track A's translator must produce each `examples/<name>.rego` from
`examples/<name>.policy.json` byte for byte, and its tests must run the cases.

## Versions

Everything here was checked with OPA 1.9.0 (`Rego Version: v1`). Track B pins
the OPA image; if it pins another version it regenerates `capabilities.json`
from `capabilities-allowlist.txt` (below) and re-runs the cases and the
spikes in section 11 of the design.

```
opa capabilities --current | python3 -c '
import json, sys
allow = set(open("docs/contracts/policy/capabilities-allowlist.txt").read().split())
c = json.load(sys.stdin)
c["builtins"] = [b for b in c["builtins"] if b["name"] in allow]
json.dump(c, sys.stdout, indent=2, sort_keys=True)' > docs/contracts/policy/capabilities.json
```
