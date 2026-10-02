# Session network rules: contracts

What the tracks of the network-rules plan build against. Design:
[../../plans/2026-10-02-session-network-policy-design.md](../../plans/2026-10-02-session-network-policy-design.md).

A change to anything here is a change to a contract: make it in its own pull
request, and say which tracks it affects.

| File | What it fixes | Consumed by |
|---|---|---|
| [`network-policy.schema.json`](network-policy.schema.json) | the JSON form of a session's network rules, version 1 | N1 (operator), N3 (backend), N4 (UI), N5 (Terraform) |
| [`translation.md`](translation.md) | how rules become a `NetworkPolicy`, the ceiling, what is refused | N1 |
| [`examples/`](examples/) | four rule sets and, for each, the `NetworkPolicy` it must become | N1 (golden tests), N4 (presets) |
| [`crd-sessionpolicy-network.yaml`](crd-sessionpolicy-network.yaml) | the fields `SessionPolicy` gains | N1, N2, N3 |
| [`baseline.networkpolicy.yaml`](baseline.networkpolicy.yaml) | the shared `session-pods` policy after the migration | N2 |
| [`backend-api.yaml`](backend-api.yaml) | the backend's HTTP API additions | N3, N4, N5 |

These add to the tool-call policy contracts in [`../policy/`](../policy/);
they change none of them. The operator's HTTP API
([`../policy/operator-api.yaml`](../policy/operator-api.yaml)) gains a
`network` member in the request and `network_errors` / `network_warnings` in
the answer of `/v1/validate`, and nothing else.

## Checking the examples

The examples were rendered by the rules of `translation.md` and parse as
YAML. `internet.networkpolicy.yaml` carries the same egress rules as the
`session-pods` policy has on `main` today: that equality is what makes the
migration a no-op for a session that changes nothing.
