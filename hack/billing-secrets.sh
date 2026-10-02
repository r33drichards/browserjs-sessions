#!/usr/bin/env bash
# Stripe's and Metronome's secrets in the cluster. The deploy workflow runs
# it; test/billing/run.sh runs it with made-up values.
#
#   hack/billing-secrets.sh --check  looks at the environment and the files
#                                    only: fails if a secret that is needed
#                                    is missing or is the other mode's
#   hack/billing-secrets.sh          the same check, then against the
#                                    current kubectl context:
#
#   Stripe, by STRIPE_MODE
#       test or live      Secret "stripe" (STRIPE_API_KEY, from that mode's
#                         secret) and ConfigMap "billing-mode" (STRIPE_MODE).
#                         The Secret "stripe-webhook" must be there already
#                         and be that mode's: the billing-apply workflow
#                         writes it (docs/billing-iac.md), never this
#       unset or empty    both are deleted if they are there; "stripe-webhook"
#                         is left alone
#   Metronome, by the stage of billing in deploy/gke (hack/billing-stage.sh)
#       meter or enforce  Secret "metronome" (METRONOME_API_TOKEN,
#                         METRONOME_WEBHOOK_SECRET): the sandbox's pair while
#                         STRIPE_MODE is unset or test, production's when it
#                         is live
#       off               it is deleted if it is there
#
# The environment: STRIPE_MODE; STRIPE_TEST_API_KEY, STRIPE_LIVE_API_KEY;
# METRONOME_SANDBOX_API_TOKEN, METRONOME_SANDBOX_WEBHOOK_SECRET,
# METRONOME_PRODUCTION_API_TOKEN, METRONOME_PRODUCTION_WEBHOOK_SECRET; NS
# (default browserjs-sessions); BILLING_STAGE in place of the files' stage
# (the test). A secret that is not needed is not looked at: with billing
# off and no mode, none is.
#
# The pods take every value with "optional: true" and read them at start
# only. When what is in the cluster changed, "stripe_changed=true" and
# "metronome_changed=true" go to $GITHUB_OUTPUT (and to stdout), for the
# caller to restart the backend (either) and the billing operator (the
# second).
#
# No value is ever printed, put in a command line or written to a file:
# they reach kubectl on stdin, and every message names the variable only.
set -euo pipefail
cd "$(dirname "$0")/.."

NS="${NS:-browserjs-sessions}"
mode="${STRIPE_MODE:-}"

fail() {
  # GitHub's annotation when run there; the same line otherwise.
  echo "::error::$*" >&2
  exit 1
}

upper=""
case "$mode" in
  "") ;;
  test) upper=TEST ;;
  live) upper=LIVE ;;
  *) fail "the repository variable STRIPE_MODE is \"$mode\": it is test, live or not set" ;;
esac

stage="${BILLING_STAGE:-$(hack/billing-stage.sh | sed -n 's|^billing-stage: deploy/gke is ||p')}"
case "$stage" in
  off | meter | enforce) ;;
  *) fail "the stage of billing in deploy/gke is \"$stage\": see hack/billing-stage.sh --check" ;;
esac
# Metronome's environment follows Stripe's mode.
environment=SANDBOX
[ "$mode" != live ] || environment=PRODUCTION

# Set, and one line: each is a line of an env file below.
missing=""
need() { # variable, why it is needed
  local name="$1"
  if [ -z "${!name:-}" ]; then
    echo "::error::$2 and the repository secret $name is not set (docs/billing-deployment.md)" >&2
    missing=1
    return
  fi
  case "${!name}" in
    *$'\n'* | *$'\r'*) fail "the repository secret $name has a line break in it" ;;
  esac
}

if [ -n "$mode" ]; then
  key_name="STRIPE_${upper}_API_KEY"
  need "$key_name" "STRIPE_MODE is $mode"
fi
if [ "$stage" != off ]; then
  token_name="METRONOME_${environment}_API_TOKEN"
  hook_name="METRONOME_${environment}_WEBHOOK_SECRET"
  why="billing is at $stage in deploy/gke (Metronome's $(tr '[:upper:]' '[:lower:]' <<<"$environment"))"
  need "$token_name" "$why"
  need "$hook_name" "$why"
fi
[ -z "$missing" ] || exit 1

