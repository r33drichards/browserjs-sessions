#!/usr/bin/env bash
# Metering and billing on a kind cluster: the checks of track B of the plan
# (docs/plans/2026-10-02-metering-billing-tracks.md) that need a cluster.
#
#   test/billing/run.sh                       against the current kubectl context
#   RESTORE_CONTEXT=kind-restore test/billing/run.sh
#                                             also restores the export into
#                                             that second, empty cluster
#   RESULTS=out.md test/billing/run.sh        also writes what was seen
#
# Real: the three CRDs and their rules, deploy/base's Roles, NetworkPolicy,
# catalogue ConfigMap and the way the backend and the operator mount it, the
# backend's Stripe variables, hack/stripe-secret.sh, the export CronJob of
# deploy/gke and hack/billing-restore.sh. Stand-ins (stub.py): the backend
# and the billing operator themselves, which are other tracks' work; the
# operator's is the placeholder image until its own exists.
#
# Needs: a cluster whose CNI enforces NetworkPolicy (kind 0.24 or later),
# kubectl and jq. It creates and deletes things in the namespace
# browserjs-sessions: never point it at a cluster that matters.
# .github/workflows/billing-kind.yml runs it. No Stripe key is involved: the
# two "keys" below are made up here.
set -uo pipefail
cd "$(dirname "$0")/../.." || exit 1

NS=browserjs-sessions
RESULTS="${RESULTS:-/dev/null}"
RESTORE_CONTEXT="${RESTORE_CONTEXT:-}"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

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
is() { if [ "$2" = "$3" ]; then pass "$1"; else fail "$1" "expected: $2   got: $3"; fi; }
note() { printf '%s\n' "$*" >>"$RESULTS"; }
k() { kubectl -n "$NS" "$@"; }
step() { printf '\n--- %s\n' "$1"; }

# accepts <description> <command...>: the API server takes it.
accepts() {
  local what="$1" out
  shift
  if out="$("$@" 2>&1)"; then pass "$what"; else fail "$what" "$(tail -3 <<<"$out")"; fi
}
# refuses <description> <the rule's message> <command...>: the API server
# refuses it, and says why in the words of the contract.
refuses() {
  local what="$1" message="$2" out
  shift 2
  if out="$("$@" 2>&1)"; then
    fail "$what" "it was accepted"
  elif grep -qF -- "$message" <<<"$out"; then
    pass "$what"
    note "| $what | \`$message\` |"
  else
    fail "$what" "refused, but not with \"$message\": $(tail -2 <<<"$out")"
  fi
}
merge() { # kind, name, JSON merge patch
  k patch "$1.browserjs.dev" "$2" --type=merge -p "$3"
}

echo "cluster: $(kubectl config current-context)"
note "Run $(date -u +%Y-%m-%dT%H:%MZ), $(kubectl version -o json 2>/dev/null | jq -r '"Kubernetes \(.serverVersion.gitVersion)"'), commit ${GITHUB_SHA:-$(git rev-parse --short HEAD)}."

# --- deploy ------------------------------------------------------------------
step "deploy"
kubectl apply -f deploy/base/namespace.yaml >/dev/null
if ! kubectl apply -k test/billing >"$work/apply.log" 2>&1; then
  cat "$work/apply.log"
  echo "apply failed: a CRD the API server does not accept is a contract problem"
  exit 1
fi
accepts "the three CRDs are accepted and established" \
  kubectl wait --for=condition=Established --timeout=60s \
  crd/accounts.browserjs.dev crd/grants.browserjs.dev crd/usageperiods.browserjs.dev
# Neither Stripe object exists, and both pods start: the three references
# are optional.
is "no secret/stripe and no configmap/billing-mode to begin with" "" \
  "$(k get secret/stripe configmap/billing-mode --ignore-not-found -o name)"
for deployment in backend billing-operator; do
  if ! k rollout status "deploy/$deployment" --timeout=240s; then
    k describe pods | tail -60
    echo "the stand-in pods did not start"
    exit 1
  fi
done
pass "the backend and the operator start with no Stripe Secret and no mode"

