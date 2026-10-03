#!/usr/bin/env bash
# mcp-exec (github.com/r33drichards/mcp-exec): shell commands for run_js, as
# the "exec" MCP server of mcp-js. Started by entrypoint.sh in the browser
# container, so a command runs on the desktop: this user, this HOME, PATH and
# DISPLAY, starting in the home directory.
#
# Who can call it. It has no login, and it shares the pod's loopback with
# Chromium, where any web page the session opens runs. Three things keep
# pages (and everything outside the pod) out:
# - it listens on 127.0.0.1 only: the backend, the only peer the session
#   NetworkPolicy admits, cannot reach it;
# - --reject-browser-requests: a request with an Origin or Sec-Fetch-* header
#   is answered 403. A browser adds those itself and page script cannot remove
#   them; mcp-js sends neither. The same rule as callers.js for this
#   container's own server;
# - mcp-exec only answers a loopback Host (DNS rebinding) and only JSON bodies.
#
# Logs are files on the session's disk, so the output of a command is still
# there after the pod slept or was restored from a snapshot. The commands
# themselves are not: what a previous pod left "running" is marked as
# interrupted here, and logs older than EXEC_LOG_KEEP_DAYS are deleted.
set -euo pipefail

EXEC_LOG_DIR="${EXEC_LOG_DIR:?set EXEC_LOG_DIR (where mcp-exec keeps the logs of commands)}"
EXEC_MCP_PORT="${EXEC_MCP_PORT:-8082}"

mkdir -p "$EXEC_LOG_DIR"
chmod 700 "$EXEC_LOG_DIR"
find "$EXEC_LOG_DIR" -maxdepth 1 -type f -mtime +"${EXEC_LOG_KEEP_DAYS:-7}" -delete || true
for status in "$EXEC_LOG_DIR"/*.status; do
  [ -f "$status" ] || continue
  if [ "$(cat "$status")" = '"Running"' ]; then
    printf '%s' '{"Failed":"interrupted: the session restarted while the command was running"}' >"$status"
  fi
done

cd "$HOME"
exec mcp-exec --http-port "$EXEC_MCP_PORT" --bind-address 127.0.0.1 \
  --reject-browser-requests --directory-path "$EXEC_LOG_DIR"