if [ -n "$mode" ]; then
  # The backend refuses to start on a key of the other mode. Said here,
  # before anything is changed, and by the prefix alone.
  case "${!key_name}" in
    rk_"$mode"_* | sk_"$mode"_*) ;;
    *) fail "the repository secret $key_name is not a $mode-mode Stripe key (it does not start with rk_${mode}_ or sk_${mode}_)" ;;
  esac
fi

if [ "${1:-}" = --check ]; then
  echo "billing-secrets: STRIPE_MODE is ${mode:-not set}$([ -z "$mode" ] || echo "; that mode's Stripe key is set")"
  echo "billing-secrets: billing is $stage in deploy/gke$([ "$stage" = off ] || echo "; both Metronome secrets of $(tr '[:upper:]' '[:lower:]' <<<"$environment") are set")"
  exit 0
fi
[ $# -eq 0 ] || fail "usage: hack/billing-secrets.sh [--check]"

# What the named objects are now: their resourceVersions, or nothing.
versions() {
  kubectl -n "$NS" get "$@" --ignore-not-found \
    -o jsonpath='{range .items[*]}{.kind}={.metadata.resourceVersion} {end}{.kind}={.metadata.resourceVersion}' 2>/dev/null || true
}
secret_from_stdin() { # name; reads KEY=value lines
  kubectl -n "$NS" create secret generic "$1" --from-env-file=/dev/stdin --dry-run=client -o yaml |
    kubectl apply -f - >/dev/null
  echo "secret/$1 applied"
}
changed() { # what
  echo "$1_changed=true"
  [ -z "${GITHUB_OUTPUT:-}" ] || echo "$1_changed=true" >>"$GITHUB_OUTPUT"
}

# The webhook's signing secret is billing-apply's to write. Without it, or
# with the other mode's, a backend told STRIPE_MODE would not start: said
# here, before anything is changed.
if [ -n "$mode" ]; then
  if ! kubectl -n "$NS" get secret stripe-webhook >/dev/null 2>&1; then
    fail "STRIPE_MODE is $mode and the cluster has no secret/stripe-webhook: run the workflow \"billing apply\" with mode $mode first (docs/billing-iac.md)"
  fi
  have="$(kubectl -n "$NS" get secret stripe-webhook -o jsonpath='{.metadata.labels.browserjs\.dev/stripe-mode}')"
  if [ "$have" != "$mode" ]; then
    fail "STRIPE_MODE is $mode and secret/stripe-webhook is of mode \"$have\": run the workflow \"billing apply\" with mode $mode first (docs/billing-iac.md)"
  fi
  if [ -z "$(kubectl -n "$NS" get secret stripe-webhook -o jsonpath='{.data.STRIPE_WEBHOOK_SECRET}')" ]; then
    fail "secret/stripe-webhook has no STRIPE_WEBHOOK_SECRET: run the workflow \"billing apply\" with mode $mode again (docs/billing-iac.md)"
  fi
  echo "secret/stripe-webhook is there, of mode $mode"
fi

before="$(versions secret/stripe configmap/billing-mode)"
if [ -z "$mode" ]; then
  kubectl -n "$NS" delete secret/stripe configmap/billing-mode --ignore-not-found >/dev/null
  echo "STRIPE_MODE is not set: secret/stripe and configmap/billing-mode are absent"
else
  echo "STRIPE_API_KEY=${!key_name}" | secret_from_stdin stripe
  kubectl -n "$NS" create configmap billing-mode --from-literal=STRIPE_MODE="$mode" --dry-run=client -o yaml |
    kubectl apply -f - >/dev/null
  echo "configmap/billing-mode applied: STRIPE_MODE=$mode"
fi
[ "$before" = "$(versions secret/stripe configmap/billing-mode)" ] || changed stripe

before="$(versions secret/metronome)"
if [ "$stage" = off ]; then
  kubectl -n "$NS" delete secret/metronome --ignore-not-found >/dev/null
  echo "billing is off: secret/metronome is absent"
else
  {
    echo "METRONOME_API_TOKEN=${!token_name}"
    echo "METRONOME_WEBHOOK_SECRET=${!hook_name}"
  } | secret_from_stdin metronome
  echo "secret/metronome is Metronome's $(tr '[:upper:]' '[:lower:]' <<<"$environment")"
fi
[ "$before" = "$(versions secret/metronome)" ] || changed metronome