# --- 1. the CRDs' rules --------------------------------------------------------
step "1. CRD rules"
note
note "### The CRDs' rules"
note
note "| Refused | With |"
note "|---|---|"
HASH=0123456789abcdef0123456789abcdef
ACCOUNT="acct-$HASH"
account() { # name, owner hash
  cat <<YAML
apiVersion: browserjs.dev/v1alpha1
kind: Account
metadata:
  name: $1
  labels:
    browserjs.dev/owner: "$2"
spec:
  owner: alice@example.com
  ownerHash: "$2"
YAML
}
grant() { # name, source, extra spec lines
  cat <<YAML
apiVersion: browserjs.dev/v1alpha1
kind: Grant
metadata:
  name: $1
  labels:
    browserjs.dev/owner: "$HASH"
spec:
  account: $ACCOUNT
  source: $2
  amountMicros: 5000000
  validFrom: "2026-10-01T00:00:00Z"
  key: $2/$1
$3
YAML
}
apply() { k apply -f -; }

refuses "an Account whose name is not its hash" "an Account is named acct-<spec.ownerHash>" \
  apply <<<"$(account acct-ffffffffffffffffffffffffffffffff "$HASH")"
accepts "an Account named by its hash" apply <<<"$(account "$ACCOUNT" "$HASH")"
refuses "a changed owner" "owner and ownerHash cannot be changed" \
  merge accounts "$ACCOUNT" '{"spec":{"owner":"mallory@example.com"}}'
accepts "stripeCustomerId set for the first time" \
  merge accounts "$ACCOUNT" '{"spec":{"stripeCustomerId":"cus_Test1"}}'
refuses "a changed stripeCustomerId" "stripeCustomerId cannot be changed once set" \
  merge accounts "$ACCOUNT" '{"spec":{"stripeCustomerId":"cus_Test2"}}'
refuses "a removed stripeCustomerId" "stripeCustomerId cannot be changed once set" \
  merge accounts "$ACCOUNT" '{"spec":{"stripeCustomerId":null}}'
accepts "signupCredit decided" \
  merge accounts "$ACCOUNT" '{"spec":{"signupCredit":{"state":"refused","reason":"prepaid","at":"2026-10-01T00:00:00Z"}}}'
refuses "a signupCredit changed once set" "the sign-up credit is decided once" \
  merge accounts "$ACCOUNT" '{"spec":{"signupCredit":{"state":"granted","reason":null,"at":"2026-10-02T00:00:00Z"}}}'
accepts "any other field of an Account's spec still changes" \
  merge accounts "$ACCOUNT" '{"spec":{"paymentMethod":{"present":true,"ids":["pm_Test1"],"readAt":"2026-10-01T00:00:00Z"}}}'

accepts "a Grant with an expiry" apply <<<"$(grant g-purchase purchase '  expiresAt: "2027-10-01T00:00:00Z"')"
refuses "a changed Grant (its amount)" "a grant is immutable; it can only be revoked" \
  merge grants g-purchase '{"spec":{"amountMicros":9000000}}'
refuses "a changed Grant (its expiry removed)" "a grant is immutable; it can only be revoked" \
  merge grants g-purchase '{"spec":{"expiresAt":null}}'
accepts "a Grant revoked" \
  merge grants g-purchase '{"spec":{"revoked":{"reason":"refund","at":"2026-10-02T00:00:00Z"}}}'
refuses "an un-revoked Grant" "a revoked grant stays revoked" \
  merge grants g-purchase '{"spec":{"revoked":null}}'
refuses "a non-admin Grant with no expiry" "only an admin's grant may have no expiry" \
  apply <<<"$(grant g-signup signup '')"
accepts "an admin's Grant with no expiry" apply <<<"$(grant g-admin admin '')"

period() { # charged
  cat <<YAML
apiVersion: browserjs.dev/v1alpha1
kind: UsagePeriod
metadata:
  name: up-$HASH-202609
  labels:
    browserjs.dev/owner: "$HASH"
spec:
  account: $ACCOUNT
  start: "2026-09-01T00:00:00Z"
  end: "2026-10-01T00:00:00Z"
  plan: payg
  awakeSeconds: 3600
  awakeMicros: 200000
  diskMicros: 1400000
  chargedMicros: $1
  bySource:
    signup: $1
  days:
    - date: "2026-09-30"
      awakeSeconds: 3600
YAML
}
accepts "a UsagePeriod" apply <<<"$(period 1600000)"
refuses "a changed UsagePeriod" "a closed period is immutable" apply <<<"$(period 1)"

