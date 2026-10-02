# The session's desktop (XFCE)

A session's display used to hold one thing: Chromium, maximised by openbox.
It is now an XFCE desktop with Chromium on it. This is what the browser
image (`images/browser/`) starts, why it is put together this way, and what
has only been checked under Docker.

## What is on it

| Part | Program | Started by |
|---|---|---|
| Window manager | `xfwm4`, compositor off | entrypoint, restarted if it exits |
| Panel | `xfce4-panel`: applications menu, launchers (terminal, files, browser, editor), open windows, clock | entrypoint, restarted |
| Desktop | `xfdesktop`: wallpaper and icons | entrypoint, restarted |
| Settings | `xfsettingsd` (theme, fonts, shortcuts), `xfconfd` (the settings store) | entrypoint; `xfconfd` through D-Bus |
| Session bus | `dbus-daemon --session` on `$XDG_RUNTIME_DIR/bus` | entrypoint, a core process |
| Browser | Chromium, as before | entrypoint, restarted |
| Terminal | `xfce4-terminal` | on demand |
| File manager | Thunar | on demand |
| Text editor | Mousepad | on demand |
| Image viewer | Ristretto | on demand |
| Run dialog, settings | `xfce4-appfinder` (Alt+F2), `xfce4-settings-manager` | on demand |

