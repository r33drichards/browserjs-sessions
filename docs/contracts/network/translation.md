# From network rules to a NetworkPolicy, exactly

The policy operator turns the `network` rules of a session
([`network-policy.schema.json`](network-policy.schema.json)) into one
Kubernetes `NetworkPolicy`. The translation is a pure function of
`(session ID, Sandbox UID, rules, platform configuration)`; the operator's
tests must reproduce every `examples/<name>.networkpolicy.yaml` from
`examples/<name>.network.json` byte for byte (session `s-abcde`, UID all
zeros, the platform configuration below).

## Platform configuration

| Setting | Value on GKE | Meaning |
|---|---|---|
| `FORBIDDEN_CIDRS` | `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`, `100.64.0.0/10`, `169.254.0.0/16` | the ceiling: never reachable from a session, whatever its rules say. The cluster (nodes `10.10.0.0/20`, pods `10.20.0.0/16`, Services `10.30.0.0/20`), any peered network, and the metadata address `169.254.169.254` are inside it. |
| `EXTRA_FORBIDDEN_CIDRS` | empty | more of the same, for example the control plane's public address |
| `DNS_PEERS` | pods `k8s-app: kube-dns` and `k8s-app: node-local-dns` in `kube-system`, and `169.254.20.10/32` | where a session resolves names (the three peers `session-pods` has today) |

The ceiling is not part of a user's rules and is not shown as editable.

## The object

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: <session>-egress
  namespace: browserjs-sessions
  labels:
    app.kubernetes.io/managed-by: policy-operator
    browserjs.dev/session: <session>
  annotations:
    browserjs.dev/network-mode: <mode>
  ownerReferences:            # the Sandbox: deleting the session deletes this
    - {apiVersion: agents.x-k8s.io/v1beta1, kind: Sandbox, name: <session>, uid: <uid>}
spec:
  podSelector:
    matchLabels:
      app: browserjs-session
      agents.x-k8s.io/sandbox-name-hash: <hash>
  policyTypes: ["Egress"]
  egress: <rules>
```

- `<hash>` is the label the Agent Sandbox controller puts on every pod of a
  Sandbox: FNV-1a (32 bit) of the Sandbox's name, as eight lower-case hex
  digits (`controllers/sandbox_controller.go`, `NameHash`, in
  kubernetes-sigs/agent-sandbox v1.0.5). `s-abcde` gives `d233a3f0`;
  `session` gives `c35f4cf7`, which is the template hash the cluster shows.
- **Collisions.** The hash is 32 bits. Before writing, the operator computes
  it for every Sandbox in the namespace. If two live Sandboxes share a hash,
  both get `egress: []` and `NetworkApplied=False`, reason `HashCollision`:
  a policy that selects two pods would give each the other's allowances.
- Ingress is never in this object. `policyTypes` has `Egress` only, so the
  shared `session-pods` policy stays the one statement about ingress.

## Rules, in this order

1. **DNS**, when `dns` is true (the default for `internet` and
   `allowlist`; never for `none`): one rule to `DNS_PEERS`, ports 53 UDP
   and 53 TCP, written as in the examples.
2. **`mode: none`**: nothing else. With no DNS rule either, `egress: []`.
3. **`mode: internet`**: one rule, no ports, to `ipBlock 0.0.0.0/0` with
   `except` = the forbidden ranges plus each `deny[].cidr` that is not
   already inside a forbidden range.
4. **`mode: allowlist`**: one rule per `allow` entry, in the order written:
   `ipBlock` with the entry's `cidr`; `except` = the forbidden ranges that
   lie strictly inside it (none for most entries); `ports` as written, with
   `protocol` defaulting to `TCP`. No `ports` key when the entry has none.

`except` lists are de-duplicated and sorted by address, then prefix length.

## What is refused

Checked by the operator, at validate time and again at reconcile. Each error
has a JSON pointer into the rules.

| Code | When |
|---|---|
| `schema` | the rules do not match the schema |
| `forbidden_range` | a `cidr` in `allow` equals or lies inside a forbidden range |
| `not_canonical` | a `cidr` has host bits set (`10.1.2.3/24`) |
| `ipv6_unsupported` | a `cidr` is IPv6 |
| `bad_port_range` | `endPort` is below `port` |
| `hosts_unavailable` | an entry names a `host` and the cluster has no hostname enforcement (always, in version 1) |

A `deny` entry inside a forbidden range is a warning (`redundant`), not an
error. Rules that are refused are not applied: the last rules that were
applied stay in force, as with a policy that does not compile.

## What the platform always has, whatever the rules

In the shared `session-pods` policy ([`baseline.networkpolicy.yaml`](baseline.networkpolicy.yaml)),
which selects every session pod and which users cannot change:

- ingress from the backend on 6080, 8080 and 8081, and from nothing else;
- egress to the shared OPA on 8181, once tool-call policies are on.

`mode: none` therefore leaves a session that the backend can still show and
drive, that can still ask for tool-call decisions, and that can reach
nothing else. mcp-exec, when it comes, listens on loopback, which no
NetworkPolicy touches.
