#!/usr/bin/env bash
# An XFCE desktop on Xvnc with one Chromium on it, viewable over noVNC (behind
# Caddy basic auth on $PORT); Chromium is drivable over CDP by the browser MCP
# server (private port 8081). docs/desktop.md describes the desktop.
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

# Who this runs as. A session pod runs it as the image's unprivileged user
# (uid 1000, "browser") with no capabilities; the standalone deployment still
# runs it as root, whose volume at /data is root's. Nothing below needs root.
#
# HOME is on the volume, next to Chromium's profile, so it outlives the pod
# like the profile does: the desktop's settings, the shell's history, and
# whatever is made in a terminal or saved from an editor. It is where the
# terminal and the file manager open. Everything else that Chromium, XFCE,
# fontconfig and Caddy write outside the profile (caches, the certificate
# store) goes under it too.
export HOME="$PROFILE_DIR/home"
export USER LOGNAME
USER="$(id -un 2>/dev/null || echo browser)"
LOGNAME="$USER"
mkdir -p "$FILES_DIR" "$HOME/Desktop" "$HOME/.config"
# The folder the session page lists and Chromium downloads to is the home
# directory's Downloads, for the file manager and for `cd ~/Downloads`.
[ -e "$HOME/Downloads" ] || [ -L "$HOME/Downloads" ] || ln -s "$FILES_DIR" "$HOME/Downloads"
# Without this file every "well-known" folder is HOME itself, Downloads
# included. Only Desktop and Downloads exist; a user may add the others.
if [ ! -e "$HOME/.config/user-dirs.dirs" ]; then
  # shellcheck disable=SC2016
  printf 'XDG_%s_DIR="$HOME/%s"\n' DESKTOP Desktop DOWNLOAD Downloads \
    >"$HOME/.config/user-dirs.dirs"
fi
if [ ! -e "$HOME/.bashrc" ] && [ -n "${DESKTOP_BASHRC:-}" ]; then
  install -m 644 "$DESKTOP_BASHRC" "$HOME/.bashrc"
fi
cd "$HOME"

# The desktop's environment, inherited by everything started below and so by
# every program opened on the desktop and every shell in a terminal. The
# image sets where XFCE's programs, menu entries, icons, D-Bus services and
# default settings are (XDG_DATA_DIRS, XDG_CONFIG_DIRS), and PATH.
export XDG_CURRENT_DESKTOP=XFCE
export XDG_SESSION_TYPE=x11
export GDK_BACKEND=x11
# The C locale with UTF-8: the only one glibc has without locale files.
export LANG=C.UTF-8
export EDITOR=nano
export PAGER=less
# GTK settings (the file chooser's, the editor's) in a file under HOME, not in
# dconf, whose daemon the image does not have.
export GSETTINGS_BACKEND=keyfile
# No accessibility bus to look for.
export NO_AT_BRIDGE=1
# Chromium's profile, for the `chromium` command of the desktop.
export BROWSER_PROFILE_DIR="$PROFILE_DIR"
# The session bus XFCE keeps its settings on (xfconfd is started through it).
export DBUS_SESSION_BUS_ADDRESS="unix:path=$XDG_RUNTIME_DIR/bus"
# The same environment for a shell that was not started from the desktop
# (kubectl exec, docker exec): `. /tmp/runtime/session-env`.
(umask 077 && export -p >"$XDG_RUNTIME_DIR/session-env")

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
  # xfconfd writes a changed setting to disk some seconds later, or when it
  # is asked to stop: ask, so that a setting changed just now is kept.
  if pkill -TERM -x xfconfd 2>/dev/null; then
    for _ in $(seq 1 10); do
      pgrep -x xfconfd >/dev/null 2>&1 || break
      sleep 0.1
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
# how the screen follows the viewer's window; xfwm4 then refits every
# maximised window, Chromium's among them, and the panel moves to the new
# edge. The size only changes when a viewer asks, so it stays put while nobody
# is connected (and across a snapshot and restore).
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

# Nothing may blank the screen: there is no screensaver or locker in the
# image, and this turns off the X server's own timer.
xset s off s noblank 2>/dev/null || true

# The session bus. If it dies the desktop has lost its settings: a core
# process, like Xvnc.
rm -f "$XDG_RUNTIME_DIR/bus"
dbus-daemon --config-file="$DBUS_SESSION_CONF" --address="$DBUS_SESSION_BUS_ADDRESS" \
  --nofork --nopidfile &
pids+=($!)
for _ in $(seq 1 50); do
  [ -S "$XDG_RUNTIME_DIR/bus" ] && break
  sleep 0.1
