#!/usr/bin/env bash
# Usage: tabs.test.sh  (needs node and a Chrome/Chromium binary in $CHROME)
set -euo pipefail
: "${CHROME:?set CHROME to a Chrome/Chromium binary}"
cd "$(dirname "$0")/../browser"
[ -d node_modules ] || npm ci --silent
tmp="$(mktemp -d)"; trap 'kill $CPID $SPID 2>/dev/null; wait 2>/dev/null; rm -rf "$tmp"' EXIT
state="$tmp/tabs.json"
"$CHROME" --headless=new --remote-debugging-port=9334 \
  --user-data-dir="$tmp/prof" --no-first-run about:blank >/dev/null 2>&1 & CPID=$!
sleep 3
start() { CDP_URL=http://127.0.0.1:9334 BROWSER_MCP_PORT=8792 TAB_STATE_FILE="$state" node server.js >/dev/null 2>&1 & SPID=$!; sleep 2; }
call() { curl -s -X POST localhost:8792/mcp -H 'content-type: application/json' \
  -H 'accept: application/json, text/event-stream' \
  -d "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\",\"params\":{\"name\":\"browser_execute\",\"arguments\":$1}}"; }
pages() { curl -s localhost:9334/json/list | grep -c '"type": "page"'; }

# A restarted server rebinds a name to the page it had, without a new tab.
start
call '{"operations":[{"type":"navigate","params":{"url":"https://example.com"}}]}' >/dev/null
before="$(pages)"
kill $SPID; wait $SPID 2>/dev/null || true
start
out="$(call '{"operations":[{"type":"url"}]}')"
echo "$out" | grep -q 'example.com' || { echo "FAIL: default tab was not rebound: $out"; exit 1; }
[ "$(pages)" = "$before" ] || { echo "FAIL: a new tab was opened ($(pages) vs $before)"; exit 1; }

# An explicit close forgets the name, as soon as the call returns.
call '{"tab":"scratch","operations":[{"type":"navigate","params":{"url":"https://example.com/?scratch"}}]}' >/dev/null
grep -q '"scratch"' "$state" || { echo "FAIL: scratch tab was not saved: $(cat "$state")"; exit 1; }
call '{"tab":"scratch","operations":[{"type":"url"}],"close":true}' >/dev/null
if grep -q '"scratch"' "$state"; then echo "FAIL: closed tab is still saved: $(cat "$state")"; exit 1; fi
grep -q '"default"' "$state" || { echo "FAIL: closing scratch dropped default: $(cat "$state")"; exit 1; }

# Chromium quitting closes every page; with the server still running, that
# must not touch the saved state.
call '{"tab":"two","operations":[{"type":"navigate","params":{"url":"https://example.com/?two"}}]}' >/dev/null
saved="$(cat "$state")"
for want in '"default":"https://example.com/"' '"two":"https://example.com/?two"'; do
  case "$saved" in *"$want"*) ;; *) echo "FAIL: state lacks $want: $saved"; exit 1 ;; esac
done
kill -TERM $CPID; wait $CPID 2>/dev/null || true
sleep 5
kill -0 $SPID 2>/dev/null || { echo "FAIL: server died with the browser"; exit 1; }
[ "$(cat "$state")" = "$saved" ] || { echo "FAIL: browser quit changed the state: $(cat "$state") vs $saved"; exit 1; }
echo PASS
