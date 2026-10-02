#!/usr/bin/env bash
# The built image, started the way a session pod starts it (uid 1000, no
# capabilities, SESSION_MODE=1, a volume at /data/chrome), with its XFCE
# desktop checked on the real display: the image build of CI runs this
# (.github/workflows/images.yml). Under Docker's runc, not gVisor.
#
#   desktop-image-smoke.sh <image> [<directory for screenshots and logs>]
#
# Needs docker and sudo (the volume has to belong to uid 1000).
set -euo pipefail

image="${1:?usage: desktop-image-smoke.sh <image> [<output directory>]}"
out="${2:-desktop-smoke-out}"
name="desktop-smoke-$$"
mkdir -p "$out"
data="$(mktemp -d)"
sudo chown 1000:1000 "$data"

finish() {
  docker logs "$name" >"$out/container-last.log" 2>&1 || true
  docker rm -f "$name" >/dev/null 2>&1 || true
  sudo rm -rf "$data"
}
trap finish EXIT

start() {
  docker run -d --name "$name" --user 1000:1000 --cap-drop ALL \
    --security-opt no-new-privileges --shm-size 1g \
    -e SESSION_MODE=1 -e DATA_DIR=/data -e TAB_STATE_FILE=/data/chrome/browserjs-tabs.json \
    -v "$data:/data/chrome" "$image" >/dev/null
  started="$(date +%s.%N)"
}

# Seconds since the container started, when a command inside it first succeeds.
seconds_until() {
  local what="$1"
  shift
  for _ in $(seq 1 600); do
    if docker exec "$name" /bin/bash -c "$*" >/dev/null 2>&1; then
      echo "info $what after $(awk -v a="$(date +%s.%N)" -v b="$started" 'BEGIN { printf "%.1f", a - b }') s"
      return 0
    fi
    sleep 0.2
  done
  echo "FAIL $what: not within 120 s"
  docker logs "$name" 2>&1 | tail -n 80
  return 1
}

