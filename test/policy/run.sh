#!/usr/bin/env bash
# Session policies on a kind cluster: the checks of track B of the plan
# (docs/plans/2026-10-02-session-policies-tracks.md) that need a cluster.
#
#   test/policy/run.sh            against the current kubectl context
#   RESULTS=out.md test/policy/run.sh   also writes what was measured
#
# Real: the OPA image and its configuration, deploy/base's Services, Roles,
# Secret wiring and NetworkPolicies, the CRDs, and the mcp-js image with the
# variable hack/policy-stage.sh writes. Stand-ins (stub.py): the policy
# operator (a bundle server publishing what this script builds from the
# contract's examples), the browser container (an MCP server that runs
# nothing) and the backend (a listener). With OPERATOR_IMAGE set, the last
# part swaps the stand-in operator for the real one. Session pods are plain Pods with
# the session label, not Sandboxes: no Agent Sandbox controller is needed.
#
# Needs: a cluster whose CNI enforces NetworkPolicy (kind 0.24 or later),
# with browserjs/mcp-js:dev loaded; kubectl, jq, python3, and an `opa` of
# the deployed version on PATH (or OPA=/path/to/opa). It creates and
# deletes things in the namespace browserjs-sessions: never point it at a
# cluster that matters. .github/workflows/policy-kind.yml runs it.
set -uo pipefail
cd "$(dirname "$0")/../.." || exit 1

NS=browserjs-sessions
OPA="${OPA:-opa}"
RESULTS="${RESULTS:-/dev/null}"
contracts=docs/contracts/policy
work="$(mktemp -d)"
python_image="$(awk '$1 == "digest:" { print "python@" $2; exit }' test/policy/kustomization.yaml)"
# Two sessions: one with a policy in the bundle, one with none (as a warm
# pod nobody has adopted, or a session whose SessionPolicy is missing).
WITH=s-pol01
WITHOUT=s-pol02

passed=0
failed=0
pass() {
  passed=$((passed + 1))
  echo "PASS  $1"
}
fail() {
  failed=$((failed + 1))
  echo "FAIL  $1"
  [ -z "${2:-}" ] || printf '      %s\n' "$2"
}
# ok <description> <passes unless this is empty> [what to say when it fails]
ok() { if [ -n "$2" ]; then pass "$1"; else fail "$1" "${3:-}"; fi; }
is() { if [ "$2" = "$3" ]; then pass "$1"; else fail "$1" "expected: $2   got: $3"; fi; }
note() { printf '%s\n' "$*" >>"$RESULTS"; }
k() { kubectl -n "$NS" "$@"; }
step() { printf '\n--- %s\n' "$1"; }

forwards=()
cleanup() {
  for pid in ${forwards[@]+"${forwards[@]}"}; do kill "$pid" 2>/dev/null; done
  rm -rf "$work"
}
trap cleanup EXIT

sha256() { if command -v sha256sum >/dev/null; then sha256sum | cut -d' ' -f1; else shasum -a 256 | cut -d' ' -f1; fi; }

# A bundle as docs/contracts/policy/rego-contract.md lays it out, from the
# contract's example policies. Arguments: output file, revision, then
# <session id>=<example name> for each session in it.
build_bundle() {
  local out="$1" revision="$2" dir pair id example loaded='{}'
  shift 2
  dir="$(mktemp -d "$work/bundle.XXXXXX")"
  mkdir -p "$dir/browserjs/loaded" "$dir/tenant" "$dir/decision"
  printf '{"roots": ["browserjs"]}\n' >"$dir/.manifest"
  for pair in "$@"; do
    id="${pair%%=*}" example="$contracts/examples/${pair#*=}.rego"
    sed "s/^package browserjs\\.policy\$/package browserjs.tenant[\"$id\"]/" "$example" >"$dir/tenant/$id.rego"
    sed "s/{{SESSION_ID}}/$id/g" "$contracts/decision-module.rego.tmpl" >"$dir/decision/$id.rego"
    loaded="$(jq -c --arg id "$id" --arg hash "sha256:$(sha256 <"$example")" '.[$id] = $hash' <<<"$loaded")"
  done
  printf '%s\n' "$loaded" >"$dir/browserjs/loaded/data.json"
  "$OPA" build -b "$dir" --capabilities "$contracts/capabilities.json" -r "$revision" -o "$out"
}
publish_bundle() { # file: what the stub operator serves from now on
  k exec -i deploy/policy-operator -- sh -c 'cat > /tmp/bundle.tmp && mv /tmp/bundle.tmp /tmp/bundle.tar.gz' <"$1"
}

# Can <host>:<port> be connected to? One line an address: open, blocked (no
# answer in 3 s), refused, or the error.
PROBE='
import socket, sys
for target in sys.argv[1:]:
    host, port = target.rsplit(":", 1)
    s = socket.socket(); s.settimeout(3)
    try:
        s.connect((socket.gethostbyname(host), int(port))); print(target, "open")
    except socket.timeout: print(target, "blocked")
    except ConnectionRefusedError: print(target, "refused")
    except OSError as e: print(target, "error:%s" % e)
    finally: s.close()
