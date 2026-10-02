#!/usr/bin/env bash
# Sessions made before the deployment moved from browserjs.com to
# computeruse.site keep the old host in their Sandbox: mcp-js makes one-time
# upload URLs from MCP_V8_PUBLIC_URL, which is written when a session is
# created and which a deploy does not change (docs/domain-switch.md). This
# rewrites that one value, and nothing else, in the Sandboxes that have it.
#
#   hack/session-public-url.sh s-abc12        shows what would change in one session
#   hack/session-public-url.sh all            ... in every session
#   hack/session-public-url.sh --apply all    changes them
#
# Only sessions stopped by their user are changed. Whether the Sandbox
# controller replaces the pod of a running one when its template changes is
# not known, and a running session must not be restarted by surprise. One
# that is asleep (the idle sweep) has a snapshot of a pod with the old
# template, and would wake from it with the old value anyway. A stop in the
# UI deletes the snapshots, so: stop the session, run this, resume it.
#
# Uses the current kubectl context. Run by .github/workflows/session-public-url.yml.
set -euo pipefail

NS="${NS:-browserjs-sessions}"
OLD_HOST="${OLD_HOST:-sessions.browserjs.com}"
NEW_HOST="${NEW_HOST:-sessions.computeruse.site}"
VARIABLE=MCP_V8_PUBLIC_URL
RESOURCE=sandboxes.agents.x-k8s.io

apply=""
if [ "${1:-}" = --apply ]; then
  apply=1
  shift
fi
target="${1:-}"
if [ "$#" -ne 1 ] || { [ "$target" != all ] && [[ ! "$target" =~ ^s-[a-z0-9]{5,10}$ ]]; }; then
  echo "usage: $0 [--apply] <session ID | all>" >&2
  exit 2
fi

if [ "$target" = all ]; then
  # Sessions only: a Sandbox of the warm pool that nobody has taken has no
  # owner, and the pool replaces it when its template changes.
  sandboxes="$(kubectl -n "$NS" get "$RESOURCE" -l browserjs.dev/owner -o json)"
else
  sandboxes="$(kubectl -n "$NS" get "$RESOURCE" "$target" -o json | jq '{items: [.]}')"
fi

# Per Sandbox with the old host in the variable: its name, its mode, who
# stopped it, and a JSON patch that replaces exactly those values. Each
# replace is preceded by a test of the value it was computed from, so a
# Sandbox that changed in between is refused, not overwritten.
plan="$(jq -c --arg variable "$VARIABLE" --arg old "//$OLD_HOST/" --arg new "//$NEW_HOST/" '
  .items[]
  | . as $sandbox
  | [ ($sandbox.spec.podTemplate.spec.containers // []) | to_entries[]
      | .key as $c
      | (.value.env // []) | to_entries[]
      | select(.value.name == $variable and ((.value.value // "") | contains($old)))
      | "/spec/podTemplate/spec/containers/\($c)/env/\(.key)/value" as $path
      | {op: "test", path: $path, value: .value.value},
        {op: "replace", path: $path, value: (.value.value | split($old) | join($new))}
    ] as $patch
  | select(($patch | length) > 0)
  | {
      name: $sandbox.metadata.name,
      mode: ($sandbox.spec.operatingMode // "Running"),
      stoppedBy: ($sandbox.metadata.annotations["browserjs.dev/stopped-by"] // ""),
      patch: $patch
    }
' <<<"$sandboxes")"

if [ -z "$plan" ]; then
  echo "No session has $OLD_HOST in $VARIABLE: nothing to do."
  exit 0
fi

changed=0
skipped=0
while IFS= read -r entry; do
  name="$(jq -r .name <<<"$entry")"
  mode="$(jq -r .mode <<<"$entry")"
  stopped_by="$(jq -r .stoppedBy <<<"$entry")"
  from="$(jq -r '.patch[0].value' <<<"$entry")"
  to="$(jq -r '.patch[1].value' <<<"$entry")"
  if [ "$mode" != Suspended ] || [ "$stopped_by" != user ]; then
    skipped=$((skipped + 1))
    state=running
    [ "$mode" != Suspended ] || state=asleep
    echo "SKIP    $name is $state, not stopped: stop it in the UI first ($from)"
    continue
  fi
  if [ -z "$apply" ]; then
    echo "WOULD   $name: $from -> $to"
    continue
  fi
  kubectl -n "$NS" patch "$RESOURCE" "$name" --type=json -p "$(jq -c .patch <<<"$entry")" >/dev/null
  changed=$((changed + 1))
  echo "CHANGED $name: $from -> $to"
done <<<"$plan"

echo
if [ -z "$apply" ]; then
  echo "Nothing was changed (no --apply). $skipped session(s) not stopped would be skipped."
else
  echo "$changed changed, $skipped skipped. Resume a changed session in the UI: it starts with the new value."
fi
