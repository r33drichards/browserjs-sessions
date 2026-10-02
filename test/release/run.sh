#!/usr/bin/env bash
# How the backend and the site are released, on a kind cluster: Argo
# Rollouts as deploy/gke installs it, and deploy/gke/rollouts.yaml as it is.
#
#   test/release/run.sh            against the current kubectl context
#   RESULTS=out.md test/release/run.sh
#
# Real: the controller (deploy/gke/argo-rollouts), the two Rollouts, their
# Services, AnalysisTemplates and NetworkPolicy (deploy/gke/rollouts.yaml),
# the backend's and the site's NetworkPolicies, and hack/release.sh
# rollout-status. Stand-ins (stub.py): the backend, the site, and the canary
# script the backend's check runs, which concludes what the stand-in backend
# tells it to. So this shows what a release does with a version that passes
# and with one that fails; what the real canary finds is test/canary.py's
# own test (test/canary-kind.sh).
#
# Needs a cluster whose CNI enforces NetworkPolicy (kind 0.24 or later),
# kubectl and jq. Never point it at a cluster that matters.
# .github/workflows/release-kind.yml runs it.
set -uo pipefail
cd "$(dirname "$0")/../.." || exit 1

NS=browserjs-sessions
export NS
RESULTS="${RESULTS:-/dev/null}"
work="$(mktemp -d)"
forward=""
cleanup() {
  [ -z "$forward" ] || kill "$forward" 2>/dev/null
  rm -rf "$work"
}
trap cleanup EXIT

passed=0
failed=0
pass() {
  passed=$((passed + 1))
  echo "PASS  $1"
  printf '| pass | %s |\n' "$1" >>"$RESULTS"
}
fail() {
  failed=$((failed + 1))
  echo "FAIL  $1"
  [ -z "${2:-}" ] || printf '      %s\n' "$2"
  printf '| **FAIL** | %s: %s |\n' "$1" "${2:-}" >>"$RESULTS"
}
is() { if [ "$2" = "$3" ]; then pass "$1"; else fail "$1" "expected: $2   got: $3"; fi; }
k() { kubectl -n "$NS" "$@"; }
step() { printf '\n--- %s\n' "$1"; }

# What answers behind a Service, asked through a port-forward (which is not
# subject to NetworkPolicy): the pod the Service selects.
ask() { # service, path
  local port=$((20000 + RANDOM % 20000)) out=""
  k port-forward "service/$1" "$port:80" >/dev/null 2>&1 &
  forward=$!
  for _ in $(seq 1 30); do
    out="$(curl -s --max-time 3 "http://127.0.0.1:$port$2" 2>/dev/null)" && [ -n "$out" ] && break
    sleep 0.5
  done
  kill "$forward" 2>/dev/null
  wait "$forward" 2>/dev/null
  forward=""
  echo "$out"
}
# The Deployment's template is what a release changes.
release() { # deployment, NAME=value...
  local name="$1"
  shift
  k set env "deployment/$name" "$@" >/dev/null
}
pods() { # app: "<pods running> <distinct versions>"
  k get pods -l "app=$1" -o json | jq -r '
    [.items[] | select(.metadata.deletionTimestamp == null) | select(.status.phase == "Running")] |
    "\(length) \([.[] | .spec.containers[0].env[] | select(.name == "VERSION") | .value] | group_by(.) | map("\(length)xv\(.[0])") | join(" "))"'
}
status() { hack/release.sh rollout-status "$1" "${2:-300}" >"$work/status.log" 2>&1; }
role_is() { [ "$(ask "$1" /role)" = "$2" ]; }           # service, role
pods_are() { [ "$(pods "$1")" = "$2" ]; }                # app, what pods prints
preview_running() {
  [ "$(k get pods -l app=backend,browserjs.dev/role=preview --field-selector=status.phase=Running -o name | wc -l | tr -d ' ')" = 1 ]
}
one_new_site_pod() {
  k get pods -l app=site -o json | jq -e '[.items[] | select(.status.phase == "Running") | .spec.containers[0].env[] | select(.name == "VERSION" and .value == "2")] | length == 1'
}
until_true() { # seconds, command...
  local deadline=$((SECONDS + $1))
  shift
  while ! "$@" >/dev/null 2>&1; do
    [ "$SECONDS" -lt "$deadline" ] || return 1
    sleep 2
  done
}