# --- 2. who may do what ----------------------------------------------------------
step "2. RBAC"
can() { # ServiceAccount, verb, resource, [subresource]
  kubectl -n "$NS" auth can-i "$2" "$3" ${4:+--subresource="$4"} --as="system:serviceaccount:$NS:$1" 2>/dev/null | tail -1
}
note
note "### RBAC"
note
note "| ServiceAccount | May | Answer |"
note "|---|---|---|"
may() { # ServiceAccount, yes|no, verb, resource, [subresource]
  local got what="$3 $4${5:+/$5}"
  got="$(can "$1" "$3" "$4" "${5:-}")"
  note "| $1 | $what | ${got:-no answer} |"
  is "$1: $what is $2" "$2" "$got"
}
may backend no patch accounts.browserjs.dev status
may backend no update accounts.browserjs.dev status
may backend yes create accounts.browserjs.dev
may backend yes patch accounts.browserjs.dev
may backend yes create grants.browserjs.dev
may backend no delete grants.browserjs.dev
may backend no create usageperiods.browserjs.dev
may billing-operator yes patch accounts.browserjs.dev status
may billing-operator yes patch grants.browserjs.dev status
may billing-operator no patch accounts.browserjs.dev
may billing-operator no update accounts.browserjs.dev
may billing-operator no create accounts.browserjs.dev
may billing-operator no create grants.browserjs.dev
may billing-operator no patch grants.browserjs.dev
may billing-operator yes delete grants.browserjs.dev
may billing-operator yes create usageperiods.browserjs.dev
for account in backend billing-operator; do
  for verb in get list; do
    may "$account" no "$verb" secrets
    may "$account" no "$verb" configmaps
  done
done

# The same, as requests and not as questions.
as() { # ServiceAccount, kubectl arguments
  local account="$1"
  shift
  k --as="system:serviceaccount:$NS:$account" "$@"
}
refuses "the backend's request to patch an Account's status" "Forbidden" \
  as backend patch accounts.browserjs.dev "$ACCOUNT" --subresource=status --type=merge -p '{"status":{"level":"ok"}}'
refuses "the operator's request to patch an Account's spec" "Forbidden" \
  as billing-operator patch accounts.browserjs.dev "$ACCOUNT" --type=merge -p '{"spec":{"exempt":true}}'
refuses "the operator's request to create a Grant" "Forbidden" \
  as billing-operator apply -f - <<<"$(grant g-operator admin '')"
refuses "the operator's request to read a Secret" "Forbidden" as billing-operator get secrets
STATUS='{"status":{"level":"ok","plan":"payg","balanceMicros":3400000,"meter":{"observedAt":"2026-10-02T00:00:00Z","consumed":[{"grant":"g-admin","micros":1600000}]}}}'
accepts "the operator's request to patch an Account's status" \
  as billing-operator patch accounts.browserjs.dev "$ACCOUNT" --subresource=status --type=merge -p "$STATUS"
is "a patch of the status leaves the spec alone" "alice@example.com true" \
  "$(k get accounts.browserjs.dev "$ACCOUNT" -o jsonpath='{.spec.owner} {.spec.paymentMethod.present}')"
# A Grant's status the operator may patch and not get (asked above), which
# kubectl's patch cannot do: it reads first. Written here as the admin, for
# the export further down.
accepts "a Grant's status" \
  k patch grants.browserjs.dev g-admin --subresource=status --type=merge -p '{"status":{"state":"active","consumedMicros":1600000}}'
# No Sandbox CRD on this cluster, so can-i cannot be asked: the rule itself.
is "billing-operator: get, list, watch sandboxes, and no more" "get list watch" \
  "$(k get role billing-operator -o json | jq -r '[.rules[] | select(.resources == ["sandboxes"] and .apiGroups == ["agents.x-k8s.io"]) | .verbs[]] | join(" ")')"