print("done")
'
# expect <from> <output> <target> open|closed
expect() {
  local got
  got="$(awk -v t="$3" '$1 == t { print $2 }' <<<"$2")"
  note "| $1 | \`$3\` | ${got:-no answer} |"
  case "$4:$got" in
    open:open) pass "$1 reaches $3" ;;
    closed:blocked | closed:refused) pass "$1 does not reach $3 ($got)" ;;
    *) fail "$1 and $3: want $4" "got: ${got:-nothing}" ;;
  esac
}

# One browser call (or more) through a session's mcp-js; JSON lines.
call() { python3 test/policy/mcp_call.py "http://127.0.0.1:$1" "$2" "${3:-1}"; }
outcome() { jq -rs 'map(.outcome) | unique | join(",")' <<<"$1"; }
seconds() { jq -rs 'map(.seconds) | sort | "\(.[0]) to \(.[-1]) s (\(length) calls, median \(.[length / 2 | floor]))"' <<<"$1"; }
# What the agent code saw, and how long its own callTool took.
seen() { jq -rs '.[0].seen' <<<"$1" | head -c 500; }

echo "cluster: $(kubectl config current-context)"
"$OPA" version | head -1
note "Run $(date -u +%Y-%m-%dT%H:%MZ), $(kubectl version -o json 2>/dev/null | jq -r '"Kubernetes \(.serverVersion.gitVersion)"'), OPA $("$OPA" version | awk 'NR == 1 { print $2 }'), commit ${GITHUB_SHA:-$(git rev-parse --short HEAD)}."

# --- deploy ------------------------------------------------------------------
step "deploy"
kubectl apply -f deploy/base/namespace.yaml >/dev/null
k create secret generic policy-tokens --dry-run=client -o yaml \
  --from-literal=bundle-token="$(head -c 32 /dev/urandom | base64 | tr '+/' '-_' | tr -d '=\n')" \
  --from-literal=opa-token="$(head -c 32 /dev/urandom | base64 | tr '+/' '-_' | tr -d '=\n')" \
  --from-literal=operator-api-token="$(head -c 32 /dev/urandom | base64 | tr '+/' '-_' | tr -d '=\n')" |
  kubectl apply -f - >/dev/null
kubectl apply -k test/policy >/dev/null || { echo "apply failed"; exit 1; }
k rollout status statefulset/webhook-redis --timeout=180s
kubectl wait --for=condition=Established crd/sessionpolicies.browserjs.dev crd/apitokens.browserjs.dev --timeout=60s >/dev/null
# The stub operator is not ready until it has a bundle to serve: running is enough.
k wait --for=jsonpath='{.status.phase}'=Running pod -l app=policy-operator --timeout=180s >/dev/null ||
  k rollout status deploy/policy-operator --timeout=120s
k rollout status deploy/backend --timeout=180s

session_pod() { # id
  cat <<POD
apiVersion: v1
kind: Pod
metadata:
  name: $1
  labels:
    app: browserjs-session
spec:
  automountServiceAccountToken: false
  enableServiceLinks: false
  terminationGracePeriodSeconds: 5
  securityContext:
    fsGroup: 1000
    runAsNonRoot: true
    runAsUser: 1000
    runAsGroup: 1000
  containers:
    - name: browser
      image: $python_image
      command: ["python3", "/stub/stub.py", "browser"]
      volumeMounts:
        - name: stub
          mountPath: /stub
    - name: mcp-js
      image: browserjs/mcp-js:dev
      imagePullPolicy: Never
      env:
        - name: SESSION_ID
          valueFrom:
            fieldRef:
              fieldPath: metadata.name
        - name: MCP_V8_PUBLIC_URL
          value: "https://\$(SESSION_ID).sessions.example.com"
$(hack/policy-stage.sh --env warm | sed 's/^/        /')
      ports:
        - name: mcp
          containerPort: 8080
      readinessProbe:
        httpGet:
          path: /api/artifacts
          port: mcp
        periodSeconds: 1
      volumeMounts:
        - name: data
          mountPath: /data
  volumes:
    - name: stub
      configMap:
        name: $(k get configmap -o name | sed -n 's|configmap/\(policy-test-stub-.*\)|\1|p' | head -1)
    - name: data
      emptyDir: {}
POD
}
for id in "$WITH" "$WITHOUT"; do session_pod "$id" | k apply -f - >/dev/null; done
if ! k wait --for=condition=Ready "pod/$WITH" "pod/$WITHOUT" --timeout=240s; then
  k describe pod "$WITH" | tail -30
  k logs "$WITH" --all-containers --tail=50
  echo "the session pods did not start"
  exit 1
fi
port=18080
for id in "$WITH" "$WITHOUT"; do
  port=$((port + 1))
  k port-forward "pod/$id" "$port:8080" >/dev/null 2>&1 &
  forwards+=($!)