# The checks run inside the container, with the environment the desktop has.
inside() {
  docker exec -i "$name" /bin/bash -s -- "$@" <<'INSIDE'
set -uo pipefail
# shellcheck disable=SC1091
. /tmp/runtime/session-env
phase="$1"
# Where the desktop's own programs run.
cd "$HOME" || exit 1
failed=0
ok() { echo "ok   $*"; }
bad() {
  echo "FAIL $*"
  failed=1
}
check() {
  local what="$1"
  shift
  if "$@" >/dev/null 2>&1; then ok "$what"; else bad "$what"; fi
}
# wait_for <seconds> <command...>
wait_for() {
  local n="$1"
  shift
  for _ in $(seq 1 $((n * 5))); do
    "$@" >/dev/null 2>&1 && return 0
    sleep 0.2
  done
  return 1
}

# A program of the image, by the name it was started with (nixpkgs wraps
# them: the process itself is named .<name>-wrapped).
running() { pgrep -f "(^|/)\\.?$1(-wrapped)?( |\$)"; }
window() { wmctrl -lx | awk -v c="$1" 'tolower($3) ~ c { print $1; exit }'; }
has_window() { [ -n "$(window "$1")" ]; }
screen_size() { xdpyinfo | sed -n 's/^ *dimensions: *\([0-9x]*\) pixels.*/\1/p'; }
workarea() { xprop -root _NET_WORKAREA | sed 's/.*= //; s/,//g' | awk '{ print $1, $2, $3, $4 }'; }
maximised() {
  local state
  state="$(xprop -id "$1" _NET_WM_STATE)"
  [[ "$state" == *MAXIMIZED_VERT* && "$state" == *MAXIMIZED_HORZ* ]]
}
# The window is as wide as the work area, about as tall, and ends above the
# panel.
fills_workarea() {
  local wx wy ww wh x=0 y=0 w=0 h=0
  read -r wx wy ww wh < <(workarea)
  eval "$(xwininfo -id "$1" | awk '
    /Absolute upper-left X/ { print "x=" $4 }
    /Absolute upper-left Y/ { print "y=" $4 }
    /^ *Width:/ { print "w=" $2 }
    /^ *Height:/ { print "h=" $2 }')"
  [ "$w" -eq "$ww" ] && [ "$x" -eq "$wx" ] && [ $((y + h)) -le $((wy + wh)) ] && [ "$h" -ge $((wh - 60)) ]
}
browser_ok() {
  local id
  id="$(window chromium)"
  [ -n "$id" ] && maximised "$id" && fills_workarea "$id"
}
cdp() { curl -fsS --max-time 2 http://127.0.0.1:9222/json/version; }
pages() { curl -fsS --max-time 2 http://127.0.0.1:9222/json/list | grep -c '"type": "page"'; }
main_browser() {
  local pid
  for pid in $(pgrep -f -- "--user-data-dir=/data/chrome"); do
    tr '\0' ' ' <"/proc/$pid/cmdline" 2>/dev/null | grep -q -- '--type=' || echo "$pid"
  done
}

# Memory of the processes whose command line matches, in MiB: resident, and
# proportional (shared pages divided among their users).
memory() {
  local label="$1" pattern="$2" pid n=0 rss=0 pss=0 r p
  for pid in $(pgrep -f -- "$pattern"); do
    r="$(awk '/^Rss:/ { print $2 }' "/proc/$pid/smaps_rollup" 2>/dev/null)"
    p="$(awk '/^Pss:/ { print $2 }' "/proc/$pid/smaps_rollup" 2>/dev/null)"
    [ -n "$r" ] || continue
    n=$((n + 1))
    rss=$((rss + r))
    pss=$((pss + p))
  done
  printf 'mem  %-34s %2d processes  rss %5d MiB  pss %5d MiB\n' "$label" "$n" $((rss / 1024)) $((pss / 1024))
}
memory_table() {
  echo "mem  --- $1 ---"
  memory "xfwm4" '(^|/)\.?xfwm4'
  memory "xfce4-panel (and its plugins)" 'xfce4-panel|panel/wrapper'
  memory "xfdesktop" '(^|/)\.?xfdesktop'
  memory "xfsettingsd" '(^|/)\.?xfsettingsd'
  memory "xfconfd + dbus-daemon" 'xfconfd|dbus-daemon'
  memory "desktop, all of the above" 'xfwm4|xfce4-panel|panel/wrapper|xfdesktop|xfsettingsd|xfconfd|dbus-daemon'
  memory "terminal, Thunar, Mousepad" 'xfce4-terminal|[Tt]hunar|mousepad'
  memory "Chromium, every process" 'chromium'
  memory "Xvnc" 'Xvnc'
  memory "browser-mcp (node)" 'browser-mcp'
  memory "websockify" 'websockify'
  awk '/^(MemTotal|MemAvailable):/ { printf "mem  host %s %d MiB\n", $1, $2 / 1024 }' /proc/meminfo
  if [ -r /sys/fs/cgroup/memory.current ]; then
    echo "mem  container, as its cgroup counts it: $(($(cat /sys/fs/cgroup/memory.current) / 1048576)) MiB"
  fi
}

# The MCP server's desktop_execute tool (nut.js), as mcp-js calls it.
desktop_execute() {
  curl -fsS --max-time 30 -X POST http://127.0.0.1:8081/mcp \
    -H 'content-type: application/json' -H 'accept: application/json, text/event-stream' \
    -d "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\",\"params\":{\"name\":\"desktop_execute\",\"arguments\":$1}}"
}
# The screen as a PNG, grabbed with it.
screenshot() {
  if desktop_execute '{"operations":[{"type":"screen.grab"}]}' 2>/dev/null |
    jq -er '.result.content[1].data' 2>/dev/null | base64 -d >"/tmp/$1.png" &&
    [ "$(head -c 4 "/tmp/$1.png" | tail -c 3)" = PNG ]; then
    ok "screenshot $1.png ($(screen_size), $(wc -c <"/tmp/$1.png") bytes)"
  else
    bad "screenshot $1.png"
  fi
}

# --- what must hold every time the image starts -----------------------------
check "the MCP server answers /healthz" wait_for 120 curl -fsS --max-time 2 http://127.0.0.1:8081/healthz
check "Xvnc: the display answers" xdpyinfo
check "the session bus answers" dbus-send --session --print-reply --dest=org.freedesktop.DBus \
  /org/freedesktop/DBus org.freedesktop.DBus.ListNames
check "xfconfd starts through the bus and has the image's defaults (no compositor)" \
  test "$(xfconf-query -c xfwm4 -p /general/use_compositing 2>&1)" = false
for program in xfwm4 xfce4-panel xfdesktop xfsettingsd xfconfd; do
  check "$program is running" wait_for 30 running "$program"
done
check "the window manager is Xfwm4" bash -c 'wmctrl -m | grep -qi "Name: xfwm4"'
check "the panel has a window" wait_for 30 has_window xfce4-panel
check "the desktop has a window" wait_for 30 has_window xfdesktop
read -r _ _ _ work_h < <(workarea)
screen_h="$(screen_size)"
screen_h="${screen_h#*x}"
if [ "$work_h" -lt "$screen_h" ]; then
  ok "the panel reserves its strip: work area $(workarea), screen $(screen_size)"
else
  bad "the panel reserves nothing: work area $(workarea), screen $(screen_size)"
fi
check "Chromium answers on the remote debugging port" wait_for 60 cdp
check "Chromium has a window" wait_for 60 has_window chromium
check "Chromium's window is maximised and ends above the panel" wait_for 30 browser_ok
[ "$(main_browser | wc -l)" = 1 ] && ok "one Chromium" || bad "main Chromium processes: $(main_browser | tr '\n' ' ')"
check "HOME is on the volume" test "$HOME" = /data/chrome/home -a -w "$HOME"
check "Downloads in HOME is the folder of the session's files" test "$HOME/Downloads" -ef "$FILES_DIR"
check "/tmp is writable" touch /tmp/desktop-smoke-touch
# What the panel starts (a terminal, the file manager) starts where it is.
panel_in_home() { [ "$(readlink "/proc/$(running xfce4-panel | head -n 1)/cwd")" = "$HOME" ]; }
check "the panel, and so what it launches, runs in HOME" panel_in_home

if [ "$phase" = restarted ]; then
  # A second container on the same volume: what the first one left.
  check "a file made in the terminal is still in HOME" test "$(cat "$HOME/smoke-file" 2>/dev/null)" = made-in-a-terminal
  check "an XFCE setting is still set" test "$(xfconf-query -c desktop-smoke -p /persisted 2>&1)" = yes
  check ".bashrc is still there" test -s "$HOME/.bashrc"
  echo "info HOME: $(ls -A "$HOME" | tr '\n' ' ')"
  screenshot restarted
  exit "$failed"
fi

# --- the first start ---------------------------------------------------------
sleep 5
memory_table "idle: the desktop and Chromium on about:blank"
screenshot desktop

# The terminal, and a shell in it.
xfce4-terminal --title smoke-terminal -x bash -ic \
  'printf "%s|%s|%s|%s\n" "$HOME" "$PWD" "$LANG" "$SHELL" >/tmp/smoke-shell; echo made-in-a-terminal >"$HOME/smoke-file"; sleep 600' \
  >/tmp/smoke-terminal.log 2>&1 &
check "xfce4-terminal maps a window" wait_for 30 has_window xfce4-terminal
check "a shell runs in it" wait_for 15 test -s /tmp/smoke-shell
shell="$(cat /tmp/smoke-shell 2>/dev/null)"
case "$shell" in
  "/data/chrome/home|/data/chrome/home|C.UTF-8|"*/bin/bash) ok "its HOME, directory, locale and shell: $shell" ;;
  *) bad "its HOME, directory, locale and shell: $shell" ;;
esac
thunar "$HOME" >/tmp/smoke-thunar.log 2>&1 &
check "thunar maps a window" wait_for 30 has_window thunar
mousepad /tmp/smoke-shell >/tmp/smoke-mousepad.log 2>&1 &
check "mousepad maps a window" wait_for 30 has_window mousepad
sleep 3
screenshot applications
memory_table "with a terminal, Thunar and Mousepad open"

# desktop_execute (nut.js, through the MCP server) on this desktop: keys
# typed into a terminal arrive, and the screen can be grabbed.
xfce4-terminal --title smoke-typing -x bash -c \
  'IFS= read -r line; printf %s "$line" >/tmp/smoke-typed; sleep 600' >>/tmp/smoke-terminal.log 2>&1 &
typing_window() { xdotool search --name '^smoke-typing$' | head -n 1 | grep .; }
if wait_for 20 typing_window; then
  xdotool windowactivate --sync "$(typing_window)" 2>/dev/null || true
  sleep 1
  desktop_execute '{"operations":[
    {"type":"keyboard.type","params":{"text":"typed by nut.js 123"}},
    {"type":"keyboard.type","params":{"keys":["Enter"]}},
    {"type":"getActiveWindow"},
    {"type":"getWindows"},
    {"type":"screen.grab"}]}' >/tmp/smoke-desktop-execute.json 2>/tmp/smoke-desktop-execute.err
  check "desktop_execute types into the focused terminal" wait_for 10 test "$(cat /tmp/smoke-typed 2>/dev/null)" = 'typed by nut.js 123'
  if jq -er '.result.content[1].data' /tmp/smoke-desktop-execute.json 2>/dev/null | base64 -d >/tmp/desktop-execute.png &&
    [ "$(head -c 4 /tmp/desktop-execute.png | tail -c 3)" = PNG ]; then
    ok "desktop_execute grabs the screen ($(wc -c </tmp/desktop-execute.png) bytes of PNG)"
  else
    bad "desktop_execute screen.grab: $(head -c 600 /tmp/smoke-desktop-execute.json /tmp/smoke-desktop-execute.err 2>/dev/null)"
  fi
  echo "info desktop_execute getActiveWindow: $(jq -r '.result.content[0].text | fromjson | .results[2].result | tostring' /tmp/smoke-desktop-execute.json 2>/dev/null | head -c 300)"
  echo "info desktop_execute getWindows titles: $(jq -r '.result.content[0].text | fromjson | .results[3].result | map(.title) | map(select(. != "")) | tostring' /tmp/smoke-desktop-execute.json 2>/dev/null | head -c 600)"