# --- 3. who reaches whom ---------------------------------------------------------
step "3. NetworkPolicy"
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
note
note "### Who reaches whom"
note
note "| From | To | Result |"
note "|---|---|---|"
# expect <from> <output> <target> open|closed
expect() {
  local got
  got="$(awk -v t="$3" '$1 == t { print $2 }' <<<"$2")"
  note "| $1 | \`$3\` | ${got:-no answer} |"
  case "$4:$got" in
    open:open) pass "$1 reaches $3" ;;
    closed:blocked) pass "$1 does not reach $3 ($got)" ;;
    *) fail "$1 and $3: want $4" "got: ${got:-nothing}" ;;
  esac
}
backend_ip="$(k get pods -l app=backend -o jsonpath='{.items[0].status.podIP}')"
operator_ip="$(k get pods -l app=billing-operator -o jsonpath='{.items[0].status.podIP}')"
dns_ip="$(kubectl -n kube-system get service kube-dns -o jsonpath='{.spec.clusterIP}')"
out="$(k exec deploy/billing-operator -- python3 -c "$PROBE" \
  kubernetes.default.svc:443 "$dns_ip:53" "backend.$NS.svc:80" "$backend_ip:8080" 1.1.1.1:80 1.1.1.1:53 2>&1)"
# By name: the operator resolves names, so DNS is reached.
expect operator "$out" kubernetes.default.svc:443 open
expect operator "$out" "$dns_ip:53" open
expect operator "$out" "backend.$NS.svc:80" closed
expect operator "$out" "$backend_ip:8080" closed
expect operator "$out" 1.1.1.1:80 closed
expect operator "$out" 1.1.1.1:53 closed
# Nothing reaches it: the backend is the neighbour that could try. That the
# listener is there is shown by the operator reaching itself.
out="$(k exec deploy/billing-operator -- python3 -c "$PROBE" "127.0.0.1:8081" 2>&1)"
expect "the operator (itself)" "$out" 127.0.0.1:8081 open
out="$(k exec deploy/backend -- python3 -c "$PROBE" "$operator_ip:8081" 2>&1)"
expect backend "$out" "$operator_ip:8081" closed
note
note "The operator's rule for the API server is ports 443 and 6443 to anywhere (a NetworkPolicy cannot name the API server), so the internet on 443 is not closed to it; everything else is."

# --- 4. the catalogue, and Stripe's Secret -----------------------------------------
step "4. catalogue"
uids() { k get pods -l 'app in (backend,billing-operator)' -o jsonpath='{range .items[*]}{.metadata.uid} {.status.containerStatuses[0].restartCount}{"\n"}{end}' | sort; }
before="$(uids)"
is "the backend has the catalogue beside the blueprint" "blueprint.yaml catalogue.yaml" \
  "$(k exec deploy/backend -- sh -c 'cd /etc/browserjs && ls' | tr '\n' ' ' | sed 's/ $//')"
for pod in backend billing-operator; do
  if k exec "deploy/$pod" -- cat /etc/browserjs/catalogue.yaml | cmp -s - docs/contracts/billing/catalogue.yaml; then
    pass "$pod reads the contract's catalogue at /etc/browserjs/catalogue.yaml"
  else
    fail "$pod reads the contract's catalogue at /etc/browserjs/catalogue.yaml"
  fi
done
sed 's/awakeMicrosPerHour: 200000 /awakeMicrosPerHour: 250000 /' deploy/base/catalogue.yaml >"$work/catalogue.yaml"
grep -q 'awakeMicrosPerHour: 250000' "$work/catalogue.yaml" || fail "the test could not change the catalogue"
k create configmap billing-catalogue --from-file=catalogue.yaml="$work/catalogue.yaml" --dry-run=client -o yaml |
  k apply -f - >/dev/null
started=$SECONDS
for pod in backend billing-operator; do
  seen=""
  # The kubelet's sync period, about a minute, plus its cache.
  for _ in $(seq 1 90); do
    if k exec "deploy/$pod" -- grep -q 'awakeMicrosPerHour: 250000' /etc/browserjs/catalogue.yaml 2>/dev/null; then
      seen=1
      break
    fi
    sleep 2
  done
  if [ -n "$seen" ]; then
    pass "a changed catalogue reaches $pod ($((SECONDS - started)) s after the apply)"
    note
    note "A changed catalogue reached \`$pod\` $((SECONDS - started)) s after the apply."
  else
    fail "a changed catalogue reaches $pod" "not after $((SECONDS - started)) s"
  fi
done
is "without a restart: the same pods, no container restarted" "$before" "$(uids)"

