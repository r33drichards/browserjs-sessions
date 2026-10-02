#!/usr/bin/env bash
# `chromium`, for everything in a session that wants the browser: the panel
# launcher, the menu, a link opened from another program, the command typed in
# a terminal, and the MCP server on the first browser_execute call.
#
# A session starts with the desktop only. This starts the session's one
# Chromium when it is not running (its profile on the session's disk, remote
# debugging on loopback, maximised) and returns once it answers; when it is
# running, it hands the arguments to it (a new window, or the URLs as tabs)
# like any second `chromium` does. Never a second browser, never another
# profile.
set -euo pipefail

: "${CHROMIUM_BIN:?set CHROMIUM_BIN to the Chromium binary}"
PROFILE_DIR="${BROWSER_PROFILE_DIR:-/data/chrome}"
RESTORE_FLAG="${BROWSER_RESTORE_FLAG:-}"
CDP_PORT=9222
RUNTIME="${XDG_RUNTIME_DIR:-/tmp}"

# The main Chromium process: launched with our profile and, unlike its
# renderer/gpu/utility children, no --type= flag.
browser_pids() {
  local pid cmd
  for pid in $(pgrep -f -- "--remote-debugging-port=$CDP_PORT" || true); do
    cmd="$({ tr '\0' ' ' <"/proc/$pid/cmdline"; } 2>/dev/null || true)"
    case "$cmd" in
      "" | *--type=*) ;;
      *"--user-data-dir=$PROFILE_DIR "*) echo "$pid" ;;
    esac
  done
}
answers() { curl -fsS --max-time 1 "http://127.0.0.1:$CDP_PORT/json/version" >/dev/null 2>&1; }

desktop_size() {
  local size
  size="$(xdpyinfo 2>/dev/null | sed -n 's/^ *dimensions: *\([0-9]*\)x\([0-9]*\) pixels.*/\1,\2/p' | head -n 1)"
  echo "${size:-1280,800}"
}

# Maximises Chromium's windows once it has put them up. --start-maximized
# does that for a new profile, but a window restored from the last session
# comes back the size it was saved with, on a desktop that may be another
# size now. After this they are ordinary windows: someone may unmaximise one,
# and xfwm4 refits those that are maximised whenever the desktop is resized.
browser_windows() {
  { wmctrl -lx 2>/dev/null || true; } | awk 'tolower($3) ~ /chromium/ { print $1 }'
}
maximise_browser_windows() {
  local ids="" id
  for _ in $(seq 1 60); do
    ids="$(browser_windows)"
    [ -z "$ids" ] || break
    sleep 0.5
  done
  # Restored windows appear one after another.
  sleep 1
  for id in $(browser_windows); do
    wmctrl -i -r "$id" -b add,maximized_vert,maximized_horz 2>/dev/null || true
  done
}

# BROWSER_MAXIMISE_ONLY=1: only maximise the browser's windows, once they are
# up (the MCP server, after it opened the first window of a browser that was
# started without one).
if [ "${BROWSER_MAXIMISE_ONLY:-}" = 1 ]; then
  maximise_browser_windows
  exit 0
fi

# One start at a time: two callers at once (a launcher click and a
# browser_execute call) must not both find no browser and start one each.
lock="$RUNTIME/chromium-start.lock"
for _ in $(seq 1 300); do
  mkdir "$lock" 2>/dev/null && break
  # A starter that died holding it.
  if [ -n "$(find "$lock" -maxdepth 0 -mmin +1 2>/dev/null)" ]; then rmdir "$lock" 2>/dev/null || true; fi
  sleep 0.2
done
trap 'rmdir "$lock" 2>/dev/null || true' EXIT

if [ -n "$(browser_pids)" ]; then
  rmdir "$lock" 2>/dev/null || true
  trap - EXIT
  # Started without a window (BROWSER_START_HIDDEN) and none opened since:
  # the window this opens is its first, and is maximised like one.
  [ -n "$(browser_windows)" ] || maximise_browser_windows &
  exec "$CHROMIUM_BIN" --no-sandbox --user-data-dir="$PROFILE_DIR" "$@"
fi

# Not running. A previous pod, or a crash, leaves the singleton lock pointing
# at a dead process; Chromium then refuses to start.
rm -f "$PROFILE_DIR"/Singleton{Lock,Socket,Cookie}
# After an unclean exit Chromium shows a "restore pages?" bubble instead of
# restoring; mark the previous exit as clean. Best effort.
prefs="$PROFILE_DIR/Default/Preferences"
if [ -n "$RESTORE_FLAG" ] && [ -f "$prefs" ]; then
  sed -i 's/"exit_type":"[A-Za-z]*"/"exit_type":"Normal"/' "$prefs" ||
    echo "warning: could not mark $prefs as cleanly exited; Chromium may ask before restoring tabs" >&2
fi
if [ -n "${FILES_DIR:-}" ]; then
  browser-mcp download-dir "$PROFILE_DIR" "$FILES_DIR" ||
    echo "warning: could not point Chromium's downloads at $FILES_DIR" >&2
fi
# A start URL is opened next to the restored tabs, so with a session to
# restore pass none (or every start would add one more blank tab).
start=("$@")
# BROWSER_START_HIDDEN=1: started ahead of use, for a new session (the MCP
# server's /browser/start). Chromium runs and answers on the debugging port
# but opens no window; the first browser_execute call, the panel launcher or
# a link opens one. A new session has no tabs to restore.
hidden=""
if [ "${BROWSER_START_HIDDEN:-}" = 1 ]; then
  hidden=--no-startup-window
  start=()
elif [ ${#start[@]} -eq 0 ]; then
  if [ -z "$RESTORE_FLAG" ] || [ -z "$(ls -A "$PROFILE_DIR/Default/Sessions" 2>/dev/null)" ]; then
    start=(about:blank)
  fi
fi

[ -n "$hidden" ] || maximise_browser_windows &
# In the background, and not this script's to wait for: it outlives the
# launcher click or the tool call that started it.
# shellcheck disable=SC2086
nohup "$CHROMIUM_BIN" \
  --no-sandbox \
  --disable-gpu \
  --disable-dev-shm-usage \
  --no-first-run \
  --no-default-browser-check \
  --password-store=basic \
  --user-data-dir="$PROFILE_DIR" \
  --remote-debugging-address=127.0.0.1 \
  --remote-debugging-port="$CDP_PORT" \
  --window-position=0,0 \
  --window-size="$(desktop_size)" \
  --force-device-scale-factor=1 \
  --start-maximized \
  $RESTORE_FLAG $hidden \
  "${start[@]}" </dev/null >&2 &
disown

for _ in $(seq 1 150); do
  answers && exit 0
  sleep 0.2
done
echo "chromium: started, but it does not answer on 127.0.0.1:$CDP_PORT" >&2
exit 1
