#!/usr/bin/env bash
# The stage of session policies in an overlay, shown or changed. The stages,
# what each needs first and what each does to existing sessions:
# docs/policy-deployment.md.
#
#   off        installed, nothing runs: OPA and the operator have no pods,
#              the backend is not told about the operator
#   serving    OPA and the operator run, with an empty bundle. The backend
#              is still not told, and no session pod asks OPA: nothing a
#              user or a session can notice has changed
#   enforcing  the backend keeps a policy for every new session, and new
#              session pods ask OPA on every browser call; a session with
#              no SessionPolicy is denied everything
#
#   hack/policy-stage.sh                  shows the stage of gke and of local
#   hack/policy-stage.sh --check          the same, and fails if the files of
#                                         an overlay disagree. The deploy
#                                         workflow runs this first.
#   hack/policy-stage.sh gke serving      edits the files; review, commit and
#   hack/policy-stage.sh local enforcing  deploy as any other change
#   hack/policy-stage.sh --env warm|cold  prints the variable an enforcing
#                                         pod template carries
#
# What it edits: the "- path: patch-policy-off.yaml" and "- path:
# patch-policy-backend-off.yaml" lines of the overlay's kustomization.yaml
# (commented out or not), and the MCP_V8_POLICIES_JSON variable of mcp-js in
# the overlay's pod templates, directly below MCP_V8_PUBLIC_URL
# (docs/contracts/policy/deploy.md).
set -euo pipefail
cd "$(dirname "$0")/.."

die() {
  echo "policy-stage: $*" >&2
  exit 1
}

# The pod templates of an overlay. A warm template takes the session's ID
# from the pod's own name; a blueprint has it rendered in by the backend.
templates() { # overlay
  case "$1" in
    gke) echo deploy/gke/blueprint.yaml deploy/gke/warmpool.yaml ;;
    # deploy/base's blueprint is what deploy/local's replaces: kept in step.
    local) echo deploy/local/blueprint.yaml deploy/base/blueprint.yaml ;;
    *) die "unknown overlay: $1 (gke or local)" ;;
  esac
}
# shellcheck disable=SC2016 # $(SESSION_ID) is for the kubelet, not this shell
session_id() { # file
  case "$1" in
    */warmpool.yaml) echo '$(SESSION_ID)' ;;
    *) echo '{{ .ID }}' ;;
  esac
}

# The variable, as lines without indentation. mode "all": the image's own
# file policy and OPA must both allow.
env_block() { # session id
  cat <<BLOCK
# Session policy: every browser call is also put to the shared OPA, at
# this session's own path. Written by hack/policy-stage.sh.
- name: MCP_V8_POLICIES_JSON
  value: >-
    {"mcp_tools":{"mode":"all","policies":[
    {"url":"file:///etc/mcp/mcp_tools.rego"},
    {"url":"http://opa.browserjs-sessions.svc:8181","policy_path":"browserjs/decision/$1/mcp_tools"}]},
    "filesystem":{"policies":[{"url":"file:///etc/mcp/filesystem.rego"}]}}
BLOCK
}

has_env() { grep -q -- '- name: MCP_V8_POLICIES_JSON' "$1"; }

