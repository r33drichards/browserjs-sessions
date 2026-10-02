#!/usr/bin/env bash
# The stage of metering and billing in an overlay, shown or changed. The
# stages, what each needs first and what each does to existing sessions:
# docs/billing-deployment.md.
#
#   off      installed, nothing runs: the Account resource, the Roles, the
#            catalogue and the webhook routes are there, the billing operator
#            has no pods, and the backend has no BILLING: it is exactly as
#            it was before the feature
#   meter    the operator has its pod and sends usage to Metronome; the
#            backend shows it (BILLING=meter on both). Nothing is refused
#            and no session is stopped. Needs the Metronome secrets
#   enforce  BILLING=enforce on both: no card, no session, and an account at
#            zero has its sessions drained and put to sleep. On GKE the
#            daily export of the Accounts starts. Needs STRIPE_MODE
#
#   hack/billing-stage.sh                the stage of gke and of local
#   hack/billing-stage.sh --check        the same, and fails if the files of
#                                        an overlay disagree, or if the stage
#                                        of gke cannot go with the STRIPE_MODE
#                                        in the environment. The deploy
#                                        workflow runs this first.
#   hack/billing-stage.sh gke meter      edits the files; review, commit and
#   hack/billing-stage.sh local enforce  deploy as any other change
#
# What it edits: the "- path: patch-billing-meter.yaml" and "- path:
# patch-billing-enforce.yaml" lines of the overlay's kustomization.yaml
# (commented out or not). BILLING is set by those two patches and nowhere
# else. Payments are not a stage of the files: they are the repository
# variable STRIPE_MODE (hack/billing-secrets.sh).
set -euo pipefail
cd "$(dirname "$0")/.."

die() {
  echo "billing-stage: $*" >&2
  exit 1
}

meter=patch-billing-meter.yaml
enforce=patch-billing-enforce.yaml

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

# Files that must leave BILLING to the two patches: the base, and the
# overlay's own backend patch.
sets_billing() { grep -qE '^ *- name: BILLING$' "$1"; }

# Prints the overlay's stage, or says what disagrees and returns 1.
stage() { # overlay
  local file
  for file in deploy/base/backend.yaml deploy/base/billing-operator.yaml "deploy/$1/patch-backend.yaml"; do
    if sets_billing "$file"; then
      echo "billing-stage: $1: $file sets BILLING: only $meter and $enforce may, so that the backend and the operator cannot disagree" >&2
      return 1
    fi
  done
  for file in "$meter" "$enforce"; do
    if [ ! -f "deploy/$1/$file" ]; then
      echo "billing-stage: $1: deploy/$1/$file is missing" >&2
      return 1
    fi
  done
  if listed "$1" "$enforce"; then
    if ! listed "$1" "$meter"; then
      echo "billing-stage: $1: $enforce is listed without $meter: the backend would enforce on usage the operator, with no pods, never sends" >&2
      return 1
    fi
    # The later patch wins: enforce has to come after meter.
    if [ "$(grep -nE "^ *- path: $meter\$" "deploy/$1/kustomization.yaml" | cut -d: -f1)" -gt \
      "$(grep -nE "^ *- path: $enforce\$" "deploy/$1/kustomization.yaml" | cut -d: -f1)" ]; then
      echo "billing-stage: $1: $enforce is listed before $meter, which would undo it" >&2
      return 1
    fi
    echo enforce
  elif listed "$1" "$meter"; then
    echo meter
  else
    echo off
  fi
}

# What the stage of deploy/gke needs of STRIPE_MODE (the backend refuses to
# start otherwise, and it is the only backend there is).
stripe_agrees() { # stage of gke
  local mode="${STRIPE_MODE-}"
  case "$mode" in
    "" | test | live) ;;
    *)
      echo "billing-stage: STRIPE_MODE is \"$mode\": it is test, live or not set" >&2
      return 1
      ;;
  esac
  if [ -n "$mode" ] && [ "$1" = off ]; then
    echo "billing-stage: STRIPE_MODE is $mode and billing is off in deploy/gke: the backend would not start. Unset the variable, or: hack/billing-stage.sh gke meter" >&2
    return 1
  fi
  if [ -z "$mode" ] && [ "$1" = enforce ] && [ -n "${STRIPE_MODE+set}" ]; then
    echo "billing-stage: deploy/gke enforces billing and STRIPE_MODE is not set: the backend would not start. Set the variable to test or live, or: hack/billing-stage.sh gke meter" >&2
    return 1
  fi
}

case "${1:-}" in
  "" | --check)
    failed=""
    for overlay in gke local; do
      if now="$(stage "$overlay")"; then
        echo "billing-stage: deploy/$overlay is $now"
        [ "$overlay" != gke ] || [ "${1:-}" != --check ] || stripe_agrees "$now" || failed=1
      else
        failed=1
      fi
    done
    [ -z "$failed" ] || exit 1
    ;;
  gke | local)
    overlay="$1"
    case "${2:-}" in
      off)
        list "$overlay" "$enforce" off
        list "$overlay" "$meter" off
        ;;
      meter)
        list "$overlay" "$enforce" off
        list "$overlay" "$meter" on
        ;;
      enforce)
        list "$overlay" "$meter" on
        list "$overlay" "$enforce" on
        ;;
      *) die "the stage is one of: off, meter, enforce" ;;
    esac
    echo "billing-stage: deploy/$overlay is $(stage "$overlay")"
    ;;
  *) die "usage: hack/billing-stage.sh [--check | gke|local off|meter|enforce]" ;;
esac