done
P_WITH=18081 P_WITHOUT=18082
for _ in $(seq 1 50); do
  (exec 3<>/dev/tcp/127.0.0.1/$P_WITHOUT) 2>/dev/null && break
  sleep 0.2
done

# --- 1. the CRDs and their rules -----------------------------------------------
step "1. CRDs"
policy() { # name, sessionRef, extra spec lines
  printf 'apiVersion: browserjs.dev/v1alpha1\nkind: SessionPolicy\nmetadata:\n  name: %s\nspec:\n  sessionRef:\n    name: %s\n  kind: rego\n  source: "package browserjs.policy"\n%s' "$1" "$2" "${3:-}"
}
# refused <description> <text the message must contain>; the manifest on stdin
refused() {
  local out
  if out="$(k apply --dry-run=server -f - 2>&1)"; then
    fail "$1" "it was accepted"
  elif grep -qF "$2" <<<"$out"; then
    pass "$1"
  else
    fail "$1" "refused, but not for the reason expected ($2): $out"
  fi
}
accepted() {
  local out
  if out="$(k apply --dry-run=server -f - 2>&1)"; then pass "$1"; else fail "$1" "$out"; fi
}
policy s-cel01 s-cel01 | accepted "a SessionPolicy named after its session is accepted"
policy s-cel01 s-cel02 | refused "a SessionPolicy with another session's name is refused" "named after its session"
policy s-cel01 s-cel01 $'  management:\n    mode: iac\n' | refused "iac without a URL is refused" "must be an https URL"
policy s-cel01 s-cel01 $'  management:\n    mode: iac\n    managedURL: http://example.com/x\n' | refused "iac with an http URL is refused" "must be an https URL"
policy s-cel01 s-cel01 $'  management:\n    mode: iac\n    managedURL: https://example.com/x\n' | accepted "iac with an https URL is accepted"
policy not-a-session not-a-session | refused "a name that is not a session ID is refused" "sessionRef.name"
policy s-cel01 s-cel01 | k apply -f - >/dev/null
is "management defaults to the editor" editor "$(k get sessionpolicy s-cel01 -o jsonpath='{.spec.management.mode}')"
out="$(k patch sessionpolicy s-cel01 --type=merge -p '{"spec":{"sessionRef":{"name":"s-cel02"}}}' 2>&1)"
ok "a changed sessionRef is refused" "$(grep -F "sessionRef is immutable" <<<"$out")" "$out"
if k patch sessionpolicy s-cel01 --type=merge -p '{"spec":{"source":"package browserjs.policy\n"}}' >/dev/null 2>&1; then
  pass "the source of a SessionPolicy can be changed"
else fail "the source of a SessionPolicy can be changed"; fi
k delete sessionpolicy s-cel01 >/dev/null

token() { # name, id
  printf 'apiVersion: browserjs.dev/v1alpha1\nkind: APIToken\nmetadata:\n  name: %s\nspec:\n  id: %s\n  owner: ada@example.com\n  name: ci\n  scopes: ["policies:write"]\n  expiresAt: "2030-01-01T00:00:00Z"\n  sha256: %s\n' "$1" "$2" "$(printf x | sha256)"
}
token tok-abcdefghijkl abcdefghijkl | accepted "an APIToken named tok-<id> is accepted"
token tok-other abcdefghijkl | refused "an APIToken with another name is refused" "named tok-<spec.id>"
token tok-abcdefghijkl abcdefghijkl | k apply -f - >/dev/null
out="$(k patch apitoken tok-abcdefghijkl --type=merge -p '{"spec":{"name":"other"}}' 2>&1)"
ok "a changed APIToken is refused" "$(grep -F "a token is immutable" <<<"$out")" "$out"
if k patch apitoken tok-abcdefghijkl --subresource=status --type=merge -p '{"status":{"lastUsedTime":"2026-10-02T00:00:00Z"}}' >/dev/null 2>&1; then
  pass "an APIToken's status can be written"
else fail "an APIToken's status can be written"; fi
k delete apitoken tok-abcdefghijkl >/dev/null

# --- 4a. a new OPA has no bundle: not ready, and everything is denied -----------
step "4a. OPA before its first bundle"
k wait --for=jsonpath='{.status.phase}'=Running pod -l app=opa --timeout=180s >/dev/null
sleep 10 # several readiness periods
is "an OPA pod with no bundle is running but not ready" "false false" \
  "$(k get pods -l app=opa -o jsonpath='{range .items[*]}{.status.containerStatuses[0].ready} {end}' | xargs)"
is "the Service has no ready address" "" \
  "$(k get endpointslices -l kubernetes.io/service-name=opa-engine -o json | jq -r '[.items[].endpoints[]? | select(.conditions.ready) | .addresses[]] | join(" ")')"
got="$(call $P_WITH url 3)"
is "a browser call is denied while no OPA is ready" denied "$(outcome "$got")"
note "" && note "### No OPA replica ready (none has a bundle yet)" && note "" &&
  note "run_js call: $(seconds "$got"). What the agent's code saw:" && note "" && note '```' && note "$(seen "$got")" && note '```'

