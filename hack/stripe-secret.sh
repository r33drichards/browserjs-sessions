#!/usr/bin/env bash
# Stripe's mode and keys in the cluster, written together or not at all. The
# deploy workflow runs it; test/billing/run.sh runs it with made-up values.
#
#   hack/stripe-secret.sh --check   looks at the environment only: fails if
#                                   STRIPE_MODE is set and a secret of that
#                                   mode is missing or is the other mode's
#   hack/stripe-secret.sh           the same check, then against the current
#                                   kubectl context:
#       STRIPE_MODE=test or live    Secret "stripe" (STRIPE_API_KEY,
#                                   STRIPE_WEBHOOK_SECRET, from that mode's
#                                   secrets) and ConfigMap "billing-mode"
#                                   (STRIPE_MODE) are applied
#       STRIPE_MODE unset or empty  both are deleted if they are there
#
# The environment: STRIPE_MODE; STRIPE_TEST_API_KEY and
# STRIPE_TEST_WEBHOOK_SECRET; STRIPE_LIVE_API_KEY and
# STRIPE_LIVE_WEBHOOK_SECRET; NS (default browserjs-sessions). With no mode
# none of the others is needed, or looked at.
#
# The backend takes all three with "optional: true" and reads them at start
# only. When what is in the cluster changed, "stripe_changed=true" goes to
# $GITHUB_OUTPUT (and to stdout), for the caller to restart the backend.
#
# No value is ever printed, put in a command line or written to a file:
# they reach kubectl on stdin, and every message names the variable only.
set -euo pipefail

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

if [ -n "$mode" ]; then
  missing=""
  for name in "STRIPE_${upper}_API_KEY" "STRIPE_${upper}_WEBHOOK_SECRET"; do
    if [ -z "${!name:-}" ]; then
      echo "::error::STRIPE_MODE is $mode and the repository secret $name is not set (docs/billing-deployment.md)" >&2
      missing=1
    fi
  done
  [ -z "$missing" ] || exit 1
  key_name="STRIPE_${upper}_API_KEY"
  secret_name="STRIPE_${upper}_WEBHOOK_SECRET"
  # The backend refuses to start on a key of the other mode. Said here,
  # before anything is changed, and by the prefix alone.
  case "${!key_name}" in
    rk_"$mode"_* | sk_"$mode"_*) ;;
    *) fail "the repository secret $key_name is not a $mode-mode Stripe key (it does not start with rk_${mode}_ or sk_${mode}_)" ;;
  esac
  case "${!secret_name}" in
    whsec_*) ;;
    *) fail "the repository secret $secret_name is not a webhook signing secret (it does not start with whsec_)" ;;
  esac
  # A line each in the env file below: neither may bring a second line.
  for name in "$key_name" "$secret_name"; do
    case "${!name}" in
      *$'\n'* | *$'\r'*) fail "the repository secret $name has a line break in it" ;;
    esac
  done
fi

if [ "${1:-}" = --check ]; then
  echo "stripe: STRIPE_MODE is ${mode:-not set}$([ -z "$mode" ] || echo "; both secrets of that mode are set")"
  exit 0
fi
[ $# -eq 0 ] || fail "usage: hack/stripe-secret.sh [--check]"

# What the two objects are now: their resourceVersions, or nothing.
versions() {
  kubectl -n "$NS" get secret/stripe configmap/billing-mode --ignore-not-found \
    -o jsonpath='{range .items[*]}{.kind}={.metadata.resourceVersion} {end}' 2>/dev/null || true
}
before="$(versions)"

if [ -z "$mode" ]; then
  kubectl -n "$NS" delete secret/stripe configmap/billing-mode --ignore-not-found >/dev/null
  echo "STRIPE_MODE is not set: secret/stripe and configmap/billing-mode are absent"
else
  {
    echo "STRIPE_API_KEY=${!key_name}"
    echo "STRIPE_WEBHOOK_SECRET=${!secret_name}"
  } | kubectl -n "$NS" create secret generic stripe --from-env-file=/dev/stdin --dry-run=client -o yaml |
    kubectl apply -f - >/dev/null
  echo "secret/stripe applied"
  kubectl -n "$NS" create configmap billing-mode --from-literal=STRIPE_MODE="$mode" --dry-run=client -o yaml |
    kubectl apply -f - >/dev/null
  echo "configmap/billing-mode applied: STRIPE_MODE=$mode"
fi

if [ "$before" != "$(versions)" ]; then
  echo "stripe_changed=true"
  [ -z "${GITHUB_OUTPUT:-}" ] || echo "stripe_changed=true" >>"$GITHUB_OUTPUT"
fi