# Without the variable (and the comment above it), whether or not it is there.
without_env() { # file
  awk '
    function indent(s) { match(s, /^ */); return RLENGTH }
    { lines[NR] = $0 }
    END {
      for (i = 1; i <= NR; i++) {
        if (lines[i] ~ /^ *- name: MCP_V8_POLICIES_JSON$/) {
          at = indent(lines[i])
          # The comment lines directly above belong to it.
          while (n > 0 && out[n] ~ /^ *#/ && indent(out[n]) == at) n--
          # Its own lines are the ones indented further.
          for (i++; i <= NR && lines[i] ~ /[^ ]/ && indent(lines[i]) > at; i++);
          i--
          continue
        }
        out[++n] = lines[i]
      }
      for (i = 1; i <= n; i++) print out[i]
    }' "$1"
}

set_env() { # file, on|off
  local file="$1" tmp="$1.tmp" block
  without_env "$file" >"$tmp"
  if [ "$2" = on ]; then
    grep -q -- '- name: MCP_V8_PUBLIC_URL$' "$tmp" || die "$file: no MCP_V8_PUBLIC_URL to put the variable below"
    block="$(env_block "$(session_id "$file")")"
    # After MCP_V8_PUBLIC_URL and whatever lines its value takes.
    BLOCK="$block" awk '
      function indent(s) { match(s, /^ */); return RLENGTH }
      function emit(   k, parts, pad) {
        pad = sprintf("%" at "s", "")
        k = split(ENVIRON["BLOCK"], parts, "\n")
        for (j = 1; j <= k; j++) print pad parts[j]
        pending = 0
      }
      pending && !(/[^ ]/ && indent($0) > at) { emit() }
      { print }
      /^ *- name: MCP_V8_PUBLIC_URL$/ { at = indent($0); pending = 1 }
      END { if (pending) emit() }' "$tmp" >"$tmp.2"
    mv "$tmp.2" "$tmp"
  fi
  if cmp -s "$tmp" "$file"; then rm "$tmp"; else
    mv "$tmp" "$file"
    echo "$file: MCP_V8_POLICIES_JSON $2"
  fi
}

# A "- path: <file>" line of the overlay's kustomization.yaml.
listed() { # overlay, file
  grep -qE "^ *- path: $2\$" "deploy/$1/kustomization.yaml"
}
list() { # overlay, file, on|off
  local file="deploy/$1/kustomization.yaml" line="- path: $2"
  grep -qE "^ *(# )?$line\$" "$file" || die "$file: no line \"$line\" (commented out or not)"
  if [ "$3" = on ]; then
    sed -E "s|^( *)# ($line)\$|\1\2|" "$file" >"$file.tmp"
  else
    sed -E "s|^( *)($line)\$|\1# \2|" "$file" >"$file.tmp"
  fi
  if cmp -s "$file.tmp" "$file"; then rm "$file.tmp"; else
    mv "$file.tmp" "$file"
    echo "$file: $2 $([ "$3" = on ] && echo listed || echo "commented out")"
  fi
}
workloads_off=patch-policy-off.yaml
backend_off=patch-policy-backend-off.yaml

# Prints the overlay's stage, or says what disagrees and returns 1.
stage() { # overlay
  local with=() without=() file
  for file in $(templates "$1"); do
    if has_env "$file"; then with+=("$file"); else without+=("$file"); fi
  done
  if [ ${#with[@]} -gt 0 ] && [ ${#without[@]} -gt 0 ]; then
    echo "policy-stage: $1: ${with[*]} asks OPA and ${without[*]} does not" >&2
    return 1
  fi
  if listed "$1" "$workloads_off"; then
    if [ ${#with[@]} -gt 0 ]; then
      echo "policy-stage: $1: the pod templates ask OPA, but $workloads_off leaves OPA without pods: every browser call of a new session would be denied" >&2
      return 1
    fi
    if ! listed "$1" "$backend_off"; then
      echo "policy-stage: $1: the backend is told about the operator, but $workloads_off leaves the operator without pods: no session could be created" >&2
      return 1
    fi
    echo off
  elif listed "$1" "$backend_off"; then
    if [ ${#with[@]} -gt 0 ]; then
      echo "policy-stage: $1: the pod templates ask OPA, but $backend_off keeps the backend from making policies: every browser call of a new session would be denied" >&2
      return 1
    fi
    echo serving
  elif [ ${#with[@]} -gt 0 ]; then
    echo enforcing
  else
    echo "policy-stage: $1: the backend keeps policies, but the pod templates do not ask OPA: it would refuse every warm pod and start each session cold" >&2
    return 1
  fi
}

case "${1:-}" in
  "" | --check)
    failed=""
    for overlay in gke local; do
      if now="$(stage "$overlay")"; then
        echo "policy-stage: deploy/$overlay is $now"
      else
        failed=1
      fi
    done
    [ -z "$failed" ] || exit 1
    ;;
  --env)
    # shellcheck disable=SC2016
    case "${2:-}" in
      warm) env_block '$(SESSION_ID)' ;;
      cold) env_block '{{ .ID }}' ;;
      *) die "--env warm or --env cold" ;;
    esac
    ;;
  gke | local)
    overlay="$1"
    case "${2:-}" in
      off)
        for file in $(templates "$overlay"); do set_env "$file" off; done
        list "$overlay" "$backend_off" on
        list "$overlay" "$workloads_off" on
        ;;
      serving)
        for file in $(templates "$overlay"); do set_env "$file" off; done
        list "$overlay" "$backend_off" on
        list "$overlay" "$workloads_off" off
        ;;
      enforcing)
        list "$overlay" "$workloads_off" off
        list "$overlay" "$backend_off" off
        for file in $(templates "$overlay"); do set_env "$file" on; done
        ;;
      *) die "the stage is one of: off, serving, enforcing" ;;
    esac
    echo "policy-stage: deploy/$overlay is $(stage "$overlay")"
    ;;
  *) die "usage: hack/policy-stage.sh [--check | --env warm|cold | gke|local off|serving|enforcing]" ;;
esac