else
  bad "a second terminal window did not appear"
fi

# The terminal's tools.
missing=""
for tool in bash ls cp find grep sed awk diff less which file tree ps top clear tar gzip bzip2 xz zip unzip \
  curl wget git ssh nano jq rg python3 node xclip xdotool wmctrl; do
  command -v "$tool" >/dev/null 2>&1 || missing="$missing $tool"
done
[ -z "$missing" ] && ok "the terminal's tools are on PATH" || bad "not on PATH:$missing"
check "/bin/bash and /usr/bin/env exist" test -x /bin/bash -a -x /usr/bin/env
check "a script with an env first line runs" bash -c 'printf "#!/usr/bin/env python3\nprint(1)\n" >/tmp/smoke.py && chmod +x /tmp/smoke.py && /tmp/smoke.py'
check "Python writes UTF-8" test "$(python3 -c 'import sys; print(sys.stdout.encoding)')" = utf-8
check "git works" bash -c 'cd /tmp && rm -rf smoke-repo && git init -q smoke-repo && cd smoke-repo && git -c user.name=s -c user.email=s@s commit -q --allow-empty -m x'
check "whoami knows the user" test "$(whoami)" = browser
# These depend on the network and on what the image can do at run time:
# reported, not required.
curl -fsS --max-time 20 -o /dev/null https://example.com && echo "info curl reaches https://example.com (the CA bundle works)" || echo "info curl could not reach https://example.com"
if python3 -m venv /tmp/smoke-venv >/tmp/smoke-venv.log 2>&1 && /tmp/smoke-venv/bin/pip --version >/dev/null 2>&1; then
  echo "info python3 -m venv gives a pip: $(/tmp/smoke-venv/bin/pip --version | cut -d' ' -f1-2)"
