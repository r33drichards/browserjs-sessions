#!/usr/bin/env bash
# Says whether the keys of a mode are there and are that mode's, without
# ever printing one. Used by the two billing workflows.
#
#   STRIPE_API_KEY=... METRONOME_BEARER_TOKEN=... infra/billing/keys.sh <test|live>
#
# Exit 0: ready. Exit 3: a key is missing (the message says which secret).
# Exit 1: a key is of the other mode, or the mode is unknown.
set -euo pipefail

mode="${1:-}"
here="$(cd "$(dirname "$0")" && pwd)"

case "$mode" in
  test) stripe_secret=STRIPE_TEST_SETUP_KEY metronome_secret=METRONOME_SANDBOX_API_TOKEN prefix=test ;;
  live) stripe_secret=STRIPE_LIVE_SETUP_KEY metronome_secret=METRONOME_PRODUCTION_API_TOKEN prefix=live ;;
  *)
    echo "mode must be test or live" >&2
    exit 1
    ;;
esac

missing=""
if [ -z "${STRIPE_API_KEY:-}" ]; then
  echo "the repository secret $stripe_secret is not set"
  missing=1
fi

# Metronome's token is needed unless the mode's variables turn Metronome off.
if grep -Eq '^[[:space:]]*metronome_enabled[[:space:]]*=[[:space:]]*false' "$here/$mode.tfvars"; then
  echo "Metronome is not managed in mode $mode ($mode.tfvars)"
elif [ -z "${METRONOME_BEARER_TOKEN:-}" ]; then
  echo "the repository secret $metronome_secret is not set"
  missing=1
fi
[ -z "$missing" ] || exit 3

# A key says which half of the Stripe account it is for. A restricted key
# (rk_) is what is asked for; a secret key (sk_) works and is more than is
# needed.
case "$STRIPE_API_KEY" in
  "rk_${prefix}_"*) ;;
  "sk_${prefix}_"*) echo "::warning::$stripe_secret is an unrestricted secret key; a restricted key is enough (docs/billing-iac.md)" ;;
  rk_* | sk_*)
    echo "::error::$stripe_secret is not a key of mode $mode"
    exit 1
    ;;
  *)
    echo "::error::$stripe_secret is not a Stripe secret or restricted key"
    exit 1
    ;;
esac
echo "the keys of mode $mode are set"
