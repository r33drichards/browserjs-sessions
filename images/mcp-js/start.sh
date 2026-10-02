#!/usr/bin/env bash
# mcp-v8 exits at startup when an upstream MCP server is unreachable, and in a
# session pod the browser container starts alongside this one. Wait for the
# browser MCP to accept connections, then start mcp-v8.
set -euo pipefail

# As PID 1 bash ignores TERM unless it is trapped; without this a pod
# shutdown during the wait hangs until the kill timeout.
trap 'exit 143' TERM INT
addr="${BROWSER_MCP_ADDR:-127.0.0.1:8081}"
for _ in $(seq 1 "${BROWSER_MCP_WAIT_SECONDS:-120}"); do
  if (exec 3<>"/dev/tcp/${addr%:*}/${addr##*:}") 2>/dev/null; then
    break
  fi
  sleep 1
done
(exec 3<>"/dev/tcp/${addr%:*}/${addr##*:}") 2>/dev/null ||
  echo "browser MCP at $addr did not come up; starting mcp-v8 anyway" >&2

# mcp-v8 has no TERM handler, and a process that is PID 1 is not stopped by a
# signal it does not handle: exec'd, it sat out every pod shutdown until the
# kill 30 s later. So it runs as a child, where TERM ends it, and this script
# stays PID 1 to pass the signal on.
mcp-v8 "$@" &
child=$!
trap 'kill -TERM "$child" 2>/dev/null' TERM INT
status=0
wait "$child" || status=$?
# wait returns early when a trapped signal arrives; collect the child.
if kill -0 "$child" 2>/dev/null; then
  status=0
  wait "$child" || status=$?
fi
exit "$status"
