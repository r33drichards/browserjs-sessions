#!/usr/bin/env bash
# Smoke test of the production deployment, from outside, with curl only. No
# sign-in and no session: it checks the edge (addresses, certificates,
# routes) and what answers without an identity.
#
#   test/smoke.sh                      checks browserjs.com
#   DOMAIN=example.org test/smoke.sh   another deployment
#   INSECURE=1 test/smoke.sh           do not verify certificates (a Let's
#                                      Encrypt staging certificate): the
#                                      certificate checks then fail, the
#                                      rest still runs
#
# Exits 0 only if every check passes.
set -uo pipefail

DOMAIN="${DOMAIN:-browserjs.com}"
APP="app.$DOMAIN"
AUTHENTICATE="authenticate.$DOMAIN"
DEX="dex.$DOMAIN"
# A well-formed session ID that (almost certainly) names no session, new each
# run: it also shows that the wildcard covers a host nobody has asked for.
ID="s-$(LC_ALL=C tr -dc 'a-z2-7' </dev/urandom | head -c 10)"
SESSION="$ID.sessions.$DOMAIN"
# Under the session domain, but not the form of a session ID.
MALFORMED="not-a-session.sessions.$DOMAIN"

insecure=()
[ -z "${INSECURE:-}" ] || insecure=(-k)
# (Written this way for the bash 3.2 of macOS, where an empty array is unset.)
fetch() { curl -sS --max-time 20 ${insecure[@]+"${insecure[@]}"} "$@"; }

passed=0
failed=0
pass() {
  passed=$((passed + 1))
  echo "PASS  $1"
}
fail() {
  failed=$((failed + 1))
  echo "FAIL  $1"
  [ -z "${2:-}" ] || printf '      %s\n' "$2"
}
# check description, expected, actual
is() { if [ "$2" = "$3" ]; then pass "$1"; else fail "$1" "expected: $2   got: $3"; fi; }
status() { fetch -o /dev/null -w '%{http_code}' "$@" 2>/dev/null; }
location() { fetch -o /dev/null -w '%{redirect_url}' "$@" 2>/dev/null; }
# A JSON document without its whitespace, so a grep need not know its layout.
compact() { tr -d ' \n\r\t'; }

echo "Deployment: $DOMAIN   session host used: $SESSION"
echo

# --- certificates ------------------------------------------------------------
# curl verifies the chain and that the certificate names the host, so success
# is the proof; the session host shows that the wildcard is on it.
for host in "$APP" "$AUTHENTICATE" "$DEX" "$SESSION"; do
  if error="$(curl -sS --max-time 20 -o /dev/null "https://$host/" 2>&1)"; then
    pass "certificate is valid for $host"
  else
    fail "certificate is valid for $host" "$error"
  fi
done
# Informational: who issued it (not every curl can print this).
issuer="$(curl -s --max-time 20 -k -o /dev/null -w '%{certs}' "https://$APP/" 2>/dev/null | grep -i -m1 '^issuer:' || true)"
[ -z "$issuer" ] || echo "      $issuer"
case "$issuer" in *STAGING*) echo "      this is a Let's Encrypt STAGING certificate: run the deploy with issuer=production" ;; esac

# --- the app -----------------------------------------------------------------
is "the app answers with a redirect" 302 "$(status "https://$APP/")"
to="$(location "https://$APP/")"
case "$to" in
  "https://$AUTHENTICATE/"*) pass "the app redirects to $AUTHENTICATE to sign in" ;;
  *) fail "the app redirects to $AUTHENTICATE to sign in" "redirected to: ${to:-nowhere}" ;;
esac
is "the app's API answers 401 to a client that wants JSON" 401 \
  "$(status -H 'Accept: application/json' "https://$APP/api/me")"

# --- Pomerium ----------------------------------------------------------------
jwks="$(fetch "https://$AUTHENTICATE/.well-known/pomerium/jwks.json" 2>/dev/null | compact)"
case "$jwks" in
  *'"keys":[{'*) pass "$AUTHENTICATE serves Pomerium's signing keys" ;;
  *) fail "$AUTHENTICATE serves Pomerium's signing keys" "got: ${jwks:0:200}" ;;
esac

# --- Dex ---------------------------------------------------------------------
discovery="$(fetch "https://$DEX/dex/.well-known/openid-configuration" 2>/dev/null | compact)"
case "$discovery" in
  *"\"issuer\":\"https://$DEX/dex\""*) pass "Dex's discovery document has the issuer https://$DEX/dex" ;;
  *) fail "Dex's discovery document has the issuer https://$DEX/dex" "got: ${discovery:0:200}" ;;
esac
# What Pomerium sends a user to. With several connectors Dex answers with the
# page that lists them.
login="$(fetch -L "https://$DEX/dex/auth?client_id=pomerium&response_type=code&scope=openid+email+profile&state=smoke&redirect_uri=https%3A%2F%2F$AUTHENTICATE%2Foauth2%2Fcallback" 2>/dev/null)"
for connector in google github; do
  if grep -q "/dex/auth/$connector" <<<"$login"; then
    pass "Dex's sign-in page offers $connector"
  else
    fail "Dex's sign-in page offers $connector" "no link to /dex/auth/$connector on the page"
  fi
done
if grep -q "/dex/auth/local" <<<"$login"; then
  fail "Dex's sign-in page offers no password login" "found a link to /dex/auth/local"
else
  pass "Dex's sign-in page offers no password login"
fi

# --- session hosts -----------------------------------------------------------
is "OAuth metadata on a malformed session host is 404" 404 \
  "$(status "https://$MALFORMED/.well-known/oauth-protected-resource")"
metadata="$(fetch "https://$SESSION/.well-known/oauth-protected-resource" 2>/dev/null | compact)"
case "$metadata" in
  *"\"resource\":\"https://$SESSION/mcp\""*"\"authorization_servers\":[\"https://$SESSION\"]"* | \
    *"\"authorization_servers\":[\"https://$SESSION\"]"*"\"resource\":\"https://$SESSION/mcp\""*)
    pass "OAuth metadata on a well-formed session host names its MCP endpoint and itself"
    ;;
  *) fail "OAuth metadata on a well-formed session host names its MCP endpoint and itself" "got: ${metadata:0:300}" ;;
esac
server="$(fetch "https://$SESSION/.well-known/oauth-authorization-server" 2>/dev/null | compact)"
case "$server" in
  *"\"token_endpoint\":\"https://$SESSION/.pomerium/mcp/token\""*) pass "authorization server metadata points at Pomerium's MCP token endpoint" ;;
  *) fail "authorization server metadata points at Pomerium's MCP token endpoint" "got: ${server:0:300}" ;;
esac

is "MCP without a token is 401" 401 \
  "$(status -X POST -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
    -d '{"jsonrpc":"2.0","id":1,"method":"ping"}' "https://$SESSION/mcp")"

# The upload route has no sign-in: a session that does not exist is refused
# by the backend, not sent to sign in.
upload="https://$SESSION/api/artifact-uploads/smoke-test-token"
is "upload to a session that does not exist is 404" 404 "$(status -X PUT --data-binary smoke "$upload")"
to="$(location -X PUT --data-binary smoke "$upload")"
is "upload to a session that does not exist is not redirected to sign in" "" "$to"

is "the screen route refuses a plain GET (426), without sign-in" 426 "$(status "https://$SESSION/vnc")"

echo
echo "$passed passed, $failed failed"
[ "$failed" -eq 0 ]
