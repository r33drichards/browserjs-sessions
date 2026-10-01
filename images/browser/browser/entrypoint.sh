#!/usr/bin/env bash
# One headed Chromium on Xvfb, viewable over noVNC (behind Caddy basic auth on
# $PORT) and drivable over CDP by the browser MCP server (private port 8081).
set -euo pipefail

: "${VNC_PASSWORD:?set VNC_PASSWORD (basic-auth password for the /vnc viewer)}"
VNC_USER="${VNC_USER:-admin}"
PORT="${PORT:-8080}"
DATA_DIR="${DATA_DIR:-/data}"
PROFILE_DIR="$DATA_DIR/chrome"
SCREEN="${SCREEN_GEOMETRY:-1280x800x24}"

export DISPLAY=:99
export HOME=/root
export XDG_RUNTIME_DIR=/tmp/runtime
export LIBGL_ALWAYS_SOFTWARE=1
mkdir -p "$PROFILE_DIR" "$XDG_RUNTIME_DIR" /tmp/.X11-unix
chmod 700 "$XDG_RUNTIME_DIR"
chmod 1777 /tmp /tmp/.X11-unix

# A previous container on the same volume leaves Chromium's singleton lock
# pointing at a dead hostname/pid; Chromium then refuses to start
# ("profile appears to be in use by another Chromium process").
rm -f "$PROFILE_DIR"/Singleton{Lock,Socket,Cookie}
rm -f /tmp/.X99-lock /tmp/.X11-unix/X99

pids=()
cleanup() { kill "${pids[@]}" 2>/dev/null || true; }
trap cleanup EXIT

Xvfb :99 -screen 0 "$SCREEN" -nolisten tcp -ac &
pids+=($!)
for _ in $(seq 1 50); do
  xdpyinfo -display :99 >/dev/null 2>&1 && break
  sleep 0.1
done

openbox --sm-disable &
pids+=($!)

# Keep Chromium alive: if someone closes the last window over VNC or it
# crashes, bring it back with the same profile.
(
  while true; do
    chromium \
      --no-sandbox \
      --disable-gpu \
      --disable-dev-shm-usage \
      --no-first-run \
      --no-default-browser-check \
      --password-store=basic \
      --user-data-dir="$PROFILE_DIR" \
      --remote-debugging-address=127.0.0.1 \
      --remote-debugging-port=9222 \
      --window-position=0,0 \
      --window-size="${SCREEN%x*}" \
      --start-maximized \
      about:blank || true
    echo "chromium exited; restarting in 2s" >&2
    rm -f "$PROFILE_DIR"/Singleton{Lock,Socket,Cookie}
    sleep 2
  done
) &
pids+=($!)

x11vnc -display :99 -localhost -rfbport 5900 -forever -shared -nopw -quiet -noxdamage &
pids+=($!)

websockify --web "$NOVNC_WEB" 127.0.0.1:6080 127.0.0.1:5900 &
pids+=($!)

VNC_HASH="$(caddy hash-password --plaintext "$VNC_PASSWORD")"
export VNC_USER VNC_HASH PORT
caddy run --adapter caddyfile --config "$CADDYFILE" &
pids+=($!)

browser-mcp &
pids+=($!)

# Exit (and let Railway restart us) if any core process dies.
wait -n "${pids[@]}"
echo "a core process exited; shutting down" >&2
exit 1