done

# XFCE, one program at a time rather than through xfce4-session: there is no
# login to end, and nothing to restore that Chromium does not restore itself.
# Each one comes back if it exits (someone can kill it from a terminal now).
keep_running() {
  (
    while true; do
      "$@" || true
      echo "$1 exited; restarting in 2s" >&2
      sleep 2
    done
  ) &
  pids+=($!)
}
# Themes, fonts and shortcuts for every GTK program.
keep_running xfsettingsd --disable-wm-check
# The window manager. No compositor: software rendering, sent over VNC.
keep_running xfwm4 --compositor=off
# Chromium's first window should find a window manager, or it is not
# maximised: wait for xfwm4 to announce itself, but not for long.
for _ in $(seq 1 50); do
  xprop -root _NET_SUPPORTING_WM_CHECK 2>/dev/null | grep -q 'window id' && break
  sleep 0.1
done
# The panel (its first layout is desktop/xdg/xfce4/panel/default.xml) and the
# desktop with its icons and wallpaper. Neither is waited for.
keep_running xfce4-panel --disable-wm-check
keep_running xfdesktop --disable-wm-check

# The panel reserves its strip of the screen (a strut), which is what keeps a
# maximised window from reaching under it. After some changes of the screen's
# size the strip is not reserved again (seen going from 1920x1080 to
# 1280x800: the work area stayed the whole screen until another window
# opened). So after each change, if a panel along the top or bottom edge has
# no strip, state it for the panel: xfwm4 then refits the maximised windows.
reserve_panel_strip() {
  local id x=0 y=0 w=0 h=0 sh work_h
  id="$({ wmctrl -lx 2>/dev/null || true; } | awk 'tolower($3) ~ /xfce4-panel/ { print $1; exit }')"
  [ -n "$id" ] || return 0
  eval "$(xwininfo -id "$id" 2>/dev/null | awk '
    /Absolute upper-left X/ { print "x=" $4 }
    /Absolute upper-left Y/ { print "y=" $4 }
    /^ *Width:/ { print "w=" $2 }
    /^ *Height:/ { print "h=" $2 }')"
  IFS=, read -r _ sh < <(desktop_size)
  work_h="$(xprop -root _NET_WORKAREA 2>/dev/null | sed 's/.*= //; s/,//g' | awk '{ print $4 }')"
  # Only a horizontal panel on an edge, and only when nothing is reserved.
  [ "$w" -gt "$h" ] && [ "${work_h:-0}" -ge "$sh" ] || return 0
  if [ $((y + h)) -ge "$sh" ]; then
    xprop -id "$id" -f _NET_WM_STRUT_PARTIAL 32c -set _NET_WM_STRUT_PARTIAL \
      "0, 0, 0, $h, 0, 0, 0, 0, 0, 0, $x, $((x + w - 1))"
  elif [ "$y" -le 0 ]; then
    xprop -id "$id" -f _NET_WM_STRUT_PARTIAL 32c -set _NET_WM_STRUT_PARTIAL \
      "0, 0, $h, 0, 0, 0, 0, 0, $x, $((x + w - 1)), 0, 0"
  fi
}
watch_screen_size() {
  stdbuf -oL xev -root -event randr 2>/dev/null | while read -r line; do
    case "$line" in
      *RRScreenChangeNotify*)
        # The panel moves first; give it the time.
        sleep 2
        reserve_panel_strip || true
        ;;
    esac
  done
}

# The desktop's size now, as Chromium wants it ("W,H"): a viewer may have
# resized it since the start. This is the window's size before it is
# maximised.
desktop_size() {
  local size
  size="$(xdpyinfo -display :99 2>/dev/null | sed -n 's/^ *dimensions: *\([0-9]*\)x\([0-9]*\) pixels.*/\1,\2/p' | head -n 1)"
  echo "${size:-$SCREEN_W,$SCREEN_H}"
}
keep_running watch_screen_size

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
  ids="$(browser_windows)"
  for id in $ids; do
    wmctrl -i -r "$id" -b add,maximized_vert,maximized_horz 2>/dev/null || true
  done
}

# Keep Chromium alive: if someone closes the last window (it is one program
# among others on the desktop now, and can be closed like them) or it
# crashes, bring it back with the same profile. browser_execute needs it.
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
    maximise_browser_windows &
    # Chromium by its path: `chromium` on PATH is the desktop's command for a
    # window in this one (flake.nix).
    # shellcheck disable=SC2086
    "${CHROMIUM_BIN:-chromium}" \
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