Theme and icons are Adwaita (GTK's built-in theme, `adwaita-icon-theme`),
fonts are the ones the image had (DejaVu, Noto, colour emoji). The image's
fontconfig now says what the generic families mean (`sans-serif` is DejaVu
Sans, `monospace` DejaVu Sans Mono): before, they resolved to a serif font,
in Chromium's own interface and in pages that ask for a generic family too.
Mousepad's text is still drawn in a serif font; the terminal's is fixed-width.

### What is left out, and why

- **`xfce4-session`.** It would add a log-out dialog, a saved session to
  restore, the lock command (`xflock4`) and autostart entries. A session
  has no login to return to, and Chromium restores its own tabs. Without
  it the menu has no "Log Out" and Ctrl+Alt+L and Ctrl+Alt+Del do nothing.
- **Screensaver, locker, power manager** (`xfce4-screensaver`,
  `xfce4-power-manager`, `light-locker`): a locked or blanked screen would
  strand an agent. The X server's own blanking is turned off too
  (`xset s off`).
- **Display manager, polkit agent, PulseAudio, notification daemon, update
  notifiers, gvfs, tumbler.** No login, nothing to authorise, no sound, and
  gvfs (trash, network places) wants FUSE. Thunar works on plain files;
  delete is delete. Thumbnails are not generated.
- **colord and xapp** in `xfce4-settings`, and **libsystemd** in the
  terminal's vte: 340 MB of scanner, printer, MATE and systemd files that
  nothing here uses. Both packages are rebuilt without them (`flake.nix`),
  which makes the image build in CI 12 to 14 minutes, from about 6.
- **A compositor.** `xfwm4 --compositor=off`, and off in the defaults: the
  display is software rendered and sent over VNC.
- **The accessibility bus** (`NO_AT_BRIDGE=1`).

## What stays as it was

- **The desktop follows the viewer.** Xvnc still resizes on request. xfwm4
  refits maximised windows when the screen changes size
  (`clientScreenResize` in its `client.c` calls `clientUpdateMaximizeSize`
  for every maximised window on GDK's `size-changed`), and the panel moves
  to the new bottom edge. One thing did not hold in the test: after most
  changes of size the panel's strip was not reserved again, and Chromium
  reached under the panel until another window opened. The panel sets its
  strut for the new size before xfwm4 has taken the new size in; xfwm4
  discards it and does not recompute the work area when the size arrives
  (xfwm4 4.20.0, `workspaceUpdateArea`). The entrypoint watches for size
  changes (`xev`) and, when no strip is in effect, removes the panel's strut
  and sets it again, which xfwm4 does act on. The smoke test resizes six
  times and checks the work area and Chromium each time.
- **Chromium is always there.** The restart loop is unchanged: close the
  last window, or kill it, and it is back in two seconds with the same
  profile. `browser_execute` needs it, so on this desktop Chromium is the
  one program that cannot be quit for good.
- **Chromium starts maximised.** openbox maximised every ordinary window,
  always. xfwm4 has no such rule, so the entrypoint maximises Chromium's
  windows once after each start (`wmctrl`); after that they are ordinary
  windows. Two differences follow: someone can unmaximise Chromium, and a
  popup a page opens keeps the size the page asked for.
- **The panel does not cover Chromium.** It reserves its strip (a strut),
  and a maximised window ends above it: the work area is the screen less
  30 pixels at the bottom.
- **Remote debugging** on 127.0.0.1:9222, **the clipboard** (Xvnc, `xclip`,
  `xsel`), and **the file chooser opening in Downloads** are unchanged.
- **`chromium` on the desktop is the session's browser.** The panel
  launcher, the menu, a link opened from another program and the command in
  a terminal all run a wrapper that hands the request to the running
  Chromium (a new window or tab, same profile). nixpkgs' own menu entry,
  which would start a second browser with an empty profile and no remote
  debugging, is not installed.

## HOME is on the session's disk

`HOME` is `/data/chrome/home`: a directory inside the volume the browser
container already has (`subPath: chrome`), next to Chromium's profile. So
the blueprint and the warm pool template did not change, and what a person
or an agent makes on the desktop survives sleep, stop and start like the
browser profile does:

- XFCE's settings (`~/.config/xfce4`), the panel's layout, GTK settings
  (`GSETTINGS_BACKEND=keyfile`, a file under `~/.config`);
- shell history and `~/.bashrc` (copied from `images/browser/desktop/bashrc`
  the first time);
- files made in the terminal or saved from the editor;
- caches (`~/.cache`: fontconfig's, so later starts skip building it).

`~/Downloads` is a link to `/data/chrome/Downloads`, the folder the session
page lists and Chromium downloads to, and `~/.config/user-dirs.dirs` names
it as the Downloads folder: Thunar, the GTK file chooser and the Files box
show the same files. The terminal and Thunar open in `HOME`.

Chromium ignores a directory it does not know in its profile directory. The
alternative, a second `subPath` for the home, needs the same change in
`deploy/gke/blueprint.yaml`, `deploy/base/blueprint.yaml` and
`deploy/gke/warmpool.yaml`, and gives nothing this does not.

Not kept: `/tmp` (the session bus socket, the X socket, `XDG_RUNTIME_DIR`),
and anything written elsewhere on the root filesystem.

## The terminal

bash (interactive, with readline) as the user `browser`, uid 1000, in
`$HOME`, `LANG=C.UTF-8`, `SHELL` and `/bin/bash` the same bash,
`/usr/bin/env` present. On `PATH`:

    coreutils findutils grep sed gawk diffutils less which file tree
    procps (ps, top, pkill) ncurses (clear, tput)
    tar gzip bzip2 xz zip unzip
    curl wget git (gitMinimal) ssh nano jq ripgrep
    python3 node
    xclip xsel xdotool wmctrl xprop xrandr xset xwininfo xdpyinfo

Python and Node are the interpreters websockify and the MCP server already
ran on, so they cost nothing. git is the largest addition (53 MB).

**Nothing can be installed system-wide.** There is no root, no sudo and no
package manager, and the image is a Nix store, not a distribution: there is
no `/lib`, `/usr/lib` or dynamic loader at the usual path, so a binary
downloaded from the internet that is not statically linked does not start.
What does work, into `HOME`: `python3 -m venv` (it gives a `pip`; the smoke
test checks that) and `pip install` of pure Python packages, `npm install` of pure JavaScript packages (`npm` is
part of Node), static binaries, scripts. Options for later, in order of
cost: add tools to `shell-tools` in `flake.nix`; ship the Nix package
manager with a store on the session disk; or build the image on a
distribution base instead of from scratch.

For a shell from outside the desktop (`kubectl exec`, `docker exec`), the
desktop's environment is in a file:

    kubectl exec -it <pod> -c browser -- /bin/bash
    . /tmp/runtime/session-env

## Under gVisor, as uid 1000, with a read-only store

What each part needs, and where it comes from:

| Need | How |
|---|---|
| D-Bus session bus | `dbus-daemon` with nixpkgs' `session.conf`; services are found in `XDG_DATA_DIRS` (`xfconfd`, Thunar), each with an absolute store path |
| `XDG_RUNTIME_DIR` | `/tmp/runtime`, mode 700, as before |
| `/etc/machine-id` | a fixed one in the image (D-Bus asks for it) |
| `/etc/passwd` entry | `browser`, home `/data/chrome/home`, shell `/bin/bash` |
| GSettings schemas, pixbuf loaders, GIO modules | each program is nixpkgs' wrapped one (`wrapGAppsHook3`) |
| Menu entries, icons, MIME database, Xfce defaults | one `buildEnv` (`desktop` in `flake.nix`) behind `XDG_DATA_DIRS` and `XDG_CONFIG_DIRS`, with `images/browser/desktop/` in front of it |
| GSettings backend | `keyfile` (no dconf daemon) |
| Locale | `C.UTF-8`, built into glibc: no locale archive |
| Open-file limit | lowered as before |

## Memory, and how many sessions fit a node

Measured by the smoke test on a GitHub runner (Docker, runc; run
37038273254), from `/proc/<pid>/smaps_rollup` and the container's cgroup.
PSS divides shared pages among the processes that map them, and most of
XFCE's pages are GTK's, which Chromium maps too.

| | RSS, MiB | PSS, MiB |
|---|---|---|
| xfwm4 | 40 | 16 |
| xfce4-panel | 40 | 15 |
| xfdesktop | 55 | 26 |
| xfsettingsd | 33 | 15 |
| xfconfd and dbus-daemon | 10 | 2 |
| Thunar, started in the background by xfdesktop | 28 | 9 |
| **The desktop, idle** | **208** | **85** |
| A terminal, Thunar's window and Mousepad open | 133 | 49 |
| Chromium on about:blank, 12 processes | 1244 | 436 |
| Xvnc | 50 | 36 |
| browser-mcp (node) | 108 | 102 |
| websockify | 41 | 35 |

The container's cgroup counted 369 MiB with the desktop idle and Chromium on
`about:blank` (423 in another run), and 445 MiB with the three programs
open and `desktop_execute` used once. So the desktop costs about 85 MiB a
session when nobody uses it, and about 40 MiB more with a terminal, a file
manager and an editor open.

**The requests and limits do not change.** The browser container requests
1Gi and is limited to 2Gi. Idle use stays well under the request, so the
packing arithmetic in `deploy/gke/warmpool.yaml` holds as written: 1280Mi a
session pod, nine pods on a 16 GB node, seven warm. What the desktop takes
is headroom under the limit: a session whose Chromium is near 2Gi today
reaches it about 100 MiB sooner. What the terminal can add is unbounded by
this change (a `pip install`, a `git clone`, a Python process) and is held
by the same 2Gi limit: the container is killed and restarted when it is
passed, as before. Raise the limit, not the request, if that happens in
practice; the limit does not affect packing.

These are runc's numbers. gVisor's own overhead per sandbox, and whether
its accounting of file-backed memory differs, are not measured (below).

## Image size and start

From `nix path-info -r --store https://cache.nixos.org` for the pinned
nixpkgs, counting store paths the image did not have before:

| | Unpacked, MiB | Compressed (cache's xz), MiB |
|---|---|---|
| The image before (without the npm package) | 2672 | 835 |
| Added, as packaged by nixpkgs | 492 | 118 |
| Added, after the two rebuilds | about 160 | about 50 |

The largest additions: git (53), the panel (8.5), Thunar (7.6),
xfce4-settings (7.3), libjxl for the image viewer (6.9), ripgrep (6.7),
xfdesktop (5). GTK 3, the icon theme, Python, Node, D-Bus and OpenSSH were
already in Chromium's closure. The built image is 3.14 GB as Docker counts
it. So a cold pull grows by about 6 %; registry layers are gzip, somewhat
larger than xz.

Start, in the smoke test (Docker, image already on the node), over four
runs: the MCP server answered `/healthz` 2.9 to 5.0 s after the container
started, Chromium's debugging port 0.2 s later, and the panel had its
window 0.1 s after that. There is no measurement of the image before XFCE
on the same runners to compare with. The entrypoint
waits for xfwm4 before starting Chromium (at most 5 s; it took about half
a second), so that Chromium's first window is maximised.

## What a terminal changes for security

Until now the only code in the browser container was the image's. A
terminal runs anything, as the session's user, for whoever holds the
session: its owner at the live view, and the owner's agent through
`desktop_execute`. Nobody else gains anything. What that code can and
cannot reach:

- **Still contained by gVisor and the pod's settings.** uid 1000, no
  capabilities, no privilege escalation, no service account token, the
  sandbox's own kernel. No root: nothing in the image is setuid.
- **Still contained by the NetworkPolicy** (`deploy/base/networkpolicy.yaml`):
  out to the internet and DNS only, not to the cluster, the nodes or the
  metadata address; in from the backend only. Chromium could already reach
  the same destinations; a terminal adds every protocol (ssh, raw TCP).
  Abuse of the egress (scanning, mail, mining within the CPU limit) is as
  possible as from any machine with a shell, and nothing here limits it
  beyond the pod's CPU and memory limits.
- **Everything inside the pod is one trust domain, and now literally.**
  A shell can read Chromium's profile on disk (cookies, saved passwords:
  `--password-store=basic` keeps them unencrypted), drive Chromium over
  127.0.0.1:9222, and call the browser's MCP server on 8081 and mcp-js on
  127.0.0.1:8080 directly. The last two skip what mcp-js enforces between
  an agent and the browser: its Rego policies (`mcp_tools.rego`,
  `filesystem.rego`) and, through them, a SessionPolicy. `callers.js` keeps
  web pages out of port 8081 by their headers; `curl` sends whatever
  headers it likes. So a policy that restricts what an agent may do in a
  session can be walked around by an agent that is allowed
  `desktop_execute`: it opens a terminal. A JSON policy (version 1) refuses
  `desktop_execute` altogether, and holds. A Rego policy that allows
  `desktop_execute` while restricting `browser_execute` (hosts, operations)
  restricts nothing any more: allowing the desktop now means allowing a
  shell. Policies bind the agent's calls, never the person at the live
  view, who could always do by hand what a policy forbids the agent.
- **mcp-js's own files stay out of reach.** `/data/memory` and `/data/mcp`
  are mounted in the other container only.
- **It can break its own session:** kill Xvnc or the entrypoint (the
  container restarts), fill the 5Gi disk, use the memory up to the limit.

## Snapshot and restore

A GKE Pod Snapshot restores the processes as they were, on a new pod.

- **Sockets.** The session bus and X are Unix sockets inside the container,
  restored with the processes that hold them.
- **Hostname and address.** Nothing on the desktop keeps them: X runs with
  `-ac` and no xauth cookie, and there is no session manager (xfce4-session
  would have an ICE socket named after the host).
- **The clock.** The panel's clock redraws on its next tick, at most a
  minute late.
- **The screen size** stays what it was until a viewer connects, as before.

None of this was run on GKE with XFCE; see below.

## The smoke test

`images/browser/test/desktop-image-smoke.sh <image>` starts the built image
the way a session pod does (uid 1000, all capabilities dropped,
`SESSION_MODE=1`, a volume at `/data/chrome`) and checks, on its display:
Xvnc, the session bus, `xfconfd` with the image's defaults, xfwm4, the
panel, the desktop, the settings daemon; Chromium's window, maximised above
the panel, and its debugging port; a terminal with a working shell, Thunar
and Mousepad opening; `desktop_execute` typing into a terminal and grabbing
the screen; the terminal's tools; the `chromium` command; the clipboard;
three resizes; Chromium coming back after being closed; no locker,
screensaver or session manager; then a second container on the same volume
finding the file, the setting and the shell configuration of the first. It
prints how long the start took and what each part uses in memory, and saves
screenshots.

CI runs it on every pull request that builds the browser image
(`.github/workflows/images.yml`), and uploads the screenshots and logs as
the `desktop-smoke` artifact. It does not run on main's publishing build.

## Not verified

Docker on a GitHub runner is runc, not gVisor, and not GKE:

1. XFCE under gVisor (`runsc`): D-Bus over Unix sockets with credential
   passing (`SO_PEERCRED`), ptys for the terminal, inotify (xfdesktop and
   Thunar watch directories), and memory use as gVisor counts it.
2. `HOME` on the Persistent Disk through `fsGroup: 1000`: tested on a
   directory owned by uid 1000.
3. A Pod Snapshot taken with XFCE running, and its restore.
4. The warm pool: seven idle desktops on one node, and the start time of a
   session pod with the larger image.
5. An existing session's disk (a profile made under openbox) starting on
   this image: its saved windows are maximised by the entrypoint, but that
   was run on a new profile only.
