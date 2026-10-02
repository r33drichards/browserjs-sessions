# Session network rules: design

Status: proposal. Nothing here is built. Contracts:
[`docs/contracts/network/`](../contracts/network/README.md).
Companion to the tool-call policy design,
[2026-10-02-session-policies-design.md](2026-10-02-session-policies-design.md).

Anything marked **UNVERIFIED** was not confirmed from a primary source or on
the cluster, and has a spike or a test against it in section 8.

## The request

> I also want users to be able to define network policy restrictions for an
> instance at the [GKE Agent Sandbox network isolation] layer, so if they
> want to ...

The message was cut off. The three most likely endings, and what each needs:

1. "... **cut a session off from the internet**, they can." Version 1,
   `mode: none`.
2. "... **let a session reach only certain destinations**, they can."
   Version 1 for addresses and ports (`mode: allowlist`); version 2 for
   hostnames, which is what most people mean by "certain sites".
3. "... **let a session reach their own private services**, they can."
   This is opening, not restricting. It is not offered: section 4.4.

The design covers 1 and 2, and treats 2-by-hostname as the likely real
wish. Which one was meant is open question 1.

## Why this is the layer that matters

A tool-call policy decides what an agent may ask for over MCP. Once
sessions have a terminal and shell execution (XFCE, mcp-exec), anything
inside the session can open a socket without asking anyone: `curl` from a
shell, a script the agent wrote, a click on the desktop. A tool-call policy
that allows `navigate` only to `example.com` says nothing about those. The
network rules are enforced by the node's dataplane, outside the gVisor
sandbox, on every packet whatever process sent it. **Network rules are what
actually contains what a session can reach; tool-call policies shape what an
agent is asked to do.** The UI should say so where the two sit side by side.

## Summary of the proposal

- **Where the rules live.** A new `spec.network` on the session's existing
  `SessionPolicy` resource: one resource per session, one version number,
  one "managed as code" switch, one Terraform resource. It is structured
  data, not a string, and it is never Rego (section 3.3).
- **What a user can say (version 1).** `none` (no network at all),
  `internet` (today's behaviour, the default), `allowlist` (only these
  address ranges and ports), and `internet` minus a list of address ranges.
- **What enforces it.** One Kubernetes `NetworkPolicy` per session,
  `<session>-egress`, written by the kopf policy operator, owned by the
  session's Sandbox, selecting the session's pod by a label the Agent
  Sandbox controller already puts on it.
- **The change that makes it possible.** `NetworkPolicy` can only add. Today
  the shared `session-pods` policy gives every session the internet, so no
  session can have less. The shared policy shrinks to the platform's own
  needs, and DNS and the internet move into each session's own policy. The
  migration adds first and removes second, so no session loses anything at
  any point (section 5).
- **The ceiling.** Private ranges, the cluster, the node and the metadata
  address are never reachable, in any mode. Users restrict within the
  ceiling; they cannot raise it. Ingress is not theirs to change.
- **Hostnames (version 2).** A shared egress proxy with a per-session
  allow-list, with the session's `NetworkPolicy` allowing egress to the
  proxy only. GKE's `FQDNNetworkPolicy` is free and simpler to build but
  weaker, and by itself it would let a user's own DNS name point a session
  at a private address; it is the alternative, not the recommendation
  (section 6).
- **GKE's own Agent Sandbox network isolation** is one shared policy per
  `SandboxTemplate`. It cannot vary per session, and cold sessions have no
  template. We keep it `Unmanaged` and do the per-session part ourselves,
  which is what upstream tells people in our position to do (section 1.1).

---

## 1. Findings

### 1.1 What Agent Sandbox offers

GKE's concept page
(<https://docs.cloud.google.com/kubernetes-engine/docs/concepts/machine-learning/agent-sandbox>,
"Network isolation"):

> GKE Agent Sandbox implements a Default Deny network security posture for
> all sandboxed environments. [...] You can define specific network
> restrictions and allowed egress or ingress rules within your
> SandboxTemplate to provide fine-grained security for agentic workloads.

