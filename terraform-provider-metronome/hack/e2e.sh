#!/usr/bin/env bash
# End to end, with a real tofu (or terraform) and no Metronome: builds the
# provider and the fake API, installs the provider with dev_overrides, and
# takes examples/computer-use through plan, apply, a rename in place, a new
# price, and destroy.
#
#   hack/e2e.sh [tofu|terraform]
set -euo pipefail

tf="${1:-tofu}"
port="${FAKE_PORT:-18090}"
here="$(cd "$(dirname "$0")/.." && pwd)"
work="$(mktemp -d)"
fake=""
trap '[ -n "$fake" ] && kill "$fake" 2>/dev/null; rm -rf "$work"' EXIT

mkdir "$work/bin" "$work/config"
(cd "$here" && go build -o "$work/bin/terraform-provider-metronome" . && go build -o "$work/bin/fakeapi" ./cmd/fakeapi)
cp "$here"/examples/computer-use/* "$work/config/"

cat > "$work/rc" <<RC
provider_installation {
  dev_overrides {
    "r33drichards/metronome" = "$work/bin"
  }
  direct {}
}
RC
export TF_CLI_CONFIG_FILE="$work/rc" TF_IN_AUTOMATION=1
export METRONOME_ENDPOINT="http://127.0.0.1:$port" METRONOME_BEARER_TOKEN="fake-e2e-token"

"$work/bin/fakeapi" -listen "127.0.0.1:$port" -token "$METRONOME_BEARER_TOKEN" > "$work/fakeapi.log" 2>&1 &
fake=$!
for _ in $(seq 50); do
  curl -fsS -o /dev/null -H "Authorization: Bearer $METRONOME_BEARER_TOKEN" "$METRONOME_ENDPOINT/v1/credit-types/list" && break
  sleep 0.1
done

cd "$work/config"
step() { printf '\n### %s\n' "$*"; }
# expect <exit code> <text the output must contain> <command...>
expect() {
  local want="$1" text="$2" code=0
  shift 2
  echo "\$ $*"
  "$@" > "$work/out" 2>&1 || code=$?
  # The last lines, without the dev_overrides warning every command prints.
  awk '/^Warning: Provider development overrides/ { skip = 1 }
       skip && /^(releases\.|and may error unexpectedly\.)$/ { skip = 0; next }
       !skip && NF' "$work/out" | tail -n 12
  if [ "$code" != "$want" ]; then
    echo "FAIL: exit $code, want $want" >&2
    cat "$work/out" >&2
    exit 1
  fi
  if ! grep -q -- "$text" "$work/out"; then
    echo "FAIL: the output does not contain: $text" >&2
    cat "$work/out" >&2
    exit 1
  fi
}

# With dev_overrides there is no init: the provider is used where it lies.
step "validate"
expect 0 "The configuration is valid" "$tf" validate -no-color

step "plan: two metrics, three products, a rate card, two rates, an alert, three custom field keys"
expect 0 "Plan: 12 to add, 0 to change, 0 to destroy." "$tf" plan -no-color

step "apply"
expect 0 "Apply complete! Resources: 12 added, 0 changed, 0 destroyed." "$tf" apply -auto-approve -no-color

step "plan again: nothing to change"
expect 0 "No changes." "$tf" plan -no-color -detailed-exitcode

step "rename a product: one update in place"
sed -i.bak 's/name               = "Disk"/name               = "Disk kept"/' main.tf
expect 2 "Plan: 0 to add, 1 to change, 0 to destroy." "$tf" plan -no-color -detailed-exitcode
expect 0 "Apply complete! Resources: 0 added, 1 changed, 0 destroyed." "$tf" apply -auto-approve -no-color

step "a new price, from a later moment: one rate replaced, nothing else"
# The awake rate is the first of the two: perl, whole file, first match.
perl -0pi -e 's/price     = 20 # cents an hour/price     = 25 # cents an hour/; s/starting_at  = "2026-10-01T00:00:00Z"/starting_at  = "2026-11-01T00:00:00Z"/' main.tf
expect 2 "Plan: 1 to add, 0 to change, 1 to destroy." "$tf" plan -no-color -detailed-exitcode
expect 0 "The rate stays in Metronome" "$tf" apply -auto-approve -no-color

step "a metric's definition is fixed: changing it is a replacement"
cp main.tf main.tf.orig
sed -i.bak 's/in_values = \["session.awake"\]/in_values = ["session.awake.v2"]/' main.tf
expect 2 "must be replaced" "$tf" plan -no-color -detailed-exitcode
mv main.tf.orig main.tf

step "a rate that does not start on the hour fails validation"
cp main.tf main.tf.orig
sed -i.bak 's/starting_at  = "2026-11-01T00:00:00Z"/starting_at  = "2026-11-01T00:30:00Z"/' main.tf
expect 1 "hour boundary" "$tf" validate -no-color
mv main.tf.orig main.tf

step "destroy: archives, except the custom field keys, which are deleted"
expect 0 "Destroy complete! Resources: 12 destroyed." "$tf" destroy -auto-approve -no-color

step "what the fake API was asked"
sed -E 's/^[0-9/]+ [0-9:]+ //; s#/[0-9a-f]{8}-[0-9a-f-]{27}#/{id}#' "$work/fakeapi.log" | grep -E '^(GET|POST|PUT|PATCH|DELETE) ' | sort | uniq -c

printf '\nend to end: ok (%s)\n' "$("$tf" version | head -n 1)"