else
  echo "info python3 -m venv gives no pip: $(tail -n 2 /tmp/smoke-venv.log | tr '\n' ' ')"
fi

# `chromium` on the desktop is a tab or window of the session's browser.
before="$(pages)"
chromium 'about:blank#desktop-smoke' >/tmp/smoke-chromium.log 2>&1 &
more_pages() { [ "$(pages)" -gt "$before" ]; }
check "the chromium command opens a page in the session's browser" wait_for 20 more_pages
sleep 2
[ "$(main_browser | wc -l)" = 1 ] && ok "and starts no second browser" || bad "main Chromium processes: $(main_browser | tr '\n' ' ')"

# The clipboard, as clipboard.js uses it.
printf 'smoke clip' | xclip -selection clipboard -i >/dev/null 2>&1
check "xclip sets and reads the clipboard" test "$(xclip -selection clipboard -o 2>/dev/null)" = 'smoke clip'

# A viewer resizes the desktop: Chromium stays maximised, above the panel.
for size in 1024x768 1920x1080 1280x800 1280x1024 800x600 1280x800; do
  xrandr -s "$size" >/dev/null 2>&1
  resized() { [ "$(screen_size)" = "$size" ]; }
  # The panel follows first, and takes its strip of the new screen.
  strip() {
    local h
    read -r _ _ _ h < <(workarea)
    [ "$h" -lt "${size#*x}" ]
  }
  wait_for 10 resized
  sleep 1
  echo "info one second after the resize to $size: work area $(workarea), panel strut $(xprop -id "$(window xfce4-panel)" _NET_WM_STRUT_PARTIAL | sed 's/.*= //')"
  if resized && wait_for 15 strip && wait_for 15 browser_ok; then
    ok "after a resize to $size Chromium is maximised in the work area ($(workarea))"
  else
    bad "after a resize to $size: screen $(screen_size), work area $(workarea), Chromium $(xwininfo -id "$(window chromium)" | grep -E 'Absolute|Width|Height' | tr -s ' \n' ' '), panel $(xwininfo -id "$(window xfce4-panel)" | grep -E 'Absolute|Width|Height' | tr -s ' \n' ' ') $(xprop -id "$(window xfce4-panel)" _NET_WM_STRUT_PARTIAL _NET_WM_STRUT | tr '\n' ' ')"
  fi
