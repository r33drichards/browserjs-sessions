#!/usr/bin/env bash
# End to end, with a real tofu (or terraform) and no real API: builds the
# provider and the fake API, installs the provider with dev_overrides, and
# takes examples/session-policies through plan, apply, an in-place policy
# edit, a policy that does not validate, and destroy.
#
#   hack/e2e.sh [tofu|terraform]
set -euo pipefail

tf="${1:-tofu}"
port="${FAKE_PORT:-18080}"
here="$(cd "$(dirname "$0")/.." && pwd)"
work="$(mktemp -d)"
fake=""
trap '[ -n "$fake" ] && kill "$fake" 2>/dev/null; rm -rf "$work"' EXIT

mkdir "$work/bin" "$work/config"
(cd "$here" && go build -o "$work/bin/terraform-provider-browserjs" . && go build -o "$work/bin/fakeapi" ./cmd/fakeapi)
cp "$here"/examples/session-policies/* "$work/config/"

cat > "$work/rc" <<RC
provider_installation {
  dev_overrides {
    "r33drichards/browserjs" = "$work/bin"
  }
  direct {}
}
RC
export TF_CLI_CONFIG_FILE="$work/rc" TF_IN_AUTOMATION=1
export BROWSERJS_ENDPOINT="http://127.0.0.1:$port" BROWSERJS_TOKEN="bjs_fake_e2e_token"

"$work/bin/fakeapi" -listen "127.0.0.1:$port" > "$work/fakeapi.log" 2>&1 &
fake=$!
for _ in $(seq 50); do
  curl -fsS -o /dev/null -H "Authorization: Bearer $BROWSERJS_TOKEN" "$BROWSERJS_ENDPOINT/v1/sessions" && break
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

step "plan: three sessions, three policies"
expect 0 "Plan: 6 to add, 0 to change, 0 to destroy." "$tf" plan -no-color

step "apply"
expect 0 "Apply complete! Resources: 6 added, 0 changed, 0 destroyed." "$tf" apply -auto-approve -no-color

step "plan again: nothing to change"
expect 0 "No changes." "$tf" plan -no-color -detailed-exitcode

step "edit a policy: one update in place, no session touched"
sed -i.bak 's/"evaluate", "setContent"\]/"evaluate", "setContent", "navigate"]/' main.tf
expect 2 "Plan: 0 to add, 1 to change, 0 to destroy." "$tf" plan -no-color -detailed-exitcode
expect 0 "Apply complete! Resources: 0 added, 1 changed, 0 destroyed." "$tf" apply -auto-approve -no-color

step "a policy that does not validate fails the plan, with its line and column"
cp one-site.rego one-site.rego.orig
printf 'package browserjs.policy\n\nallow_tool_call if {\n' > one-site.rego
expect 1 "rego line 4, column 1" "$tf" plan -no-color
mv one-site.rego.orig one-site.rego

step "destroy is refused while a session has prevent_destroy"
expect 1 "lifecycle.prevent_destroy" "$tf" destroy -auto-approve -no-color

step "destroy, with prevent_destroy removed"
sed -i.bak '/lifecycle {/,/}/d' main.tf
expect 0 "Destroy complete! Resources: 6 destroyed." "$tf" destroy -auto-approve -no-color

step "what the fake API was asked"
sed -E 's/^[0-9/]+ [0-9:]+ //; s#s-[a-z2-7]{10}#{id}#' "$work/fakeapi.log" | grep -E '^(GET|POST|PUT|PATCH|DELETE) ' | sort | uniq -c

printf '\nend to end: ok (%s)\n' "$("$tf" version | head -n 1)"
