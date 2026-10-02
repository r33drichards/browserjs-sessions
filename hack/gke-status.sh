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
# pod and its disk.
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
  "\(.metadata.name)  served=\([.spec.versions[] | select(.served) | .name] | join(","))  stored=\(.status.storedVersions | join(","))"' 2>&1
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

[ -n "$full" ] || exit 0

section "Nodes"
show kubectl get nodes -o wide -L sandbox.gke.io/runtime,cloud.google.com/gke-nodepool,topology.kubernetes.io/zone
show kubectl get runtimeclasses,storageclasses
# What each node has, what is reserved on it (requests) and what is in use.
show kubectl describe nodes
show kubectl top nodes
show kubectl top pods -A --containers --sort-by=memory
# What each node has, what is reserved on it (requests) and what is in use.
show kubectl describe nodes
show kubectl top nodes
show kubectl top pods -A --containers --sort-by=memory
show kubectl -n kube-system get pods -l k8s-app=kube-dns -o wide

section "Admission policies in full"
for kind in validatingadmissionpolicy validatingadmissionpolicybinding; do
  for name in $(kubectl get "$kind" -o name 2>/dev/null | grep -i sandbox); do
    show kubectl get "$name" -o yaml
  done
done

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
fi
exit 0