step "4. hack/stripe-secret.sh"
# Made up here, in the shape of the real ones. Never a real key.
fake_key="rk_test_$(head -c 18 /dev/urandom | od -An -tx1 | tr -d ' \n')"
fake_secret="whsec_$(head -c 18 /dev/urandom | od -An -tx1 | tr -d ' \n')"
stripe() { # mode; output and $GITHUB_OUTPUT to files
  : >"$work/github_output"
  NS="$NS" STRIPE_MODE="$1" STRIPE_TEST_API_KEY="${2-$fake_key}" STRIPE_TEST_WEBHOOK_SECRET="${3-$fake_secret}" \
    GITHUB_OUTPUT="$work/github_output" hack/stripe-secret.sh >"$work/stripe.log" 2>&1
}
value() { k get "$1" -o "jsonpath={.data.$2}" 2>/dev/null; }
silent() { # nothing of either value in what it wrote
  if grep -qF -e "${fake_key#rk_test_}" -e "${fake_secret#whsec_}" "$work/stripe.log" "$work/github_output"; then
    fail "$1 prints no value" "a value is in its output"
  else
    pass "$1 prints no value"
  fi
}

if stripe test "" ""; then fail "a missing secret stops it"; else
  is "a missing secret stops it, naming both" "2" "$(grep -c '^::error::STRIPE_MODE is test and the repository secret STRIPE_TEST_' "$work/stripe.log")"
fi
is "and nothing was made" "" "$(k get secret/stripe configmap/billing-mode --ignore-not-found -o name)"
if stripe test "rk_live_${fake_key#rk_test_}"; then fail "a live key in test mode stops it"; else pass "a live key in test mode stops it"; fi
silent "the refusal"
if stripe staging; then fail "an unknown mode stops it"; else pass "an unknown mode stops it"; fi

if stripe test; then pass "STRIPE_MODE=test with both secrets"; else fail "STRIPE_MODE=test with both secrets" "$(tail -3 "$work/stripe.log")"; fi
silent "making them"
is "secret/stripe has the key" "$fake_key" "$(value secret/stripe STRIPE_API_KEY | base64 -d)"
is "secret/stripe has the webhook secret" "$fake_secret" "$(value secret/stripe STRIPE_WEBHOOK_SECRET | base64 -d)"
is "secret/stripe has those two keys only" "STRIPE_API_KEY STRIPE_WEBHOOK_SECRET" "$(k get secret stripe -o json | jq -r '.data | keys | join(" ")')"
is "configmap/billing-mode says test" "test" "$(value configmap/billing-mode STRIPE_MODE)"
is "a change is reported, for the backend's restart" "stripe_changed=true" "$(cat "$work/github_output")"
stripe test
is "the same again reports no change" "" "$(cat "$work/github_output")"

# The backend's three variables are these objects' keys.
k rollout restart deploy/backend >/dev/null
k rollout status deploy/backend --timeout=180s >/dev/null
is "the backend's STRIPE_MODE is the ConfigMap's" "test" "$(k exec deploy/backend -- printenv STRIPE_MODE)"
is "the backend's STRIPE_API_KEY is the Secret's" "$fake_key" "$(k exec deploy/backend -- printenv STRIPE_API_KEY)"
is "the backend's STRIPE_WEBHOOK_SECRET is the Secret's" "$fake_secret" "$(k exec deploy/backend -- printenv STRIPE_WEBHOOK_SECRET)"
is "the operator has none of them" "" "$(k exec deploy/billing-operator -- sh -c 'env | grep -c STRIPE || true' | grep -v '^0$')"

if stripe ""; then pass "STRIPE_MODE empty"; else fail "STRIPE_MODE empty" "$(tail -3 "$work/stripe.log")"; fi
is "both are removed when STRIPE_MODE is empty" "" "$(k get secret/stripe configmap/billing-mode --ignore-not-found -o name)"
is "which is reported as a change" "stripe_changed=true" "$(cat "$work/github_output")"
stripe ""
is "and removing nothing is not" "" "$(cat "$work/github_output")"

# --- 5. the export, and its restore ------------------------------------------------
step "5. export"
is "the CronJob is suspended as deploy/gke has it with billing off" "true" \
  "$(kubectl kustomize deploy/gke | awk '/^kind: CronJob/ { cron = 1 } cron && $1 == "suspend:" { print $2; exit }')"