echo "cluster: $(kubectl config current-context)"
{
  echo "Run $(date -u +%Y-%m-%dT%H:%MZ), $(kubectl version -o json 2>/dev/null | jq -r '"Kubernetes \(.serverVersion.gitVersion)"'), commit ${GITHUB_SHA:-$(git rev-parse --short HEAD)}."
  echo
  echo "| | |"
  echo "|---|---|"
} >>"$RESULTS"

# --- install -------------------------------------------------------------------
step "install"
kubectl apply --server-side --force-conflicts -k deploy/gke/argo-rollouts >/dev/null || { echo "the controller's manifests were refused"; exit 1; }
kubectl -n argo-rollouts rollout status deployment/argo-rollouts --timeout=300s || exit 1
kubectl wait --for=condition=Established --timeout=60s crd/rollouts.argoproj.io crd/analysistemplates.argoproj.io >/dev/null
is "the controller asks for what its kustomization says" "25m 96Mi" \
  "$(kubectl -n argo-rollouts get deployment argo-rollouts -o jsonpath='{.spec.template.spec.containers[0].resources.requests.cpu} {.spec.template.spec.containers[0].resources.requests.memory}')"

kubectl apply -f deploy/base/namespace.yaml >/dev/null
k create configmap release-test-stub --from-file=test/release/stub.py --dry-run=client -o yaml | k apply -f - >/dev/null
# What the deploy workflow makes of test/canary.py: here, the stand-in.
k create configmap release-canary --from-file=canary.py=test/release/stub.py --dry-run=client -o yaml | k apply -f - >/dev/null
k create secret generic release-canary --from-literal=token=made-up --dry-run=client -o yaml | k apply -f - >/dev/null
# The NetworkPolicies of the backend (deploy/base) and of the site.
k apply -f deploy/base/networkpolicy.yaml >/dev/null
awk 'BEGIN { RS = "\n---\n"; ORS = "\n---\n" } /\nkind: NetworkPolicy\n/' deploy/gke/site.yaml | k apply -f - >/dev/null
k apply -f test/release/workloads.yaml >/dev/null
if ! k apply -f deploy/gke/rollouts.yaml >"$work/apply.log" 2>&1; then
  cat "$work/apply.log"
  echo "deploy/gke/rollouts.yaml was refused"
  exit 1
fi
pass "deploy/gke/rollouts.yaml is accepted"

# --- the first version ------------------------------------------------------------
step "the first version"
if status backend && status site; then pass "both Rollouts come up from their Deployments' templates"; else
  fail "both Rollouts come up" "$(tail -5 "$work/status.log")"
  k get rollouts.argoproj.io,pods -o wide
  k describe rollouts.argoproj.io | tail -40
  exit 1
fi
is "the Deployments themselves have no pods" "0 0" "$(k get deployment backend site -o jsonpath='{.items[*].spec.replicas}')"
is "one backend, four site pods" "1 1xv1 / 4 4xv1" "$(pods backend) / $(pods site)"
is "the backend behind the Service is version 1" "1" "$(ask backend /version)"
until_true 90 role_is backend active
is "it is told it is the active one, in the file it reads" "active" "$(ask backend /role)"

# --- a backend that fails its check -----------------------------------------------
step "backend: a version that fails the canary"
release backend VERSION=2 VERDICT=fail
# While it is checked: two pods, and the Service still the old one.
until_true 120 preview_running
is "the new backend starts beside the old one, marked preview" "2 1xv1 1xv2" "$(pods backend)"
is "on standby: its own file says preview" "2 preview" "$(ask backend-preview /version) $(ask backend-preview /role)"
is "users are still on the old one" "1" "$(ask backend /version)"
if status backend 300; then fail "the release of a backend that fails its check is reported as failed"; else
  if grep -q "was given up on" "$work/status.log"; then pass "the release of a backend that fails its check is reported as failed, with why"; else
    fail "the release is reported as failed, with why" "$(tail -5 "$work/status.log")"
  fi
fi
if grep -q "says: fail" "$work/status.log"; then pass "the check's own output is in the report"; else
  fail "the check's own output is in the report" "$(tail -5 "$work/status.log")"
