#!/usr/bin/env bash
# mcp-v8 exits at startup when an upstream MCP server is unreachable, and in a
# session pod the browser container starts alongside this one. Wait for the
# browser MCP to accept connections, then hand over to mcp-v8.
set -euo pipefail

# As PID 1 bash ignores TERM unless it is trapped; without this a pod
# shutdown during the wait hangs until the kill timeout.
trap 'exit 143' TERM INT
addr="${BROWSER_MCP_ADDR:-127.0.0.1:8081}"
for _ in $(seq 1 "${BROWSER_MCP_WAIT_SECONDS:-120}"); do
  if (exec 3<>"/dev/tcp/${addr%:*}/${addr##*:}") 2>/dev/null; then
    exec mcp-v8 "$@"
  fi
  sleep 1
done
echo "browser MCP at $addr did not come up; starting mcp-v8 anyway" >&2
exec mcp-v8 "$@"