# deploy/gke's own file, with no bucket: the export is printed, not uploaded.
grep -q 'value: browserjs-sessions-billing-export$' deploy/gke/billing-export.yaml || fail "billing-export.yaml names no bucket to take out"
sed 's|value: browserjs-sessions-billing-export$|value: ""|' deploy/gke/billing-export.yaml | k apply -f - >/dev/null
may billing-export yes list accounts.browserjs.dev
may billing-export yes list grants.browserjs.dev
may billing-export yes list usageperiods.browserjs.dev
may billing-export no delete accounts.browserjs.dev
may billing-export no patch grants.browserjs.dev
may billing-export no get secrets
k create job --from=cronjob/billing-export export-test >/dev/null
if k wait --for=condition=Complete job/export-test --timeout=300s >/dev/null 2>&1; then
  pass "the export job completes (kubectl, curl and jq are in its image)"
else
  fail "the export job completes" "$(k describe job export-test | tail -15; k logs job/export-test --tail=20 2>&1)"
fi
k logs job/export-test >"$work/export.yaml"
is "the export has every object" "1 Account, 2 Grant, 1 UsagePeriod" \
  "$(kubectl create --dry-run=client --validate=false -o json -f "$work/export.yaml" 2>/dev/null |
    jq -rs '[.[] | if .kind == "List" then .items[] else . end] | group_by(.kind) | map("\(length) \(.[0].kind)") | join(", ")')"

# What a restore must bring back: names, labels, spec and status.
ledger() { # of the current context
  kubectl -n "$NS" get accounts.browserjs.dev,grants.browserjs.dev,usageperiods.browserjs.dev -o json |
    jq -S '[.items[] | {kind, name: .metadata.name, labels: .metadata.labels, spec, status}] | sort_by(.kind + .name)'
}
if [ -z "$RESTORE_CONTEXT" ]; then
  echo "SKIP  the restore into a second cluster (RESTORE_CONTEXT is not set)"
else
  here="$(kubectl config current-context)"
  ledger >"$work/first.json"
  kubectl config use-context "$RESTORE_CONTEXT" >/dev/null
  kubectl apply -f deploy/base/namespace.yaml -f deploy/base/crd-account.yaml -f deploy/base/crd-grant.yaml \
    -f deploy/base/crd-usageperiod.yaml >/dev/null
  kubectl wait --for=condition=Established --timeout=60s \
    crd/accounts.browserjs.dev crd/grants.browserjs.dev crd/usageperiods.browserjs.dev >/dev/null
  is "the second cluster starts empty" "[]" "$(ledger | jq -c .)"
  accepts "hack/billing-restore.sh applies the export to a second cluster" hack/billing-restore.sh "$work/export.yaml"
  ledger >"$work/second.json"
  if diff "$work/first.json" "$work/second.json" >"$work/diff"; then
    pass "the second cluster has the same Accounts, Grants and UsagePeriods: labels, spec and status"
  else
    fail "the second cluster has the same Accounts, Grants and UsagePeriods" "$(head -20 "$work/diff")"
  fi
  is "the ledger came with it (what was used of a grant)" "1600000" \
    "$(kubectl -n "$NS" get accounts.browserjs.dev "$ACCOUNT" -o jsonpath='{.status.meter.consumed[0].micros}')"
  is "and the record of the sign-up credit" "refused" \
    "$(kubectl -n "$NS" get accounts.browserjs.dev "$ACCOUNT" -o jsonpath='{.spec.signupCredit.state}')"
  accepts "restoring a second time" hack/billing-restore.sh "$work/export.yaml"
  ledger >"$work/third.json"
  if diff -q "$work/first.json" "$work/third.json" >/dev/null; then pass "changes nothing"; else fail "changes nothing"; fi
  kubectl config use-context "$here" >/dev/null
fi

is "the two stand-ins never restarted" "0 0" \
  "$(k get pods -l 'app in (backend,billing-operator)' -o jsonpath='{range .items[*]}{.status.containerStatuses[0].restartCount} {end}' | sed 's/ $//')"

printf '\n%d passed, %d failed\n' "$passed" "$failed"
note
note "$passed passed, $failed failed."
[ "$failed" -eq 0 ]