# --- 2. decisions -------------------------------------------------------------
step "2. decisions through the real mcp-js"
build_bundle "$work/one.tar.gz" test-1 "$WITH=no-scripting" || { echo "opa build failed"; exit 1; }
publish_bundle "$work/one.tar.gz"
k rollout status deploy/opa --timeout=120s
is "OPA is ready once it has the bundle" "true true" \
  "$(k get pods -l app=opa -o jsonpath='{range .items[*]}{.status.containerStatuses[0].ready} {end}' | xargs)"

opa_ips="$(k get pods -l app=opa -o jsonpath='{.items[*].status.podIP}')"
# The operator's own path: each replica, by address, with the operator's token.
# shellcheck disable=SC2086 # two addresses, two arguments
loaded="$(k exec deploy/policy-operator -- python3 -c '
import json, os, sys, urllib.request
for ip in sys.argv[1:]:
    r = urllib.request.Request("http://%s:8181/v1/data/browserjs/loaded" % ip, headers={"Authorization": "Bearer " + os.environ["OPA_TOKEN"]})
    print(json.dumps(json.load(urllib.request.urlopen(r, timeout=5)).get("result")))
' $opa_ips 2>&1 | xargs)"
want="{$WITH: sha256:$(sha256 <$contracts/examples/no-scripting.rego)}"
is "each replica reports what it has loaded to the operator's token" "$want $want" "$loaded"

got="$(call $P_WITH url 9)"
is "an allowed browser_execute runs" ran "$(outcome "$got")"
note "" && note "### Allowed call (policy: no-scripting, operation: url)" && note "" && note "run_js call: $(seconds "$got"). What the agent's code saw:" && note "" && note '```' && note "$(seen "$got")" && note '```'
got="$(call $P_WITH evaluate 9)"
is "a browser_execute the policy denies does not run" denied "$(outcome "$got")"
note "" && note "### Denied by the policy (policy: no-scripting, operation: evaluate)" && note "" && note "run_js call: $(seconds "$got"). What the agent's code saw:" && note "" && note '```' && note "$(seen "$got")" && note '```'
got="$(call $P_WITHOUT url 9)"
is "a session with no policy in the bundle is denied everything" denied "$(outcome "$got")"
note "" && note "### Session with no policy in the bundle (a pod nobody made a SessionPolicy for)" && note "" && note "run_js call: $(seconds "$got"). What the agent's code saw:" && note "" && note '```' && note "$(seen "$got")" && note '```'
is "the stub browser of that session was never called" 0 "$(k logs "$WITHOUT" -c browser | grep -c 'mcp tools/call')"

# A change applies with nothing restarted.
build_bundle "$work/two.tar.gz" test-2 "$WITH=unrestricted" "$WITHOUT=observe-only"
started="$(date +%s.%N)"
publish_bundle "$work/two.tar.gz"
applied=""
for _ in $(seq 1 100); do
  if [ "$(outcome "$(call $P_WITH evaluate)")" = ran ]; then
    applied="$(python3 -c "import time; print(round(time.time() - $started, 2))")"
    break
  fi
  sleep 0.1
done
ok "a changed policy applies without restarting anything (${applied:-not} after publishing, in seconds)" "$applied" "evaluate was still denied after 10 s"
note "" && note "### A policy change" && note "" && note "From publishing a new bundle to the first call judged by it: ${applied:-never} s (including one run_js round trip). No pod was restarted."
is "the session that had no policy is now judged by its own" ran "$(outcome "$(call $P_WITHOUT screenshot)")"
is "and its policy is not the other session's" denied "$(outcome "$(call $P_WITHOUT evaluate)")"

# OPA's API from inside a session pod.
api="$(k exec "$WITH" -c browser -- python3 -c '
import json, urllib.request, urllib.error
def ask(method, path, body=None):
    r = urllib.request.Request("http://opa.browserjs-sessions.svc:8181" + path, data=body, method=method)
    try: return urllib.request.urlopen(r, timeout=5).status
    except urllib.error.HTTPError as e: return e.code
i = json.dumps({"input": {"operation": "mcp_call_tool", "server": "browser", "tool": "browser_execute", "arguments": {"operations": []}}}).encode()
print(ask("POST", "/v1/data/browserjs/decision/s-pol01/mcp_tools", i),
      ask("POST", "/v1/data/browserjs/decision/s-pol01/mcp_tools?explain=full", i),
      ask("GET", "/v1/policies"), ask("GET", "/v1/data/browserjs/loaded"), ask("GET", "/v1/data"),
      ask("PUT", "/v1/policies/x", b"package x"), ask("POST", "/v1/data/browserjs/tenant/s-pol01", i))
' 2>&1)"
is "from a session pod OPA gives decisions and nothing else (decision, ?explain, policies, loaded, data, a write, a tenant document)" \
  "200 401 401 401 401 401 401" "$api"

