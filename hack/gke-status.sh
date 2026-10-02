#!/usr/bin/env bash
# What the production cluster looks like, as Markdown on stdout. Read-only:
# only get, describe and logs. Used by the deploy and cluster-info workflows
# (which write it to the job summary), and usable by hand with a kubeconfig.
#
#   hack/gke-status.sh            the short status the deploy ends with
#   hack/gke-status.sh --full     plus nodes, events, logs, failing pods and
#                                 the admission policies in full
#
# With --full, POD=<name> (and POD_NAMESPACE, default browserjs-sessions) adds
# one pod's description and logs; SANDBOX=<session id> adds one Sandbox, its
# pod, its disk and its Pod Snapshots.
set -uo pipefail

NS=browserjs-sessions
namespaces=("$NS" cert-manager)
full=""
[ "${1:-}" != --full ] || full=1

section() { printf '\n### %s\n\n' "$1"; }
# Run a command and show its output as a code block, whatever it returns.
show() {
  printf '```\n$ %s\n' "$*"
  "$@" 2>&1 | head -c "${MAX_BYTES:-60000}"
  printf '\n```\n'
}

section "Versions"
show kubectl version

section "Agent Sandbox API"
echo "The backend needs \`sandboxes.agents.x-k8s.io\` to serve \`v1beta1\`."
printf '```\n'
kubectl get crd -o json 2>&1 | jq -r '
  .items[] | select(.metadata.name | test("sandbox|agents\\.x-k8s\\.io|podsnapshot"; "i")) |
  "\(.metadata.name)  scope=\(.spec.scope)  served=\([.spec.versions[] | select(.served) | .name] | join(","))  stored=\(.status.storedVersions | join(","))"' 2>&1
printf '```\n'

section "Agent Sandbox admission policies"
show kubectl get validatingadmissionpolicies,validatingadmissionpolicybindings -o wide

section "Pods"
for ns in "${namespaces[@]}"; do
  show kubectl -n "$ns" get pods -o wide
done

section "Edge"
show kubectl -n "$NS" get service pomerium -o wide
show kubectl -n "$NS" get certificate,certificaterequest,order,challenge -o wide
show kubectl get clusterissuers -o wide

section "Sessions"
show kubectl -n "$NS" get sandboxes.agents.x-k8s.io -o wide
show kubectl -n "$NS" get pvc

section "Pod Snapshots"
cat <<'TEXT'
An idle session sleeps to a snapshot and wakes from it (deploy/gke/snapshots.yaml).
Healthy: the storage config and the policy are Ready; a sleeping session
(MODE Suspended, STOPPED-BY idle) has a SNAPSHOT, a POOL and the same pool as
its PIN, and that PodSnapshot is listed below as Ready; a session stopped by
its user, or never put to sleep, has none. There is at most one PodSnapshot
a session, and no trigger older than a few minutes.
TEXT
# One of the two may be cluster-scoped; -A lists both either way.
show kubectl get podsnapshotstorageconfigs.podsnapshot.gke.io,podsnapshotpolicies.podsnapshot.gke.io -A -o wide
show kubectl -n "$NS" get sandboxes.agents.x-k8s.io -o 'custom-columns=NAME:.metadata.name,MODE:.spec.operatingMode,STOPPED-BY:.metadata.annotations.browserjs\.dev/stopped-by,SNAPSHOT:.metadata.annotations.browserjs\.dev/snapshot,POOL:.metadata.annotations.browserjs\.dev/snapshot-pool,PIN:.spec.podTemplate.spec.nodeSelector.browserjs\.com/pool,NODE:.status.nodeName'
show kubectl -n "$NS" get podsnapshots.podsnapshot.gke.io -o 'custom-columns=NAME:.metadata.name,POD:.metadata.annotations.podsnapshot\.gke\.io/origin-pod,READY:.status.conditions[?(@.type=="Ready")].status,REASON:.status.conditions[?(@.type=="Ready")].reason,POLICY:.spec.policyName,LAST-RESTORE:.status.lastAccessTime,CREATED:.metadata.creationTimestamp'
show kubectl -n "$NS" get podsnapshotmanualtriggers.podsnapshot.gke.io -o wide
# Which running session pods came up from a snapshot, and from which.
echo "Session pods with a PodRestored condition (a pod not listed cold started):"
printf '```\n'
kubectl -n "$NS" get pods -l app=browserjs-session -o json 2>&1 | jq -r '
  .items[] | . as $pod | .status.conditions[]? | select(.type == "PodRestored") |
  "\($pod.metadata.name)  node=\($pod.spec.nodeName)  PodRestored=\(.status)  reason=\(.reason // "")  \(.message // "")"' 2>&1
