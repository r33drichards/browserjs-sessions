# Sourced by the sandbox checks. They are run by a person with their OWN
# sandbox key, never in CI, and they print no key.
#
#   export STRIPE_API_KEY=sk_test_...   # a sandbox secret key (or a restricted one)
#
# Needs curl and jq; check 1 also needs the Stripe CLI.
set -euo pipefail

case "${STRIPE_API_KEY:-}" in
  sk_test_*|rk_test_*) ;;
  "") echo "STRIPE_API_KEY is not set: export a SANDBOX key (sk_test_... or rk_test_...)" >&2; exit 2 ;;
  *)  echo "STRIPE_API_KEY is not a sandbox key (sk_test_ or rk_test_): these checks never run against live mode" >&2; exit 2 ;;
esac
for tool in curl jq; do
  command -v "$tool" >/dev/null || { echo "$tool is needed" >&2; exit 2; }
done

API=https://api.stripe.com
STARTED=$(date +%s)

# api METHOD PATH [curl args...]: one call; the body, as JSON, on stdout.
# The key goes to curl by a config on stdin, so it is in no argument list.
api() {
  local method=$1 path=$2
  shift 2
  curl -sS -X "$method" "$API$path" -K <(printf 'user = "%s:"\n' "$STRIPE_API_KEY") "$@"
}

say() { printf '\n== %s\n' "$*"; }

# events [TYPE_PREFIX]: the events since the script started, oldest first.
events() {
  api GET "/v1/events" -G -d "created[gte]=$STARTED" -d limit=100 |
    jq -r --arg p "${1:-}" '.data | reverse | .[] | select(.type | startswith($p)) |
      "\(.created) \(.type) \(.data.object.id) customer=\(.data.object.customer // "null")"'
}

pause() { read -r -p "$* [Enter] " _; }