# --- 3. who reaches whom ----------------------------------------------------------
step "3. reachability"
opa_pod="$(k get pods -l app=opa -o jsonpath='{.items[0].metadata.name}')"
opa_ip="$(k get pod "$opa_pod" -o jsonpath='{.status.podIP}')"
operator_ip="$(k get pods -l app=policy-operator -o jsonpath='{.items[0].status.podIP}')"
backend_ip="$(k get pods -l app=backend -o jsonpath='{.items[0].status.podIP}')"
other_ip="$(k get pod "$WITHOUT" -o jsonpath='{.status.podIP}')"
targets=("opa.$NS.svc:8181" "$opa_ip:8181" "policy-operator.$NS.svc:8080" "$operator_ip:8080" "backend.$NS.svc:80" "$backend_ip:8080"
  "$other_ip:8080" "webhook-redis.$NS.svc:6379" kubernetes.default.svc:443 1.1.1.1:443)
note "" && note "### Reachability" && note "" && note "| From | To | Result |" && note "|---|---|---|"

out="$(k exec "$WITH" -c browser -- python3 -c "$PROBE" "${targets[@]}" 2>&1)"
expect "a session pod" "$out" "opa.$NS.svc:8181" open
expect "a session pod" "$out" "$opa_ip:8181" open
expect "a session pod" "$out" "policy-operator.$NS.svc:8080" open
expect "a session pod" "$out" "$operator_ip:8080" open
expect "a session pod" "$out" "backend.$NS.svc:80" closed
expect "a session pod" "$out" "$backend_ip:8080" closed
expect "a session pod" "$out" "$other_ip:8080" closed
expect "a session pod" "$out" "kubernetes.default.svc:443" closed
expect "a session pod" "$out" "1.1.1.1:443" open
expect "a session pod" "$out" "webhook-redis.$NS.svc:6379" closed

# The OPA image has no shell: an ephemeral container in its pod, which shares
# the pod's network and so its NetworkPolicy.
k debug "pod/$opa_pod" -c probe --image="$python_image" -- python3 -c "$PROBE" "${targets[@]}" >/dev/null 2>&1
out=""
for _ in $(seq 1 90); do
  out="$(k logs "$opa_pod" -c probe 2>/dev/null)"
  grep -q '^done$' <<<"$out" && break
  sleep 1
done
expect "OPA" "$out" "policy-operator.$NS.svc:8080" open
expect "OPA" "$out" "$operator_ip:8080" open
expect "OPA" "$out" "backend.$NS.svc:80" closed
expect "OPA" "$out" "$backend_ip:8080" closed
expect "OPA" "$out" "$other_ip:8080" closed
expect "OPA" "$out" "kubernetes.default.svc:443" closed
expect "OPA" "$out" "1.1.1.1:443" closed

out="$(k exec deploy/policy-operator -- python3 -c "$PROBE" "${targets[@]}" 2>&1)"
expect "the operator" "$out" "$opa_ip:8181" open
expect "the operator" "$out" "webhook-redis.$NS.svc:6379" open
expect "the operator" "$out" "kubernetes.default.svc:443" open
expect "the operator" "$out" "backend.$NS.svc:80" closed
expect "the operator" "$out" "$other_ip:8080" closed

# Something else in the namespace, with none of the labels.
k run policy-test-stranger --image="$python_image" --restart=Never --labels=app=policy-test-stranger \
  --overrides='{"spec":{"securityContext":{"runAsNonRoot":true,"runAsUser":65532},"terminationGracePeriodSeconds":1}}' \
  --command -- sleep 600 >/dev/null
k wait --for=condition=Ready pod/policy-test-stranger --timeout=120s >/dev/null
out="$(k exec policy-test-stranger -- python3 -c "$PROBE" "${targets[@]}" 2>&1)"
expect "another pod of the namespace" "$out" "opa.$NS.svc:8181" closed
expect "another pod of the namespace" "$out" "$opa_ip:8181" closed
expect "another pod of the namespace" "$out" "policy-operator.$NS.svc:8080" closed
expect "another pod of the namespace" "$out" "$operator_ip:8080" closed
k delete pod policy-test-stranger --wait=false >/dev/null

# --- 4. OPA replicas going away --------------------------------------------------
step "4. OPA replicas"
# One pod after another is deleted and replaced, as a node drain, a Spot
# preemption or a rollout does, while calls are made without a pause. Under
# enforcing a call that fails here is a tool call an agent was refused for no
# reason of its own, so none may: REPLACEMENTS times over.
replacements="${REPLACEMENTS:-20}"
stamp() { printf '%s %s\n' "$(date +%s.%N)" "$*" >>"$work/timeline"; }
call $P_WITH url 3600s >"$work/during.jsonl" &
caller=$!
# Evidence for when a call fails. A new connection to the Service every 50 ms
# from inside the session's pod (mcp-js keeps its connections; this does
# not), and every change of an OPA pod's readiness, both with the time.
k exec "$WITH" -c browser -- python3 -u -c '
import socket, time
n = 0
while True:
    t = time.time()
    n += 1
    try:
        # The name first, by itself: a lookup that stalls is not a replica
        # that does not answer.
        address = socket.getaddrinfo("opa.'"$NS"'.svc", 8181, socket.AF_INET, socket.SOCK_STREAM)[0][4]
        looked = time.time() - t
        if looked > 0.5: print("%.3f lookup took %.3f s" % (t, looked))
        c = socket.create_connection(address, timeout=1)
        c.settimeout(1)
        c.sendall(b"GET /health HTTP/1.1\r\nHost: opa\r\nConnection: close\r\n\r\n")
        ok = c.recv(64).startswith(b"HTTP/1.1 200")
        c.close()
        if not ok: print("%.3f bad answer after %.3f s" % (t, time.time() - t))
    except Exception as e:
        print("%.3f %r after %.3f s" % (t, e, time.time() - t))
    if n % 1000 == 0: print("%.3f %d probes so far" % (t, n))
    time.sleep(0.05)