done

# Closing Chromium brings it back, maximised.
old="$(main_browser)"
kill -TERM $old
new_browser() {
  local pid
  pid="$(main_browser)"
  [ -n "$pid" ] && [ "$pid" != "$old" ] && cdp
}
check "Chromium comes back after it is closed" wait_for 40 new_browser
check "and is maximised again" wait_for 40 browser_ok

# Nothing that could lock, blank or end the session.
lockers="$(pgrep -fl 'screensaver|xflock|light-locker|xscreensaver|xfce4-session|xfce4-power' || true)"
[ -z "$lockers" ] && ok "no screensaver, locker, power manager or session manager is running" || bad "running: $lockers"
present=""
for program in xflock4 xfce4-session xfce4-session-logout xfce4-screensaver xfce4-screensaver-command xscreensaver light-locker xfce4-power-manager; do
  command -v "$program" >/dev/null 2>&1 && present="$present $program"
done
[ -z "$present" ] && ok "and none is in the image's PATH" || bad "on PATH:$present"
check "the X server's own screen blanking is off" bash -c 'xset q | grep -q "timeout: *0 "'
echo "info windows: $(wmctrl -lx | awk '{ print $3 }' | sort | uniq -c | tr -s ' \n' ' ')"

# For the restarted container to find.
xfconf-query -c desktop-smoke -p /persisted -n -t string -s yes
check "a file made in the terminal is in HOME" test -s "$HOME/smoke-file"
sleep 2
screenshot final
exit "$failed"
INSIDE
}

copy_out() {
  local file
  for file in "$@"; do
    docker cp "$name:/tmp/$file" "$out/$file" 2>/dev/null || true
  done
}

status=0

echo "== first start =="
start
seconds_until "the environment file exists" '[ -s /tmp/runtime/session-env ]'
seconds_until "the MCP server answers /healthz" '. /tmp/runtime/session-env; curl -fsS --max-time 1 http://127.0.0.1:8081/healthz'
seconds_until "Chromium answers on the remote debugging port" '. /tmp/runtime/session-env; curl -fsS --max-time 1 http://127.0.0.1:9222/json/version'
seconds_until "the panel has a window" '. /tmp/runtime/session-env; wmctrl -lx | grep -qi xfce4-panel'
inside first || status=1
copy_out desktop.png applications.png final.png desktop-execute.png
docker logs "$name" >"$out/container-first.log" 2>&1 || true
echo "== stop =="
docker stop -t 30 "$name" >/dev/null
docker rm "$name" >/dev/null

echo "== a second container on the same volume =="
start
seconds_until "the MCP server answers /healthz" '. /tmp/runtime/session-env; curl -fsS --max-time 1 http://127.0.0.1:8081/healthz'
seconds_until "the panel has a window" '. /tmp/runtime/session-env; wmctrl -lx | grep -qi xfce4-panel'
inside restarted || status=1
copy_out restarted.png

if [ "$status" != 0 ]; then
  echo "== container log =="
  docker logs "$name" 2>&1 | tail -n 150
  echo "desktop smoke test FAILED"
  exit 1
fi
echo "desktop smoke test passed"
