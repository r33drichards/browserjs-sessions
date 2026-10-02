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
# The token is made here, for the local admin, by writing its record to the
# cluster as the backend would; it is deleted afterwards.
set -euo pipefail
. "$(dirname "$0")/../hack/lib.sh"

[ "${UP:-1}" = 0 ] || hack/local-up.sh

record="$(python3 - <<'PY'
import datetime, hashlib, json, secrets, string
owner = "admin@example.com"
token_id = "".join(secrets.choice("abcdefghijklmnopqrstuvwxyz234567") for _ in range(12))
secret = "".join(secrets.choice(string.ascii_letters + string.digits + "-_") for _ in range(43))
token = "bjs_%s_%s" % (token_id, secret)
expires = (datetime.datetime.now(datetime.timezone.utc) + datetime.timedelta(days=1)).strftime("%Y-%m-%dT%H:%M:%SZ")
print(token)
print(json.dumps({
    "apiVersion": "browserjs.dev/v1alpha1", "kind": "APIToken",
    "metadata": {"name": "tok-" + token_id, "labels": {"browserjs.dev/owner": hashlib.sha256(owner.encode()).hexdigest()[:32]}},
    "spec": {"id": token_id, "owner": owner, "name": "release-canary",
             "scopes": ["sessions:read", "sessions:write", "sessions:connect", "policies:read", "policies:write"],
             "expiresAt": expires, "sha256": hashlib.sha256(token.encode()).hexdigest()},
}))
PY
)"
token="$(head -1 <<<"$record")"
name="$(tail -1 <<<"$record" | jq -r .metadata.name)"
tail -1 <<<"$record" | kubectl -n "$NS" apply -f - >/dev/null
trap 'kubectl -n "$NS" delete apitokens.browserjs.dev "$name" --ignore-not-found >/dev/null' EXIT

# Policies bind only in the stage "enforcing" (hack/policy-stage.sh).
policies=0
[ "$(hack/policy-stage.sh | sed -n 's|^policy-stage: deploy/local is ||p')" != enforcing ] || policies=1

CANARY_API_TOKEN="$token" DOMAIN=localtest.me SITE_URL="" CA_FILE="$LOCAL_DIR/tls/ca.crt" \
  EXPECT_STATE_SAVED=0 EXPECT_POLICIES="$policies" \
  SESSION_HOOK="hack/release.sh verify-session" \
  EXPECT_IMAGES="$(awk '$1 == "image:" { n = split($2, p, "/"); sub(/:.*/, "", p[n]); printf "%s%s=%s", sep, p[n], $2; sep = "," }' deploy/local/blueprint.yaml)" \
  test/canary.py
