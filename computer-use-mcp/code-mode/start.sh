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
wait_for() { # address, seconds
  for _ in $(seq 1 "$2"); do
    up "$1" && return 0
    sleep 1
  done
  up "$1"
}

browser="${BROWSER_MCP_ADDR:-127.0.0.1:8081}"
wait_for "$browser" "${BROWSER_MCP_WAIT_SECONDS:-120}" ||
  echo "browser MCP at $browser did not come up; starting mcp-v8 anyway" >&2

# The exec server starts a moment after the browser MCP (entrypoint.sh). A
# session without it (a browser image from before it existed, or one where it
# failed to start) still gets its browser: mcp-v8 is started with the
# browser server only, instead of exiting over and over on the missing one.
# Shell commands are then unavailable until the pod is restarted.
exec_addr="${EXEC_MCP_ADDR:-127.0.0.1:8082}"
if ! wait_for "$exec_addr" "${EXEC_MCP_WAIT_SECONDS:-30}"; then
  echo "exec MCP (mcp-exec) at $exec_addr did not come up; starting mcp-v8 without shell commands" >&2
  export MCP_V8_MCP_CONFIG="${MCP_V8_MCP_CONFIG_NO_EXEC:-/etc/mcp/mcp-servers.no-exec.json}"
fi

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