' >"$work/probe.log" 2>&1 &
prober=$!
(
  last=""
  while :; do
    now="$(k get pods -l app=opa -o json 2>/dev/null | jq -r '[.items[] | "\(.metadata.name | .[-5:])@\(.status.podIP // "-"):\(if .metadata.deletionTimestamp then "terminating" elif any(.status.conditions[]?; .type == "Ready" and .status == "True") then "ready" else "notready" end)"] | sort | join(" ")')"
    [ "$now" = "$last" ] || { printf '%s pods %s\n' "$(date +%s.%N)" "$now" >>"$work/timeline"; last="$now"; }
    sleep 0.2
  done
) &
watcher=$!
# First with nothing happening to OPA at all, for as long as the replacements
# will take: a call that fails here did not fail because of one.
stamp "quiet from here"
sleep "${QUIET_SECONDS:-240}"
stamp "quiet until here"
quiet="$(jq -rs --argjson until "$(date +%s)" '[.[] | select(.at < $until)] | "\(length) calls, outcomes: \(map(.outcome) | group_by(.) | map("\(.[0]) \(length)") | join(", "))"' <"$work/during.jsonl")"
echo "      with no replacement: $quiet"
for i in $(seq 1 "$replacements"); do
  victim="$(k get pods -l app=opa -o json | jq -r '[.items[] | select(.metadata.deletionTimestamp == null) | select(any(.status.conditions[]?; .type == "Ready" and .status == "True"))] | sort_by(.metadata.creationTimestamp) | .[0].metadata.name')"
  stamp "$i delete $victim"
  k delete pod "$victim" --wait=true >/dev/null
  stamp "$i gone $victim"
  k rollout status deploy/opa --timeout=120s >/dev/null
  stamp "$i replaced"
  sleep 1
done
kill "$caller" "$prober" "$watcher" 2>/dev/null
wait "$caller" "$prober" "$watcher" 2>/dev/null || true
got="$(cat "$work/during.jsonl")"
is "no call fails while OPA pods are deleted and replaced, $replacements times ($(jq -s length <<<"$got") calls)" ran "$(outcome "$got")"
if [ "$(outcome "$got")" != ran ]; then
  # When, against the deletions, and what the agent's code and mcp-js saw.
  echo "      calls that did not run (at, seconds, seen):"
  jq -rs '.[] | select(.outcome != "ran") | "      \(.at) \(.seconds)s \(.outcome): \(.seen | .[0:200])"' <<<"$got"
  echo "      the prober (a lookup and a new connection every 50 ms): what it logged (at, what):"
  sed 's/^/      /' "$work/probe.log" | head -60
  echo "      timeline:"
  sort -n "$work/timeline" | sed 's/^/      /'
fi
note "" && note "### OPA pods deleted and replaced, $replacements times, while calls are made" && note "" &&
  probed="$(grep -c 'probes so far' "$work/probe.log" || true)"
slow="$(grep -c 'lookup took' "$work/probe.log" || true)"
broken="$(grep -vc 'probes so far\|lookup took' "$work/probe.log" || true)"
echo "      the prober: about ${probed}000 probes, $slow lookups over 0.5 s, $broken failed connections"
note "With no replacement first: $quiet. Then, with the replacements:" && note "" &&
  note "$(jq -rs '"\(length) calls, outcomes: \(map(.outcome) | group_by(.) | map("\(.[0]) \(length)") | join(", ")); slowest \(map(.seconds) | max) s"' <<<"$got")"

k scale deploy/opa --replicas=0 >/dev/null
k wait --for=delete pod -l app=opa --timeout=120s >/dev/null
got="$(call $P_WITH url 5)"
is "with no OPA pod every call is denied" denied "$(outcome "$got")"
note "" && note "### No OPA pod at all (the Service has no address)" && note "" && note "run_js call: $(seconds "$got"). What the agent's code saw:" && note "" && note '```' && note "$(seen "$got")" && note '```'
k scale deploy/opa --replicas=2 >/dev/null
k rollout status deploy/opa --timeout=120s >/dev/null
recovered=""
for _ in $(seq 1 50); do
  [ "$(outcome "$(call $P_WITH url)")" = ran ] && recovered=1 && break
  sleep 0.2
done
ok "calls are allowed again when OPA is back" "$recovered"

# OPA there but not answering: its NetworkPolicy without the gateway rule,
# which drops the packets. Recorded, not asserted on its length: connections
# mcp-js already holds may outlive the change.
k patch networkpolicy opa --type=json -p '[{"op":"remove","path":"/spec/ingress/0/from/0"}]' >/dev/null
sleep 3
got="$(call $P_WITH url 3)"
note "" && note "### OPA not answering (packets dropped)" && note "" && note "run_js call: $(seconds "$got"), outcomes: $(outcome "$got"). What the agent's code saw:" && note "" && note '```' && note "$(seen "$got")" && note '```'
echo "INFO  OPA unreachable: $(outcome "$got"), $(seconds "$got")"
kubectl apply -k test/policy >/dev/null
recovered=""
for _ in $(seq 1 100); do
  [ "$(outcome "$(call $P_WITH url)")" = ran ] && recovered=1 && break
  sleep 0.2
done
ok "calls are allowed again when OPA is reachable again" "$recovered"

# --- 5. the operator itself -------------------------------------------------------
# With OPERATOR_IMAGE (an image the cluster has), the stand-in operator is
# replaced by deploy/base's own Deployment, and policies are SessionPolicy
# objects from here on.
if [ -n "${OPERATOR_IMAGE:-}" ]; then
  step "5. the real operator ($OPERATOR_IMAGE)"
  session_policy() { # id, example name
    jq -n --arg id "$1" --rawfile source "$contracts/examples/$2.rego" \
      '{apiVersion: "browserjs.dev/v1alpha1", kind: "SessionPolicy", metadata: {name: $id},
        spec: {sessionRef: {name: $id}, kind: "rego", source: $source}}'
  }
  # until <seconds> <operation> <outcome>: the first time a call has it.
  until_outcome() {
    local started
    started="$(date +%s.%N)"
    for _ in $(seq 1 $(($1 * 4))); do
      if [ "$(outcome "$(call $P_WITH "$2")")" = "$3" ]; then
        python3 -c "import time; print(round(time.time() - $started, 2))"
        return
      fi
      sleep 0.25
    done
  }
  kubectl kustomize deploy/base | sed "s|image: browserjs/policy-operator\$|image: $OPERATOR_IMAGE|" |
    kubectl apply -l app=policy-operator -f - >/dev/null
  if k rollout status deploy/policy-operator --timeout=180s; then
    pass "the operator starts and becomes ready with the Role and NetworkPolicy of deploy/base"
  else
    fail "the operator starts and becomes ready with the Role and NetworkPolicy of deploy/base"
    k describe pod -l app=policy-operator | tail -30
  fi
  operator_started="$(date +%s)"
  ok "with no SessionPolicy the operator's bundle denies a session that was allowed" "$(until_outcome 30 url denied)"

  session_policy "$WITH" no-scripting | k apply -f - >/dev/null
  started="$(date +%s.%N)"
  if k wait --for=condition=Ready "sessionpolicy/$WITH" --timeout=60s >/dev/null 2>&1; then
    ready="$(python3 -c "import time; print(round(time.time() - $started, 2))")"
    pass "a SessionPolicy becomes Ready: status written through the subresource, every OPA replica asked (${ready}s)"
  else
    ready=never
    fail "a SessionPolicy becomes Ready" "$(k get sessionpolicy "$WITH" -o json | jq -c .status)"
  fi
  status="$(k get sessionpolicy "$WITH" -o json | jq -c '.status // {}')"
  is "it is loaded by both replicas" "2 of 2" "$(jq -r '"\(.loaded.replicas) of \(.loaded.total)"' <<<"$status")"
  ok "its status has the hash and the Rego" "$(jq -r 'select((.hash // "") | startswith("sha256:")) | select((.rego // "") | contains("package browserjs.policy")) | "yes"' <<<"$status")" "$status"
  is "an allowed call runs" ran "$(outcome "$(call $P_WITH url)")"
  is "a denied call does not" denied "$(outcome "$(call $P_WITH evaluate)")"
  is "the session without a SessionPolicy is denied" denied "$(outcome "$(call $P_WITHOUT url)")"

  k patch sessionpolicy "$WITH" --type=merge -p "$(session_policy "$WITH" unrestricted | jq -c '{spec: {source: .spec.source}}')" >/dev/null
  edited="$(until_outcome 30 evaluate ran)"
  ok "an edit applies with nothing restarted (${edited:-not} s after the patch)" "$edited"

  # The desktop tool (mouse, keyboard, screen). mcp-js's own file policy
  # allows it, so the session's policy decides, tool by tool
  # (docs/contracts/policy/rego-contract.md): the unrestricted policy, which
  # a new session gets, allows it; a policy that restricts the browser
  # denies it, because the desktop could walk around its rules.
  is "under the unrestricted policy desktop_execute runs" ran "$(outcome "$(TOOL=desktop_execute call $P_WITH mouse.click)")"
  k patch sessionpolicy "$WITH" --type=merge -p "$(session_policy "$WITH" no-scripting | jq -c '{spec: {source: .spec.source}}')" >/dev/null
  desktop=""
  for _ in $(seq 1 60); do
    [ "$(outcome "$(TOOL=desktop_execute call $P_WITH mouse.click)")" = denied ] && desktop=1 && break
    sleep 0.25
  done
  ok "under no-scripting desktop_execute is denied" "$desktop" "$(k get sessionpolicy "$WITH" -o json | jq -c .status.errors)"
  is "and browser_execute still runs" ran "$(outcome "$(call $P_WITH url)")"
  is "and no warning is reported for a preset" "[]" "$(k get sessionpolicy "$WITH" -o json | jq -c '.status.warnings // []')"

  # Shell commands: the "exec" server (mcp-exec), a second upstream server of
  # mcp-js. The same rule: its own file policy allows it, the session's
  # decides, and a policy that does not name it denies it.
  is "under no-scripting, which does not name the exec server, a command is denied" denied "$(outcome "$(SERVER="exec" call $P_WITH 'git status')")"
  rego='package browserjs.policy\n\nimport rego.v1\n\nallow_tool_call if {\n\tinput.server == \"exec\"\n\tinput.tool == \"exec\"\n\tinput.arguments.bin == \"git\"\n\tinput.arguments.args[0] in {\"status\", \"log\"}\n}\n'
  k patch sessionpolicy "$WITH" --type=merge -p "{\"spec\":{\"kind\":\"rego\",\"source\":\"$rego\"}}" >/dev/null
  command=""
  for _ in $(seq 1 60); do
    [ "$(outcome "$(SERVER="exec" call $P_WITH 'git status')")" = ran ] && command=1 && break
    sleep 0.25
  done
  ok "a Rego policy on the program and its first argument allows git status" "$command" "$(k get sessionpolicy "$WITH" -o json | jq -c .status.errors)"
  is "and another subcommand on its list" ran "$(outcome "$(SERVER="exec" call $P_WITH 'git log --oneline')")"
  is "a subcommand that is not on it is denied" denied "$(outcome "$(SERVER="exec" call $P_WITH 'git push')")"
  is "another program is denied" denied "$(outcome "$(SERVER="exec" call $P_WITH 'sh -c git')")"
  is "and so is the browser, which that policy does not name" denied "$(outcome "$(call $P_WITH url)")"
  k patch sessionpolicy "$WITH" --type=merge -p "$(session_policy "$WITH" unrestricted | jq -c '{spec: {source: .spec.source}}')" >/dev/null
  back=""
  for _ in $(seq 1 60); do
    [ "$(outcome "$(TOOL=desktop_execute call $P_WITH mouse.click)")" = ran ] && back=1 && break
    sleep 0.25
  done
  ok "back on the unrestricted policy, desktop_execute runs again" "$back"

  k patch sessionpolicy "$WITH" --type=merge -p '{"spec":{"source":"{ this is not a policy"}}' >/dev/null
  if k wait --for=condition=Compiled=False "sessionpolicy/$WITH" --timeout=60s >/dev/null 2>&1; then
    pass "a policy that does not compile is reported (Compiled=False)"
  else
    fail "a policy that does not compile is reported (Compiled=False)" "$(k get sessionpolicy "$WITH" -o json | jq -c .status.conditions)"
  fi
  ok "with its errors" "$(k get sessionpolicy "$WITH" -o json | jq -r '.status.errors[0].code // empty')"
  is "and the previous policy stays in force" ran "$(outcome "$(call $P_WITH evaluate)")"

  k delete sessionpolicy "$WITH" --timeout=60s >/dev/null 2>&1
  is "a deleted SessionPolicy goes (the operator's finalizer lets it)" "" "$(k get sessionpolicy "$WITH" --ignore-not-found -o name)"
  removed="$(until_outcome 30 url denied)"
  ok "and its session is denied (${removed:-not} s after the delete)" "$removed"

  # Long enough for the kubelet's liveness probe (kopf's, on 8081) to have
  # failed three times if it did not answer.
  while [ $(($(date +%s) - operator_started)) -lt 75 ]; do sleep 5; done
  is "the operator was not restarted (its liveness endpoint answers the kubelet)" "true 0" \
    "$(k get pods -l app=policy-operator -o jsonpath='{.items[0].status.containerStatuses[0].ready} {.items[0].status.containerStatuses[0].restartCount}')"
  note "" && note "### The real operator" && note "" &&
    note "SessionPolicy created to Ready: ${ready} s. Edit to the first call judged by it: ${edited:-never} s. Delete to the first call denied: ${removed:-never} s." &&
    note "" && note '```' && note "$(k logs deploy/policy-operator --tail=40 2>&1)" && note '```'
fi

printf '\n%d passed, %d failed\n' "$passed" "$failed"
note "" && note "$passed checks passed, $failed failed."
[ "$failed" -eq 0 ]