printf '```\n'

[ -n "$full" ] || exit 0

section "Nodes"
show kubectl get nodes -o wide -L sandbox.gke.io/runtime,cloud.google.com/gke-nodepool,browserjs.com/pool,cloud.google.com/machine-family,topology.kubernetes.io/zone
show kubectl get runtimeclasses,storageclasses
# What each node has, what is reserved on it (requests) and what is in use.
show kubectl describe nodes
show kubectl top nodes
show kubectl top pods -A --containers --sort-by=memory
show kubectl -n kube-system get pods -l k8s-app=kube-dns -o wide

section "Admission policies in full"
for kind in validatingadmissionpolicy validatingadmissionpolicybinding; do
  for name in $(kubectl get "$kind" -o name 2>/dev/null | grep -i -E 'sandbox|snapshot'); do
    show kubectl get "$name" -o yaml
  done
done

section "Pod Snapshots in full"
# Conditions say why a storage config, policy or snapshot is not ready.
show kubectl get podsnapshotstorageconfigs.podsnapshot.gke.io,podsnapshotpolicies.podsnapshot.gke.io -A -o yaml
show kubectl -n "$NS" get podsnapshots.podsnapshot.gke.io,podsnapshotmanualtriggers.podsnapshot.gke.io -o yaml
# "Successfully checkpointed the pod to PodSnapshot", and failures.
show kubectl -n "$NS" get events --field-selector reason=GKEPodSnapshotting --sort-by=.lastTimestamp
# GKE's own side: the per-node agent that writes and reads snapshots.
show kubectl -n gke-managed-pod-snapshots get pods -o wide
show kubectl -n gke-managed-pod-snapshots logs -l k8s-app=pod-snapshot-agent --all-containers --prefix --tail=50

section "Events"
for ns in "${namespaces[@]}"; do
  show kubectl -n "$ns" get events --sort-by=.lastTimestamp
done

section "Pods that are not ready"
for ns in "${namespaces[@]}"; do
  kubectl -n "$ns" get pods -o json 2>/dev/null | jq -r '
    .items[] | select(.status.phase != "Succeeded") |
    select(([.status.conditions[]? | select(.type == "Ready" and .status == "True")] | length) == 0) |
    .metadata.name' | while read -r pod; do
    show kubectl -n "$ns" describe pod "$pod"
  done
done

section "Logs (last 100 lines)"
show kubectl -n "$NS" logs statefulset/pomerium -c pomerium --tail=100
show kubectl -n "$NS" logs deployment/dex --tail=100
show kubectl -n "$NS" logs deployment/backend --tail=100
show kubectl -n cert-manager logs deployment/cert-manager --tail=100

section "Workloads"
show kubectl -n "$NS" get deployments,statefulsets,networkpolicies,serviceaccounts -o wide
show kubectl -n "$NS" describe service pomerium
show kubectl -n "$NS" describe certificate pomerium-tls

if [ -n "${POD:-}" ]; then
  ns="${POD_NAMESPACE:-$NS}"
  section "Pod $ns/$POD"
  show kubectl -n "$ns" describe pod "$POD"
  show kubectl -n "$ns" logs "$POD" --all-containers --prefix --tail="${TAIL:-200}"
  show kubectl -n "$ns" logs "$POD" --all-containers --prefix --previous --tail="${TAIL:-200}"
fi

if [ -n "${SANDBOX:-}" ]; then
  section "Sandbox $SANDBOX"
  show kubectl -n "$NS" get sandboxes.agents.x-k8s.io "$SANDBOX" -o yaml
  show kubectl -n "$NS" describe pod "$SANDBOX"
  show kubectl -n "$NS" logs "$SANDBOX" --all-containers --prefix --tail="${TAIL:-200}"
  show kubectl -n "$NS" get pvc -o wide
  # Its snapshots (named by the pod they were taken of), whether its pod was
  # restored from one (the PodRestored condition), and what happened to it.
  printf '```\n'
  kubectl -n "$NS" get podsnapshots.podsnapshot.gke.io -o json 2>&1 | jq --arg id "$SANDBOX" '
    .items[] | select(.metadata.annotations["podsnapshot.gke.io/origin-pod"] == $id) |
    {name: .metadata.name, labels: .metadata.labels, annotations: .metadata.annotations, spec, status}' 2>&1
  printf '```\n'
  show kubectl -n "$NS" get pod "$SANDBOX" -o 'jsonpath={range .status.conditions[*]}{.type}={.status} {.reason} {.message}{"\n"}{end}'
  show kubectl -n "$NS" get events --field-selector "involvedObject.name=$SANDBOX" --sort-by=.lastTimestamp
fi
exit 0
