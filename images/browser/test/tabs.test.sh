#!/usr/bin/env bash
# Usage: tabs.test.sh  (needs node and a Chrome/Chromium binary in $CHROME)
set -euo pipefail
cd "$(dirname "$0")/../browser"
[ -d node_modules ] || npm ci --silent
tmp="$(mktemp -d)"; trap 'kill $CPID $SPID 2>/dev/null; wait 2>/dev/null; rm -rf "$tmp"' EXIT
"${CHROME:?set CHROME to a Chrome/Chromium binary}" --headless=new --remote-debugging-port=9334 \
  --user-data-dir="$tmp/prof" --no-first-run about:blank >/dev/null 2>&1 & CPID=$!
sleep 3
start() { CDP_URL=http://127.0.0.1:9334 BROWSER_MCP_PORT=8792 TAB_STATE_FILE="$tmp/tabs.json" node server.js >/dev/null 2>&1 & SPID=$!; sleep 2; }
call() { curl -s -X POST localhost:8792/mcp -H 'content-type: application/json' \
  -H 'accept: application/json, text/event-stream' \
  -d "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\",\"params\":{\"name\":\"browser_execute\",\"arguments\":$1}}"; }
pages() { curl -s localhost:9334/json/list | grep -c '"type": "page"'; }

start
call '{"operations":[{"type":"navigate","params":{"url":"https://example.com"}}]}' >/dev/null
before="$(pages)"
kill $SPID; wait $SPID 2>/dev/null || true
start
out="$(call '{"operations":[{"type":"url"}]}')"
echo "$out" | grep -q 'example.com' || { echo "FAIL: default tab was not rebound: $out"; exit 1; }
[ "$(pages)" = "$before" ] || { echo "FAIL: a new tab was opened ($(pages) vs $before)"; exit 1; }
echo PASS
