#!/usr/bin/env bash
# Makes or deletes an API token directly in the cluster, for a machine that
# has the cluster's credentials and no browser: CI's live tests and canary.
# It does what the backend does on POST /api/tokens (backend/internal/tokens):
# an APIToken named tok-<id> holding only the SHA-256 of the token.
#
#   hack/mint-token.sh create --owner <email> --scopes "<scope> ..." [--ttl <minutes>] [--session <id>] [--name <label>]
#   hack/mint-token.sh delete <id>
#
# create prints two lines on stdout, `id=<id>` and `token=bjs_<id>_<secret>`,
# and nothing else. The token exists only in that output: read it into a
# variable, never into a file. Under GitHub Actions, mask it the moment it is
# read (`echo "::add-mask::$token"`): a mask written from inside a command
# substitution is captured with the output and never reaches the runner. --ttl is in
# minutes, 60 by default; the backend refuses an expired token itself.
#
# delete removes tok-<id>, and succeeds when it is already gone. A deleted
# token, and every access token made from it, is refused at the next request.
#
# kubectl's current context and the namespace browserjs-sessions (NS to
# change it). The caller needs create and delete on apitokens.browserjs.dev.
set -euo pipefail

NS="${NS:-browserjs-sessions}"
SCOPES_ALLOWED="sessions:read sessions:write sessions:connect policies:read policies:write"

die() { echo "mint-token: $*" >&2; exit 2; }

create() {
  local owner="" scopes="" ttl=60 session="" name="ci"
  while [ $# -gt 0 ]; do
    case "$1" in
      --owner) owner="$2"; shift 2 ;;
      --scopes) scopes="$2"; shift 2 ;;
      --ttl) ttl="$2"; shift 2 ;;
      --session) session="$2"; shift 2 ;;
      --name) name="$2"; shift 2 ;;
      *) die "unknown argument $1" ;;
    esac
  done
  [ -n "$owner" ] || die "--owner is required"
  [ -n "$scopes" ] || die "--scopes is required"
  [[ "$ttl" =~ ^[0-9]+$ ]] && [ "$ttl" -ge 1 ] && [ "$ttl" -le 1440 ] || die "--ttl is minutes, 1 to 1440"
  [[ "$name" =~ ^.{1,64}$ ]] || die "--name is 1 to 64 characters"
  if [ -n "$session" ] && ! [[ "$session" =~ ^s-([a-z2-7]{10}|[a-z0-9]{5})$ ]]; then
    die "--session is not a session id"
  fi
  local scope
  for scope in $scopes; do
    [[ " $SCOPES_ALLOWED " == *" $scope "* ]] || die "unknown scope $scope"
  done
  owner="$(printf '%s' "$owner" | tr '[:upper:]' '[:lower:]')"

  # The backend's shape (tokens.go): 12 characters of lower-case base32 from
  # 60 random bits, and 43 of base64url from 32 random bytes.
  local id secret token sha expires
  id="$(python3 -c 'import base64,os; print(base64.b32encode(os.urandom(8)).decode().lower()[:12])')"
  secret="$(python3 -c 'import base64,os; print(base64.urlsafe_b64encode(os.urandom(32)).decode().rstrip("="))')"
  token="bjs_${id}_${secret}"
  sha="$(printf '%s' "$token" | python3 -c 'import hashlib,sys; print(hashlib.sha256(sys.stdin.buffer.read()).hexdigest())')"
  expires="$(python3 -c "import datetime; print((datetime.datetime.now(datetime.timezone.utc)+datetime.timedelta(minutes=$ttl)).strftime('%Y-%m-%dT%H:%M:%SZ'))")"

  # The backend's owner label (sessions.OwnerLabel): its token page lists
  # by it, so the owner sees the token while it exists.
  local owner_label
  owner_label="$(printf '%s' "$owner" | python3 -c 'import hashlib,sys; print(hashlib.sha256(sys.stdin.buffer.read()).hexdigest()[:32])')"

  local scope_list="" s
  for s in $scopes; do scope_list="$scope_list\"$s\","; done
  local session_line=""
  [ -z "$session" ] || session_line="\"session\": \"$session\","
  # JSON, so nothing in the owner's address or the name is read as YAML.
  kubectl -n "$NS" create -f - > /dev/null <<EOF
{
  "apiVersion": "browserjs.dev/v1alpha1",
  "kind": "APIToken",
  "metadata": {
    "name": "tok-$id",
    "labels": {"browserjs.dev/owner": "$owner_label", "app.kubernetes.io/managed-by": "mint-token"}
  },
  "spec": {
    "id": "$id",
    "owner": $(python3 -c 'import json,sys; print(json.dumps(sys.argv[1]))' "$owner"),
    "name": $(python3 -c 'import json,sys; print(json.dumps(sys.argv[1]))' "$name"),
    "scopes": [${scope_list%,}],
    $session_line
    "expiresAt": "$expires",
    "sha256": "$sha"
  }
}
EOF
  echo "id=$id"
  echo "token=$token"
}

delete() {
  local id="${1:-}"
  [[ "$id" =~ ^[a-z2-7]{12}$ ]] || die "delete takes a token id"
  kubectl -n "$NS" delete apitoken "tok-$id" --ignore-not-found --wait=false > /dev/null
  echo "mint-token: tok-$id deleted" >&2
}

case "${1:-}" in
  create) shift; create "$@" ;;
  delete) shift; delete "$@" ;;
  *) die "usage: mint-token.sh create --owner <email> --scopes \"...\" [--ttl <minutes>] [--session <id>] [--name <label>] | delete <id>" ;;
esac
