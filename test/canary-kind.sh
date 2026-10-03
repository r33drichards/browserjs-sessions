#!/usr/bin/env bash
# The release canary (test/canary.py) against the local kind cluster: the
# whole system built from this checkout (hack/local-up.sh), reached the way
# production is, through Pomerium on the API host with an API token.
#
#   test/canary-kind.sh           brings the cluster up (or up to date) first
#   UP=0 test/canary-kind.sh      against the cluster as it is
#
# It is what a pull request is held to before it merges
# (.github/workflows/canary-kind.yml). What kind cannot show, and production's
# canary does: gVisor, Pod Snapshots (sleep saves nothing here, and wake
# starts the session fresh), the warm pool, the canary create option (the
# local blueprint names its images by tag), and the images as the registry
# has them (these are built here, from the same source). docs/releases.md.
#
# The token is minted here by hack/release.sh mint-token, as the deploy
# workflow mints one for each release, and revoked afterwards; the end checks
# that it is refused once revoked.
set -euo pipefail
. "$(dirname "$0")/../hack/lib.sh"

[ "${UP:-1}" = 0 ] || hack/local-up.sh

# The token as the deploy workflow makes it for a release: minted here,
# never printed, revoked at the end.
tokenfile="$(mktemp)"
name="$(hack/release.sh mint-token "$tokenfile")"
token="$(cat "$tokenfile")"
rm -f "$tokenfile"
trap 'hack/release.sh revoke-token "$name" >/dev/null' EXIT

# Policies bind only in the stage "enforcing" (hack/policy-stage.sh).
policies=0
[ "$(hack/policy-stage.sh | sed -n 's|^policy-stage: deploy/local is ||p')" != enforcing ] || policies=1

images="$(awk '$1 == "image:" { n = split($2, p, "/"); sub(/:.*/, "", p[n]); printf "%s%s=%s", sep, p[n], $2; sep = "," }' deploy/local/blueprint.yaml)"
failed=""

echo "=== through the edge: Pomerium, the API host"
CANARY_API_TOKEN="$token" DOMAIN=localtest.me SITE_URL="" CA_FILE="$LOCAL_DIR/tls/ca.crt" \
  EXPECT_STATE_SAVED=0 EXPECT_POLICIES="$policies" \
  SESSION_HOOK="hack/release.sh verify-session" EXPECT_IMAGES="$images" \
  test/canary.py || failed=1

# As the backend's Rollout runs it on GKE against a backend on standby
# (deploy/gke/rollouts.yaml): straight at the backend, saying which host it
# is being asked as.
echo
echo "=== straight at the backend, as a rollout's check"
port=$((20000 + RANDOM % 20000))
kubectl -n "$NS" port-forward service/backend "$port:80" >/dev/null 2>&1 &
forward=$!
trap 'kill "$forward" 2>/dev/null; hack/release.sh revoke-token "$name" >/dev/null' EXIT
for _ in $(seq 1 30); do
  curl -s -o /dev/null --max-time 2 "http://127.0.0.1:$port/healthz" && break
  sleep 0.5
done
CANARY_API_TOKEN="$token" API_URL="http://127.0.0.1:$port" API_HOST=api.localtest.me APP_URL="" SITE_URL="" \
  EXPECT_STATE_SAVED=0 EXPECT_POLICIES="$policies" \
  test/canary.py || failed=1

# Revoked, it is refused at once, and so is what was made from it.
echo
echo "=== revoked"
hack/release.sh revoke-token "$name"
status="$(curl -s -o /dev/null -w '%{http_code}' -H "Host: api.localtest.me" -H "Authorization: Bearer $token" "http://127.0.0.1:$port/v1/me")"
if [ "$status" = 401 ]; then echo "PASS  the revoked token is refused (401)"; else echo "FAIL  the revoked token answered $status, not 401"; failed=1; fi

[ -z "$failed" ]