The how-to
(<https://docs.cloud.google.com/kubernetes-engine/docs/how-to/agent-sandbox>):

> by default, Agent Sandbox enforces a strict Secure-by-Default network
> posture (networkPolicyManagement: Managed). [...] Ingress is blocked from
> all sources except the designated Sandbox Router. Egress is allowed to the
> public internet, but egress to private LAN ranges (RFC 1918), internal
> cluster DNS (CoreDNS), and the Cloud Provider Metadata Server
> (169.254.0.0/16) is explicitly blocked.

The API behind it, read from kubernetes-sigs/agent-sandbox at tag `v1.0.5`
(commit `82d410e`):

- `SandboxTemplate.spec.networkPolicyManagement`: `Managed` (default) or
  `Unmanaged`. `Unmanaged`: "the controller will skip NetworkPolicy creation
  entirely, allowing external systems (like Cilium) to manage networking."
  (`extensions/api/v1beta1/sandboxtemplate_types.go`)
- `SandboxTemplate.spec.networkPolicy`: `ingress` and `egress` lists only.
  "A single shared NetworkPolicy is created per Template." The controller
  writes one object, `<template>-network-policy`, selecting pods by
  `agents.x-k8s.io/sandbox-template-ref-hash`
  (`extensions/controllers/sandboxtemplate_controller.go`).
- **Per template, never per session.** Upstream's own guide
  (`examples/policy/network-policy-management/README.md`): "No Per-Claim
  Policies: Individual `SandboxClaim` resources cannot define their own
  network policies." and "No Native L7 Filtering". `Sandbox`, `SandboxClaim`
  and `SandboxWarmPool` have no network field.
- Per-claim policies existed and were removed (PR #607) for scale: issue
  [#643](https://github.com/kubernetes-sigs/agent-sandbox/issues/643) speaks
  of "cluster instability with 100,000+ redundant policies". The roadmap
  lists "Network Policy 'Attach' at Claim Time [...] to restrict internet
  access or whitelist specific FQDNs" as planned, not built. A maintainer
  comment on #643 says hostname and layer 7 rules are expected through
  `Unmanaged` plus the CNI's own policies.
- **A Sandbox created directly gets no policy at all** (#643 confirms). That
  is our cold path.

This cluster (`cluster info` run 37030645989, 2026-10-02, server
`v1.36.4-gke.1247000`): the served `SandboxTemplate.spec` has exactly
`envVarsInjectionPolicy, networkPolicy, networkPolicyManagement,
podTemplate, service, volumeClaimTemplates`; `SandboxClaim.spec` has
`additionalPodMetadata, env, lifecycle, warmPoolRef`. Our template `session`
is `Unmanaged`. The namespace has three NetworkPolicies: `backend`,
`session-pods`, `site`.

So "the Agent Sandbox network isolation layer" is, concretely, Kubernetes
`NetworkPolicy` enforced by Dataplane V2. The managed field could express
per-session rules only as one template (and one warm pool) per rule set,
which does not work for rules a user writes. We stay `Unmanaged` and write
per-session policies ourselves; if upstream ships attach-at-claim-time, the
operator's output is the thing to swap.

### 1.2 What can express a per-session rule set here

| Mechanism | None | Internet, no private (today) | Address and port allow-list | Hostname allow-list | Deny-list | Status on this cluster |
|---|---|---|---|---|---|---|
| Kubernetes `NetworkPolicy` | yes | yes | yes | no | addresses only, as `except` | in use |
| GKE `FQDNNetworkPolicy` | with a NetworkPolicy | - | - | yes, by resolved address | no | off; free; enabling restarts `anetd` |
| `ClusterNetworkPolicy` (deny, tiers) | yes | yes | yes | no | yes | GKE preview, alpha API |
| `CiliumClusterwideNetworkPolicy` | yes | yes | yes | UNVERIFIED | yes | off; GKE now advises against it for new use |
| Egress proxy plus a NetworkPolicy | - | - | - | yes, by name | yes, by name | to build |

**Kubernetes NetworkPolicy** (<https://kubernetes.io/docs/concepts/services-networking/network-policies/>):
"Network policies do not conflict; they are additive." There is no deny:
the page lists under what you cannot do "The ability to explicitly deny
policies". Hence the baseline change. It also says "there is no way to tell
from the Kubernetes API when exactly that happens" (a policy taking effect),
and that whether a change affects an established connection "is
implementation defined".

**FQDNNetworkPolicy** (<https://docs.cloud.google.com/kubernetes-engine/docs/how-to/fqdn-network-policies>):

- **Edition.** No longer needs GKE Enterprise. Release notes, 2025-09-02:
  "Features that were part of GKE Enterprise are now available as part of
  the standard GKE offering [...] at no additional cost: [...] Fully
  Qualified Domain Name (FQDN) Network Policy". No charge.
- **Enabling.** `--enable-fqdn-network-policy`, then "For Standard clusters
  only, restart the GKE Dataplane V2 anetd DaemonSet"; a sibling page warns
  that this "will cause temporary downtime for your cluster".
- **How it works.** Egress only, by address and port: the dataplane watches
  the DNS answers a pod gets and allows those addresses. "Allowing access to
  a domain that is backed by a shared IP address will allow your Pod to
  communicate with all other domains served by that IP address."
- **Limits.** "The maximum number of IPv4 and IPv6 IP addresses that a
  FQDNNetworkPolicy can resolve to is 50." `*.company.com` "matches
  api.company.com [...] but not eu.api.company.com or company.com". Every
  alias a pod may query has to be listed. Pods must resolve through
  kube-dns or Cloud DNS.
- **TTL.** After a record expires "new connections are rejected"; long-lived
  connections continue.
- **UNVERIFIED:** its behaviour with NodeLocal DNSCache (which this cluster
  runs) and with gVisor pods is not documented.

**Cilium policies on GKE**
(<https://docs.cloud.google.com/kubernetes-engine/docs/how-to/configure-cilium-network-policy>):
only the cluster-wide kind is offered, behind a flag and an `anetd`
restart; "Layer 7 policies are not supported"; and "For new deployments, use
the Kubernetes-native, standardized ClusterNetworkPolicy instead." Namespaced
`CiliumNetworkPolicy` and `toFQDNs` are UNVERIFIED and not to be built on.

**ClusterNetworkPolicy**
(<https://docs.cloud.google.com/kubernetes-engine/docs/how-to/configure-cluster-network-policy>):
has Deny and an Admin tier above NetworkPolicy, which is exactly a ceiling.
"in preview and is at the alpha level", from 1.36.0-gke.4447000. Not for
version 1; a candidate for a second, independent ceiling later (section 7).

**Network policy logging**
(<https://docs.cloud.google.com/kubernetes-engine/docs/how-to/network-policy-logging>):
one `NetworkLogging` object per cluster; denied connections can be logged
per namespace (annotation `policy.network.gke.io/enable-deny-logging`). No
generation charge; Cloud Logging's usual charge; "up to 500 connections per
second" a node. Useful for "why did my session's request fail", and the
only source of that in version 1.

**An egress proxy.** A forward proxy that allows `CONNECT host:port` (and
plain HTTP) by hostname, per client, and logs every decision. `HTTPS_PROXY`
and a browser's proxy setting are advisory, so the session's NetworkPolicy
must allow egress to the proxy and nowhere else; then the setting is only
what makes things work, not what enforces. No TLS interception is needed:
the name is in the `CONNECT` line. Smokescreen
(<https://github.com/stripe/smokescreen>) is a "HTTP CONNECT proxy" with a
hostname ACL that also refuses names resolving to internal addresses, but
identifies clients by mTLS certificate. Envoy's HTTP dynamic forward proxy
can authorise on source address and authority with its RBAC filter and takes
configuration without a restart; its SNI-only variant is "alpha and not
production ready". Squid can do both and filter by `src`.

### 1.3 How strong each is against a hostile process in the pod

- **Enforcement point.** gVisor's network stack writes packets to the pod's
  virtual device (<https://gvisor.dev/docs/user_guide/networking/>); policy
  is applied by Dataplane V2 on the node side of it. A process cannot switch
  it off from inside. (That the hook is outside the sandbox is an inference
  from the gVisor and Cilium designs, not a quoted statement.)
- **Raw sockets.** GKE Sandbox: "By default, the container is prevented from
  opening raw sockets" (<https://docs.cloud.google.com/kubernetes-engine/docs/concepts/sandbox-pods>),
  and our containers drop every capability. Raw sockets would not get past
  the dataplane anyway.
- **Direct addresses, DNS over HTTPS.** Irrelevant to `none` and to an
  address allow-list: only listed addresses are reachable, by whatever
  means. They defeat nothing in version 1.
- **DNS tunnelling.** While a session may resolve names, it can send data
  out, slowly, inside queries for a domain whose name server the attacker
  runs. The cluster's resolver forwards them. `none` closes this (no DNS).
  `allowlist` with `"dns": false` closes it. `allowlist` with DNS does not,
  and `FQDNNetworkPolicy` cannot (it requires the pod to resolve). The
  proxy design can: the proxy resolves, the pod does not.
- **Shared addresses.** A hostname rule enforced by address
  (`FQDNNetworkPolicy`) also opens every other site on the same address,
  which for a CDN is a great many. The proxy matches the name.
- **Other sessions, the node, the control plane, metadata.** Inside the
  ceiling in every mode; section 4.4.

### 1.4 Constraints of this product

- **Warm pods run before they have an owner.** A `NetworkPolicy` selects by
  label and applies to running pods; nothing is restarted. The time it takes
  is not observable from the API (quoted above). On Dataplane V2 it is
  expected to be well under a few seconds: UNVERIFIED, spike 1.
- **A label that identifies one session's pod already exists.** The Sandbox
  controller labels every pod `agents.x-k8s.io/sandbox-name-hash` (FNV-1a of
  the Sandbox name), warm or cold, before adoption, and again on the pod a
  snapshot restores, because the Sandbox keeps its name. Checked: the
  template hash the cluster shows for `session` (`c35f4cf7`) is that
  function of `session`. Our snapshot grouping already relies on this label.
- **Sleep and restore.** A sleeping session has no pod; its policy stays and
  selects nothing. The restored pod has a new address and the same label, so
  it is selected from the start. Whether a restored pod can send a packet
  before its policy is in place is UNVERIFIED (spike 1); the shrunken
  baseline makes the failure direction safe, because a pod with no
  per-session policy has no egress.
- **What must keep working whatever a user writes.** Ingress from the
  backend on 6080, 8080, 8081; egress to the shared OPA on 8181; later
  mcp-exec on loopback, which no NetworkPolicy touches. These stay in the
  shared policy, which users cannot edit. DNS is deliberately not in this
  list: `none` means none.
- **Chromium under restricted egress** shows its ordinary error pages
  (`ERR_CONNECTION_TIMED_OUT`, or `ERR_NAME_NOT_RESOLVED` without DNS), and
  its background services fail quietly. That is the expected look of a
  restricted session; the UI says which mode a session is in.

---

## 2. Data model

### 2.1 A `network` section of the session's policy

Three places were considered:

| | For | Against |
|---|---|---|
| **A key inside the JSON policy source** | one document | a Rego policy has no JSON to put it in; network rules would be parsed out of a string |
| **`spec.network` on `SessionPolicy`** (recommended) | one resource, one generation, one management switch, one owner reference; typed, validated by the API server | the resource now has two reconcilers' worth of status |
| **A separate `SessionNetwork` resource** | fully independent of tool-call policies | a second management switch, a second version to send in `If-Match`, a second Terraform resource for what users think of as "the session's policy" |

`spec.network` it is. Fields:
[`crd-sessionpolicy-network.yaml`](../contracts/network/crd-sessionpolicy-network.yaml).

```yaml
apiVersion: browserjs.dev/v1alpha1
kind: SessionPolicy
metadata:
  name: s-abcde
spec:
  sessionRef: {name: s-abcde}
  kind: json
  source: '{"version": 1, "allow": {"operations": ["*"]}}'
  network:                      # absent means mode internet
    version: 1
    mode: allowlist
    dns: true
    allow:
      - {cidr: 192.0.2.0/24, ports: [{port: 443}], description: the staging site}
  management: {mode: editor}
status:
  network:
    observedGeneration: 7
    mode: allowlist
    policy: s-abcde-egress
    hostnames: unavailable
    errors: []
  conditions:
    - {type: NetworkApplied, status: "True", observedGeneration: 7}
```

### 2.2 The rules a user writes

Schema: [`network-policy.schema.json`](../contracts/network/network-policy.schema.json).
Examples: [`examples/`](../contracts/network/examples/).

| Preset in the UI | Rules | What the session can reach |
|---|---|---|
| **No network** | `{"version": 1, "mode": "none"}` | nothing: no DNS, no internet. The owner still sees and drives it; files still upload and download through the backend. |
| **Internet** (default) | `{"version": 1, "mode": "internet"}` | what every session reaches today |
| **Only these destinations** | `{"version": 1, "mode": "allowlist", "allow": [...]}` | the listed address ranges and ports, and DNS unless `"dns": false` |
| (no preset; edit Internet) | `{"version": 1, "mode": "internet", "deny": [{"cidr": ...}]}` | the internet minus those ranges |

Limits: 50 entries, 20 ports an entry, IPv4 only. A deny entry is a whole
address range: NetworkPolicy cannot refuse one port.

Version 2 adds `{"host": "github.com"}` and `{"host": "*.github.com"}` as
an alternative to `cidr` in `allow`. Version 1 refuses them with
`hosts_unavailable` and the UI shows the field disabled with the reason, so
the format does not change when hostnames arrive.

### 2.3 Not Rego

Tool-call policies can be Rego because each call is a question put to OPA.
Network rules are not evaluated per connection by anything we run: they are
data handed to the dataplane, which matches packets itself. There is no
point at which a Rego rule could be asked, and a Rego module cannot be
turned into a list of address ranges in general. So the network section is
JSON only, whatever `spec.kind` says about the tool-call part. (OPA could
check a rule set against the platform's ceiling, but that check is ten lines
in the operator and has no tenant-written logic in it.)

### 2.4 Beside the tool-call policy

- **Editor.** The policy page gets two sections: "What agents may do"
  (today's editor) and "Network". Network has the three preset tiles, a
  table editor for entries (address range, ports, note), and a JSON view in
  the same Monaco component with the schema for diagnostics. A line under
  the heading: "Applies to everything in the session, including the
  terminal. Tool rules apply only to agents connected over MCP."
- **Create page.** A Network choice next to the policy preset; default
  Internet.
- **Managed as code.** The one switch covers both: in `iac` mode neither
  section is editable in the UI.
- **API.** The policy document gains `network`
  ([`backend-api.yaml`](../contracts/network/backend-api.yaml)). A `PUT`
  without `network` leaves it alone, so today's clients change nothing.
- **Terraform.** `browserjs_session_policy` gains an optional `network`
  attribute (a string, compared as parsed JSON, like `json`), and
  `browserjs_policy_document` its blocks (`network_mode`, `network_allow`,
  `network_deny`). `browserjs_session` gains `network` for create time.

### 2.5 Sessions without a `SessionPolicy`

Tool-call policies are merged but off, and a session created before they
ship never gets a `SessionPolicy`. Network rules must not depend on that:

- The operator's network reconciler is keyed on the **Sandbox**. Every
  Sandbox labelled as a session pod's gets a `<session>-egress` policy. The
  rules come from `SessionPolicy.spec.network` if there is one, and are
  `internet` otherwise. Warm, unowned Sandboxes included.
- Version 1 therefore needs the policy operator running, and does not need
  OPA, the pod template change or the mcp-js configuration.
- To give an older session network rules, the backend creates its
  `SessionPolicy` with the unrestricted source. The old pod never asks OPA,
  so the tool-call part is inert. This changes one sentence of the tool-call
  design (4.8, "A `SessionPolicy` is never created for a session that
  predates the feature"): the API must report tool-call support from the
  Sandbox, not from the object's absence. A contract change for the policy
  tracks; open question 6.

---

## 3. What the operator reconciles

Exact rules: [`translation.md`](../contracts/network/translation.md).

- **One object per session:** `NetworkPolicy <session>-egress`, `Egress`
  only, owned by the Sandbox, so deleting the session deletes it with no
  code of ours. Selector: `app: browserjs-session` and the Sandbox's
  name-hash label.
- **`none`** is `egress: []`. The object still exists, so "every Sandbox has
  exactly one" is a check anyone can run.
- **`internet`** is the DNS rule plus `0.0.0.0/0` except the ceiling: the
  same rules `session-pods` carries today (checked mechanically against
  `main`).
- **Triggers:** a Sandbox appears (warm or cold), a `SessionPolicy`'s
  `spec.network` changes, the operator starts (it lists and repairs), and a
  timer, because someone can delete the object by hand.
- **Refused rules are not applied.** The previous rules stay in force and
  `NetworkApplied` is False with the errors, the same shape as a policy
  that does not compile.
- **Hash collisions** (32 bits) are detected before writing; both sessions
  fall to no egress with reason `HashCollision` rather than share
  allowances. At 1,000 live sessions the chance that any pair collides is
  about 1 in 8,600. Open question 5 offers a label of our own instead.
- **Scale.** One small object per session. Upstream's trouble was at
  100,000 policies; our ceiling is `session_max_nodes` times nine pods.
  Sessions in the default mode could later share one policy by label if this
  ever matters; not now.
- **RBAC.** The operator gains get, list, watch, create, update, patch,
  delete on `networkpolicies` in the sessions namespace, and read on
  `sandboxes`. It must not be able to touch `session-pods`: a
  `ValidatingAdmissionPolicy` (or `resourceNames` where verbs allow) limits
  it to objects labelled `app.kubernetes.io/managed-by: policy-operator`.

### 3.1 Status

`status.network` and the `NetworkApplied` condition, written by the operator
only. **"Applied" means the object is written, not that the dataplane has
loaded it:** Kubernetes offers no way to know the second. The UI words it as
"Network rules saved and sent to the cluster". Errors carry a JSON pointer
(`/allow/2/cidr`) so the editor can mark the entry.

### 3.2 Warm, adopted, sleeping, restored

| Moment | What happens |
|---|---|
| Warm Sandbox created by the pool | operator writes `<session>-egress` with `internet`. The pod is running the platform's image and nobody's instructions. |
| Adopted, default rules | nothing to do: already `internet`. The operator is not on the path of the common case. |
| Adopted, restrictive rules | backend creates the `SessionPolicy` with `network`; operator rewrites the policy; **the backend writes the owner onto the Sandbox only after `NetworkApplied` is True, plus a settle time** (spike 1 sets it; start at 2 s). Until the owner is written nobody can use the session, so no user-directed process runs before the restriction is in. The claim carries the intended rules in an annotation, as it carries the policy, so crash recovery cannot finish an adoption with the default. |
| Cold | backend creates the Sandbox and its `SessionPolicy`; the pod has no egress until the operator writes its policy (the shrunken baseline); the session stays `starting` until `NetworkApplied`. |
| Rules edited on a running session | applied to the running pod. Loosening works at once for new connections. After tightening, **connections already open may continue** (implementation defined; conntrack). The UI says "Open connections may last until they close. Stop and resume the session to cut them." |
| Sleeping | the policy stays, selecting nothing. Editing rules while asleep works. |
| Restored | new pod, new address, same label, same policy. |
| Deleted | garbage-collected with the Sandbox. |
| Operator down | existing sessions keep their policies. Warm and default adoptions work. A new restrictive or cold session stays `starting` and times out: it fails closed. |

---

## 4. Baseline, ceiling, and what users may not do

### 4.1 The shared policy after the change

[`baseline.networkpolicy.yaml`](../contracts/network/baseline.networkpolicy.yaml):
ingress from the backend on the three ports; egress to OPA only (empty until
tool-call policies deploy). No DNS, no internet.

### 4.2 The ceiling

Never reachable, in any mode: `10.0.0.0/8`, `172.16.0.0/12`,
`192.168.0.0/16`, `100.64.0.0/10`, `169.254.0.0/16`. That covers nodes
(`10.10.0.0/20`), pods and so other sessions (`10.20.0.0/16`), Services
(`10.30.0.0/20`), the metadata address, and any network later peered with
the VPC. How it holds:

- an `allow` entry inside a forbidden range is refused;
- an `allow` entry that contains one (`0.0.0.0/0`) is written with the
  forbidden ranges as `except`;
- the translation is a pure function with golden tests, and nothing else
  writes session policies.

The control plane's public endpoint is closed by authorized networks with no
entries (`infra/main/cluster.tf`), and session pods have no service account
token. If authorized networks are ever opened, its address goes in
`EXTRA_FORBIDDEN_CIDRS`.

### 4.3 Ingress

Not offered. A session is reached through the backend, which is where the
owner check is. Two sessions of the same owner cannot talk to each other
either.

### 4.4 Opening to private destinations

Not offered. The ceiling protects the platform and every other tenant; a
user-written exception to it is a request to trust the user's judgement
about our network. If a customer needs sessions that reach their private
services, that is a deployment-level feature (their own cluster or a peered
network with an operator-configured allowance), decided by the platform's
administrator, not a field in a session's policy.

---

## 5. Migration, without a gap

NetworkPolicy is a union, so the order is: add everywhere, verify, then
remove from the shared policy.

1. **Deploy the operator's network reconciler.** It writes
   `<session>-egress` (`internet`) for every existing Sandbox: running,
   sleeping, warm. Each is the same rules the shared policy already gives,
   so nothing changes for any pod.
2. **Verify** with `cluster info`: the count of `*-egress` policies equals
   the count of Sandboxes, and none is missing. Add this as a line the
   status script prints (section 8.3).
3. **Shrink `session-pods`** to the baseline, as its own pull request and
   deploy. The deploy workflow refuses it if step 2's check fails. Every
   pod is still allowed the same traffic by its own policy; open
   connections are not expected to drop, since each flow stays allowed
   throughout (UNVERIFIED on Dataplane V2: do it first with one test session
   streaming a download and a VNC view open).
4. **Roll back** by re-applying the old `session-pods`. It is additive, so
   it is safe at any time and undoes only the ability to restrict.
5. **Then** turn on the API and UI for network rules.

Between 1 and 3 a user's `none` would be stored and not enforced, which is
why the user-facing part comes last. The backend reports
`network_status.state: unsupported` until a deployment flag says the
baseline has shrunk.

The local kind cluster follows the same manifests; whether its CNI enforces
them is section 8.2.

---

## 6. Version 2: hostnames

Most people asking to "only allow certain sites" mean names. Two ways.

| | `FQDNNetworkPolicy` | Egress proxy (recommended) |
|---|---|---|
| What matches | addresses learned from the pod's DNS answers | the name in `CONNECT` or the HTTP request |
| Other sites on the same address | allowed too | not allowed |
| Wildcards | one label deep | as we define them |
| Limits | 50 addresses a policy | ours |
| Protocols | any TCP or UDP | HTTP, HTTPS, and anything that can use `CONNECT`; nothing else |
| DNS tunnelling | stays open (the pod must resolve) | closed: the pod gets no DNS; the proxy resolves |
| A user's name that resolves to a private address | **allowed by the FQDN policy, piercing the ceiling**, unless a deny tier sits above (ClusterNetworkPolicy, alpha on GKE) | refused: the proxy's own NetworkPolicy has the ceiling, and it re-checks the resolved address |
| Log of what was reached or refused, by name | no (addresses, through policy logging) | yes, per session |
| In the session | nothing to configure | Chromium and the shell must be pointed at the proxy |
| To build | one more object from the operator | a proxy deployment, its per-session configuration, and the in-session setting |
| Cluster change | a flag and an `anetd` restart (a short cluster-wide network interruption) | none |
| Cost to run | none | two small replicas on the system pool; no new node at today's size |
| Unknowns | NodeLocal DNSCache and gVisor interplay undocumented | how to set the proxy in a pod that was started warm |

**Recommendation: the proxy.** The deciding points are the pierced ceiling
(a user allows `x.their-domain.example`, points it at `10.20.0.7` or
`169.254.169.254`, and `FQDNNetworkPolicy` lets the packets through, because
allow rules are a union and it has no notion of our forbidden ranges), the
shared-address leak, and the log. `FQDNNetworkPolicy` remains the cheaper
fallback if the product owner prefers no new workload and accepts those
limits, and only together with a deny tier for the ceiling.

Sketch of the proxy version, to be designed in full when version 2 is
scheduled:

- **Mode.** `allowlist` entries with `host`. The session's
  `<session>-egress` allows egress to pods `app: egress-proxy` on its port
  and nothing else, plus any `cidr` entries. No DNS rule unless asked.
- **Identity.** The proxy decides by the client's source address. The
  operator watches session pods and writes the address-to-rules table,
  updating it when a restore changes the address. Cilium drops packets
  whose source is not the pod's own address (UNVERIFIED on GKE; spike 3),
  so one session cannot use another's rules. Unknown source: refused.
- **The proxy** is Envoy (HTTP dynamic forward proxy, RBAC by source and
  authority, configuration pushed without restarts) unless spike 3 prefers
  Squid. Its own NetworkPolicy: ingress from session pods, egress to DNS and
  to the internet minus the ceiling. It refuses a name that resolves inside
  the ceiling.
- **In the session.** Enforcement does not depend on it, but usability does.
  Warm pods are already running, so it cannot be a start-up flag. Candidate:
  the browser container's server (8081) accepts a "set proxy" call from the
  backend and writes Chromium's managed policy file (`ProxySettings`) and
  the shell's environment file; Chromium reloads managed policy without a
  restart (UNVERIFIED; spike 3).
- **Logs.** The proxy's access log, labelled by session, is the "recent
  network decisions" view the tool-call design deferred.

If hostname enforcement is not deployed (version 1, or a cluster without
the proxy), `status.network.hostnames` is `unavailable`, a rule set with a
`host` is refused with `hosts_unavailable`, and the UI disables the field
with "Hostnames are not available on this deployment; use address ranges."
It never silently ignores a rule.

---

## 7. Security review

- **What it is for.** Containing what a session can reach, against any
  process in it, including a shell the agent drives. It is the control that
  holds when tool-call policies are bypassed from inside the session.
- **What it is not.** Not content inspection; not a limit on what the
  backend relays (the VNC view, uploads, downloads, MCP responses: a
  `none` session can still hand data to its agent over MCP, which is the
  point of a session); not a hostname filter in version 1.
- **Fail closed.** The shrunken baseline means a pod with no per-session
  policy has no egress: an operator bug or outage costs availability, not
  containment. Refused rules never replace applied ones.
- **The tightening window.** Rules reach the dataplane some time after the
  object is written, and Kubernetes cannot say when. At creation the owner
  is written after a settle time, so the window is closed to users. For an
  edit on a running session it is short and stated in the UI, as is the
  survival of open connections.
- **Warm pods have the internet before adoption.** As today. They run only
  the platform's image. Connections Chromium opened while warm can survive
  a restrictive adoption; they are the browser's own background traffic.
  Giving warm pods no egress until adoption is the stricter option, at the
  price of a propagation delay on every default session start (open
  question 4).
- **DNS as a channel.** Stated in the schema and the UI for `allowlist`
  with DNS on. `none` and `"dns": false` close it.
- **Destinations that are public but ours.** The product's own front end is
  on the internet and reachable from a session, as today; it requires a
  login the session does not have. Private Google Access addresses are
  public Google APIs; the pod has no credentials (no token mounted,
  metadata forbidden).
- **Tenants.** A session's policy selects that session's pod only; the
  collision guard keeps a hash clash from merging two sessions' allowances.
  A user can write rules only for a session they own (the backend's check,
  as for the tool-call policy). The agent cannot change them: the MCP route
  reaches a session's `/mcp`, never the API.
- **The operator's new power.** It can write NetworkPolicies in the sessions
  namespace. Limited by admission to objects it labels, so a compromised
  operator cannot widen `session-pods` or the backend's policy. It could
  still write a wide `*-egress` policy: an independent ceiling above
  NetworkPolicy (ClusterNetworkPolicy, Admin tier, Deny to the forbidden
  ranges for session pods) would bound even that. It is alpha on GKE today;
  adopt when it is generally available.
- **Someone with `kubectl`** can write past all of it. That is a cluster
  administrator, and is accepted, as in the tool-call design.
- **IPv6.** The cluster is IPv4 only; IPv6 entries are refused. If the
  cluster becomes dual-stack, `fc00::/7` and `fe80::/10` join the ceiling
  first.

---

## 8. Plan

### 8.1 Stages and tracks

**Version 1: plain NetworkPolicy** (none, internet, address allow-list and
deny-list).

| Track | Work | Depends on |
|---|---|---|
| N0 | Spikes 1 and 2 below | - |
| N1 | Operator: the translation (pure function, golden tests), the Sandbox-keyed reconciler, status, collision guard, `/v1/validate` | contracts |
| N2 | Deploy: CRD fields, operator RBAC and its admission limit, the migration (section 5) in two deploys, status script lines, policy logging for denied connections | N1 |
| N3 | Backend: `network` on create and on the policy API, the adoption order of section 3.2, the claim annotation, `network_status` | contracts |
| N4 | UI: Network section, presets, table and JSON editors, the status and the warnings quoted above | N3's API |
| N5 | Terraform provider: `network` on the two resources and the data source | N3's API |
| N6 | Integration on GKE (section 8.3) | all |

N1, N3, N4 and N5 run in parallel against the contracts.

**Version 2: hostnames.** Spike 3, then its own design and tracks: the
proxy deployment, the operator's table, the in-session setting, the log
view.

### 8.2 Tests

- **Unit (operator).** Every `examples/*.network.json` becomes its
  `*.networkpolicy.yaml` byte for byte. Table tests for each refusal code.
  Property tests: no output ever has an `ipBlock` that reaches a forbidden
  range (expand `cidr` minus `except` and intersect); `none` is always
  `egress: []`; output is stable under re-ordering of `deny`. Collision:
  two names with the same hash give two empty policies.
- **Unit (backend).** The adoption order (owner written only after
  `NetworkApplied`), crash recovery from the claim annotation, `PUT` without
  `network` leaving it alone, the management-mode table applied to network.
- **Operator against a fake API** (as `tests/fake_kube.py` does today):
  create on Sandbox, update on rules change, repair after manual delete, no
  write to objects it does not label.
- **kind.** Needs a CNI that enforces NetworkPolicy. Recent kind's default
  network does (UNVERIFIED for the version `hack/local-up.sh` installs;
  otherwise install Cilium in the local cluster). There: `none` blocks,
  `allowlist` allows only the listed address (a pod in the cluster stands in
  for "a public address", with a test-only ceiling), editing rules on a
  running pod takes effect, deleting the Sandbox removes the policy. Session
  pods on kind do not run under gVisor and there is no warm pool, NodeLocal
  DNSCache or snapshot.
- **Only on GKE.** DNS through NodeLocal DNSCache under a per-session
  policy; warm adoption; sleep and restore; gVisor; policy logging;
  everything in version 2.

### 8.3 Verification on the cluster

Read-only, through `cluster info` plus a session's own browser. The status
script gains: the per-session policies with their mode annotation, a line
"N Sandboxes, N egress policies, missing: ...", and each `SessionPolicy`'s
`NetworkApplied`.

1. After migration step 1: counts match; a session still loads
   `https://example.com`.
2. After step 3: the same, and `session-pods` shows no internet rule.
3. A session created with `none`: the browser shows
   `ERR_NAME_NOT_RESOLVED` for `https://example.com` and a timeout for
   `https://1.1.1.1`; the VNC view, MCP and a file upload still work.
4. A session with `allowlist` of `1.1.1.1/32` port 443: `https://1.1.1.1`
   loads, `https://example.com` does not, `http://1.1.1.1` does not.
5. In every mode, `http://169.254.169.254/` and a node address time out.
6. Edit a running session from `internet` to `none`: new loads fail within
   the settle time. Record the time.
7. Warm adoption with `none`: the session is never usable with the
   internet (load a page as the first action).
8. Sleep, wake: step 3 or 4 holds on the restored pod.
9. Delete the session: its policy is gone from the next `cluster info`.

With a terminal in the session the same checks run with `curl` and
`getent hosts`, which is the case the feature exists for.

### 8.4 Spikes

1. **Propagation and restore** (GKE, before N3): time from writing a policy
   to enforcement on a running gVisor pod, both directions; whether a
   restored pod can send before its policy applies; whether shrinking the
   shared policy disturbs open connections.
2. **The label** (GKE): confirm `sandbox-name-hash` on warm, adopted, cold
   and restored pods, and whether a label of our own can be put on an
   adopted warm pod (`SandboxClaim.spec.additionalPodMetadata` is served
   here), which would retire the collision guard.
3. **Hostnames** (before version 2): the proxy with source-address
   identity; source-address spoofing from a gVisor pod; setting Chromium's
   proxy in a running pod; and, for comparison, `FQDNNetworkPolicy` with
   NodeLocal DNSCache and gVisor on a throwaway cluster, since enabling it
   restarts `anetd`.

---

## 9. Open questions for the product owner

Each has a recommended default; the design above assumes it.

1. **What did the cut-off sentence ask for?** (a) cut a session off from
   the internet; (b) restrict a session to certain destinations; (c) let a
   session reach private services. *Default: (a) and (b), version 1 by
   address, version 2 by hostname; (c) not offered.*
2. **Is an address-only allow-list worth shipping before hostnames?**
   "No network" is useful by itself; an address allow-list is awkward for
   web sites, which move. *Default: yes, ship version 1 as designed; it is
   also the foundation version 2 needs.*
3. **Hostnames: proxy or `FQDNNetworkPolicy`?** *Default: the proxy
   (section 6). The alternative is free and quicker, but weaker, needs an
   `anetd` restart, and needs a deny tier to keep the ceiling.*
4. **Should warm pods have the internet before they are adopted?**
   *Default: yes, as today, so the common session start pays nothing. The
   stricter choice is no egress until adoption.*
5. **Select the pod by the controller's 32-bit hash label, or by a label of
   our own?** *Default: the hash, with the collision guard; switch if spike
   2 shows our own label can be put on adopted pods.*
6. **May a session created before tool-call policies get a `SessionPolicy`
   so it can have network rules?** *Default: yes, with the tool-call part
   reported as unsupported from the Sandbox (a one-sentence contract change
   to the tool-call design).*
7. **One "managed as code" switch for both sections, or one each?**
   *Default: one.*
8. **Should changing rules on a running session cut open connections?**
   Doing so needs a stop and resume. *Default: no; say so in the UI and
   offer the stop and resume.*
9. **Log denied connections (GKE network policy logging) for the sessions
   namespace?** It is the only "why did this fail" in version 1; Cloud
   Logging charges apply. *Default: yes, denied only.*
10. **May users ever exceed the ceiling or change ingress?** *Default: no.*
