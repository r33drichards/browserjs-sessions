#!/usr/bin/env bash
# mcp-v8 exits at startup when an upstream MCP server is unreachable, and in a
# session pod the browser container, which runs both of them (the browser MCP
# and mcp-exec, mcp-servers.json), starts alongside this one. Wait for each to
# accept connections, then start mcp-v8.
set -euo pipefail

# As PID 1 bash ignores TERM unless it is trapped; without this a pod
# shutdown during the wait hangs until the kill timeout.
trap 'exit 143' TERM INT
up() {
  (exec 3<>"/dev/tcp/${1%:*}/${1##*:}") 2>/dev/null
}
# The seconds are shared: the second server is waited for with what is left.
waited=0
for addr in "${BROWSER_MCP_ADDR:-127.0.0.1:8081}" "${EXEC_MCP_ADDR:-127.0.0.1:8082}"; do
  until up "$addr" || [ "$waited" -ge "${BROWSER_MCP_WAIT_SECONDS:-120}" ]; do
    sleep 1
    waited=$((waited + 1))
  done
  up "$addr" || echo "MCP server at $addr did not come up; starting mcp-v8 anyway" >&2
done

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
