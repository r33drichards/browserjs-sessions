#!/usr/bin/env bash
# One headed Chromium on Xvnc, viewable over noVNC (behind Caddy basic auth on
# $PORT) and drivable over CDP by the browser MCP server (private port 8081).
#
# SESSION_MODE=1 (a browserjs session pod): no Caddy and no VNC password, the
# backend is the only way in; websockify listens on all interfaces and
# Chromium restores its tabs across restarts.
set -euo pipefail

VNC_USER="${VNC_USER:-admin}"
PORT="${PORT:-8080}"
DATA_DIR="${DATA_DIR:-/data}"
PROFILE_DIR="$DATA_DIR/chrome"
# Where Chromium downloads to and its file chooser opens, and what the session
# page lists (files.js). In the profile's volume, so it outlives the pod.
export FILES_DIR="${FILES_DIR:-$PROFILE_DIR/Downloads}"
SCREEN="${SCREEN_GEOMETRY:-1280x800x24}"
# WxHxDepth, the size before any viewer asks for another; Chromium wants its
# window size as "W,H".
SCREEN_W="${SCREEN%%x*}"
SCREEN_H="${SCREEN#*x}"
SCREEN_H="${SCREEN_H%%x*}"
SCREEN_D=24
case "$SCREEN" in
  *x*x*) SCREEN_D="${SCREEN##*x}" ;;
esac

SESSION_MODE="${SESSION_MODE:-0}"
if [ "$SESSION_MODE" != 1 ]; then
  : "${VNC_PASSWORD:?set VNC_PASSWORD (basic-auth password for the /vnc viewer)}"
fi
WEBSOCKIFY_BIND=127.0.0.1
RESTORE_FLAG=""
if [ "$SESSION_MODE" = 1 ]; then
  WEBSOCKIFY_BIND=0.0.0.0
  RESTORE_FLAG="--restore-last-session"
fi

# Under containerd the open-file limit can be a billion (Docker's default is
# far lower). x11vnc, which this image used to run, walked every possible
# descriptor when a viewer connected and never answered; keep the limit sane
# for whatever else sizes itself by it. Not fatal if it cannot be changed.
ulimit -n 65536 2>/dev/null || echo "warning: could not lower the open-file limit ($(ulimit -n))" >&2

# Who this runs as. A session pod runs it as the image's unprivileged user
# (uid 1000, "browser", home /home/browser) with no capabilities; the
# standalone deployment still runs it as root, whose volume at /data is
# root's. Nothing below needs root. Everything outside the profile that
# Chromium, openbox, fontconfig and Caddy write (caches, crash reports, the
# certificate store) goes under HOME.
if [ "$(id -u)" = 0 ]; then
  export HOME=/root
else
  export HOME="${HOME:-/home/browser}"
  # A uid the image does not know has no home, or "/".
  if ! mkdir -p "$HOME" 2>/dev/null || [ ! -w "$HOME" ]; then
    export HOME=/tmp/home
    mkdir -p "$HOME"
  fi
fi

export DISPLAY=:99
export XDG_RUNTIME_DIR=/tmp/runtime
export LIBGL_ALWAYS_SOFTWARE=1
mkdir -p "$PROFILE_DIR" "$XDG_RUNTIME_DIR" /tmp/.X11-unix
chmod 700 "$XDG_RUNTIME_DIR"
# /tmp is already world-writable in the image, and only its owner may change
# it: as another user this fails, harmlessly.
chmod 1777 /tmp /tmp/.X11-unix 2>/dev/null || true
if [ ! -w "$PROFILE_DIR" ]; then
  echo "error: $PROFILE_DIR is not writable by uid $(id -u) (groups: $(id -G)); as a volume it must belong to this user or be group-writable for one of its groups (fsGroup)" >&2
  exit 1
fi

# A previous container on the same volume leaves Chromium's singleton lock
# pointing at a dead hostname/pid; Chromium then refuses to start
# ("profile appears to be in use by another Chromium process").
rm -f "$PROFILE_DIR"/Singleton{Lock,Socket,Cookie}
rm -f /tmp/.X99-lock /tmp/.X11-unix/X99

# The main Chromium process: launched with our profile and, unlike its
# renderer/gpu/utility children, no --type= flag. Matching on the command line
# works whatever the Nix wrapper names the binary (chromium, .chromium-wrapped).
browser_pids() {
  local pid cmd
  for pid in $(pgrep -f -- "--user-data-dir=$PROFILE_DIR" || true); do
    # The pid can be gone by now: no cmdline to read, or an empty one.
    cmd="$({ tr '\0' ' ' <"/proc/$pid/cmdline"; } 2>/dev/null || true)"
    [ -n "$cmd" ] || continue
    case "$cmd" in
      *--type=*) ;;
      *) echo "$pid" ;;
    esac
  done
}