fi
is "the Rollout is Degraded" "Degraded" "$(k get rollouts.argoproj.io backend -o jsonpath='{.status.phase}')"
is "users never left the old one" "1 active" "$(ask backend /version) $(ask backend /role)"
until_true 120 pods_are backend "1 1xv1"
is "the failed one is taken away" "1 1xv1" "$(pods backend)"

# --- a backend that passes -----------------------------------------------------------
step "backend: a version that passes"
release backend VERSION=3 VERDICT=pass
if status backend 300; then pass "the release of a backend that passes is promoted"; else fail "the release of a backend that passes is promoted" "$(tail -5 "$work/status.log")"; fi
is "users are on the new one" "3" "$(ask backend /version)"
until_true 90 role_is backend active
is "which is told it is now the active one, without a restart" "active 0" \
  "$(ask backend /role) $(k get pods -l app=backend,browserjs.dev/role=active -o jsonpath='{.items[0].status.containerStatuses[0].restartCount}')"
until_true 120 pods_are backend "1 1xv3"
is "the old one goes after the switch" "1 1xv3" "$(pods backend)"
is "the check ran in a pod of its own, which reached the standby backend through the NetworkPolicies" "the backend at http://backend-preview.browserjs-sessions.svc says: pass" \
  "$(k logs "$(k get jobs -l app=release-canary --sort-by=.metadata.creationTimestamp -o name | tail -1)" 2>/dev/null | tail -1)"

# --- the canary not switched on ---------------------------------------------------------
step "backend: no token"
k delete secret release-canary >/dev/null
release backend VERSION=4 VERDICT=fail
if status backend 300; then pass "without the token the release goes through unchecked"; else fail "without the token the release goes through" "$(tail -5 "$work/status.log")"; fi
is "and its check says that it checked nothing" "1" \
  "$(k logs "$(k get jobs -l app=release-canary --sort-by=.metadata.creationTimestamp -o name | tail -1)" 2>/dev/null | grep -c 'not switched on')"
is "users are on it" "4" "$(ask backend /version)"

# --- the site: a quarter first ------------------------------------------------------------
step "site: a version that does not answer"
release site VERSION=2 BROKEN=1
# One new pod beside the old ones while it is looked at.
until_true 120 one_new_site_pod
canary_share="$(pods site)"
if status site 300; then fail "the release of a site that does not answer is reported as failed"; else pass "the release of a site that does not answer is reported as failed"; fi
case "$canary_share" in
  "4 3xv1 1xv2" | "5 4xv1 1xv2") pass "while it was checked, one pod in four (or five) was the new one: $canary_share" ;;
  *) fail "while it was checked, one pod in four was the new one" "$canary_share" ;;
esac
until_true 120 pods_are site "4 4xv1"
is "the old site's four pods serve again" "4 4xv1" "$(pods site)"

step "site: a version that answers"
release site VERSION=3 BROKEN-
if status site 300; then pass "the release of a site that answers is promoted"; else fail "the release of a site that answers is promoted" "$(tail -5 "$work/status.log")"; fi
until_true 120 pods_are site "4 4xv3"
is "all four pods are the new site" "4 4xv3" "$(pods site)"
is "and it is what the Service answers with" "<html><title>site 3</title></html>" "$(ask site /)"

# --- nothing else reaches the standby backend ---------------------------------------------
step "NetworkPolicy"
out="$(k exec "$(k get pods -l app=site -o name | head -1)" -- python3 -c '
import socket
s = socket.socket(); s.settimeout(3)
try:
    s.connect((socket.gethostbyname("backend-preview.browserjs-sessions.svc"), 80)); print("open")
except socket.timeout: print("blocked")
except OSError as e: print("error:%s" % e)
' 2>&1)"
# The site may reach nothing at all, DNS included.
case "$out" in
  blocked | error:*) pass "a pod that is not the release's check does not reach backend-preview ($out)" ;;
  *) fail "a pod that is not the release's check does not reach backend-preview" "$out" ;;
esac

printf '\n%d passed, %d failed\n' "$passed" "$failed"
printf '\n%d passed, %d failed.\n' "$passed" "$failed" >>"$RESULTS"
[ "$failed" -eq 0 ]