pids=()
chromium_loop=""
cleaned=""
# Chromium only writes a complete session file on a clean exit, so on SIGTERM
# (pod shutdown, suspend) ask it to quit and wait before killing the rest.
cleanup() {
  local bp
  # Runs again from the EXIT trap after a signal.
  [ -z "$cleaned" ] || return 0
  cleaned=1
  # Stop the restart loop first so it cannot bring Chromium back.
  [ -z "$chromium_loop" ] || kill "$chromium_loop" 2>/dev/null || true
  bp="$(browser_pids)"
  if [ -n "$bp" ]; then
    # shellcheck disable=SC2086
    kill -TERM $bp 2>/dev/null || true
    for _ in $(seq 1 50); do
      [ -n "$(browser_pids)" ] || break
      sleep 0.2
    done
  fi
  kill "${pids[@]}" 2>/dev/null || true
}
trap cleanup EXIT
# Exit after cleaning up, or a signal during startup would let the script
# carry on starting processes.
trap 'cleanup; exit 143' TERM INT

# Xvnc is the X server and the VNC server in one. Unlike x11vnc on Xvfb it
# honours a viewer's request to resize the desktop (SetDesktopSize), which is
# how the screen follows the viewer's window; openbox then refits the
# maximised Chromium window (see openbox-rc.xml). The size only changes when a viewer asks, so it
# stays put while nobody is connected (and across a snapshot and restore).
#
# No VNC password, as before: it listens on loopback only and websockify is
# the way in. SendPrimary=0: only text that was copied goes to the viewer's
# clipboard, not every selection.
Xvnc :99 -geometry "${SCREEN_W}x${SCREEN_H}" -depth "$SCREEN_D" -nolisten tcp -ac \
  -rfbport 5900 -localhost -UseIPv6=0 -SecurityTypes None -AlwaysShared \
  -AcceptSetDesktopSize -SendPrimary=0 &
pids+=($!)
for _ in $(seq 1 50); do
  xdpyinfo -display :99 >/dev/null 2>&1 && break
  sleep 0.1
done

# The config is what maximises Chromium's windows, so that they follow the
# desktop's size.
openbox --sm-disable ${OPENBOX_RC:+--config-file "$OPENBOX_RC"} &
pids+=($!)

# The desktop's size now, as Chromium wants it ("W,H"): a viewer may have
# resized it since the start. openbox maximises the window anyway; this is
# the size it gets before that.
desktop_size() {
  local size
  size="$(xdpyinfo -display :99 2>/dev/null | sed -n 's/^ *dimensions: *\([0-9]*\)x\([0-9]*\) pixels.*/\1,\2/p' | head -n 1)"
  echo "${size:-$SCREEN_W,$SCREEN_H}"
}

# Keep Chromium alive: if someone closes the last window over VNC or it
# crashes, bring it back with the same profile.
(
  while true; do
    # After an unclean exit Chromium shows a "restore pages?" bubble instead
    # of restoring; mark the previous exit as clean. Best effort: a failure
    # here (disk full, permissions) must not end this loop.
    prefs="$PROFILE_DIR/Default/Preferences"
    if [ -n "$RESTORE_FLAG" ] && [ -f "$prefs" ]; then
      sed -i 's/"exit_type":"[A-Za-z]*"/"exit_type":"Normal"/' "$prefs" ||
        echo "warning: could not mark $prefs as cleanly exited; Chromium may ask before restoring tabs" >&2
    fi
    browser-mcp download-dir "$PROFILE_DIR" "$FILES_DIR" ||
      echo "warning: could not point Chromium's downloads at $FILES_DIR" >&2
    # A start URL is opened next to the restored tabs, so with a session to
    # restore pass none (or every restart would add one more blank tab).
    start_url=about:blank
    if [ -n "$RESTORE_FLAG" ] && [ -n "$(ls -A "$PROFILE_DIR/Default/Sessions" 2>/dev/null)" ]; then
      start_url=""
    fi
    # shellcheck disable=SC2086
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
      --window-size="$(desktop_size)" \
      --force-device-scale-factor=1 \
      --start-maximized \
      $RESTORE_FLAG \
      $start_url || true
    echo "chromium exited; restarting in 2s" >&2
    rm -f "$PROFILE_DIR"/Singleton{Lock,Socket,Cookie}
    sleep 2
  done
) &
chromium_loop=$!
pids+=($!)

websockify --web "$NOVNC_WEB" "$WEBSOCKIFY_BIND:6080" 127.0.0.1:5900 &
pids+=($!)

if [ "$SESSION_MODE" != 1 ]; then
  VNC_HASH="$(caddy hash-password --plaintext "$VNC_PASSWORD")"
  export VNC_USER VNC_HASH PORT
  caddy run --adapter caddyfile --config "$CADDYFILE" &
  pids+=($!)
fi

browser-mcp &
pids+=($!)

# Exit (and let Railway restart us) if any core process dies.
wait -n "${pids[@]}"
echo "a core process exited; shutting down" >&2
exit 1
