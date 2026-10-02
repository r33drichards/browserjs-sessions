{
  description = "Persistent, VNC-viewable Chromium exposed as MCP behind mcp-js";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
  # Commands for run_js. Pinned to the commit of
  # https://github.com/r33drichards/mcp-exec/pull/7 (exec takes bin and args,
  # the kill tool); move to master once it is merged.
  inputs.mcp-exec = {
    url = "github:r33drichards/mcp-exec/a0688bf628e6b5662431c8e990b69e9cbfcb7ef3";
    flake = false;
  };

  outputs =
    { self, nixpkgs, mcp-exec }:
    let
      linuxSystems = [
        "x86_64-linux"
        "aarch64-linux"
      ];
      forLinux = f: nixpkgs.lib.genAttrs linuxSystems (system: f nixpkgs.legacyPackages.${system});
    in
    {
      packages = forLinux (
        pkgs:
        let
          browser-mcp = pkgs.buildNpmPackage {
            pname = "browser-mcp";
            version = "2.0.0";
            src = ./browser;
            npmDeps = pkgs.importNpmLock { npmRoot = ./browser; };
            npmConfigHook = pkgs.importNpmLock.npmConfigHook;
            dontNpmBuild = true;
            # puppeteer-core never downloads a browser; skip any install hooks.
            npmFlags = [ "--ignore-scripts" ];
            nativeBuildInputs = [
              pkgs.makeWrapper
              pkgs.patchelf
            ];
            # nut.js (the desktop_execute tool) drives X through libnut, a
            # native addon that npm delivers already built, for x86_64 only,
            # against the system's libX11 and libXtst: point it at Nix's. On
            # another architecture it cannot load, and the tool says so. The
            # builds for other systems and clipboardy's own copy of xsel
            # (the image has a working one on PATH) are dropped.
            postInstall = ''
              modules=$out/lib/node_modules/browser-mcp/node_modules
              rm -rf "$modules"/@nut-tree-fork/libnut-{darwin,win32}/build "$modules"/clipboardy/fallbacks
              patchelf --set-rpath ${
                pkgs.lib.makeLibraryPath [
                  pkgs.libx11
                  pkgs.libxtst
                  pkgs.stdenv.cc.cc.lib
                ]
              } "$modules"/@nut-tree-fork/libnut-linux/build/Release/libnut.node
            '';
            postFixup = ''
              wrapProgram "$out/bin/browser-mcp" --prefix PATH : ${pkgs.nodejs_22}/bin
            '';
          };

          # Built from its own package expression and Cargo.lock.
          mcp-exec-pkg = pkgs.callPackage "${mcp-exec}/nix/package.nix" { };

          # Only the Xvnc server out of TigerVNC: the package also carries the
          # viewer and its toolkit, which the image has no use for.
          xvnc = pkgs.runCommand "xvnc-${pkgs.tigervnc.version}" { } ''
            mkdir -p $out/bin
            cp ${pkgs.tigervnc}/bin/Xvnc $out/bin/Xvnc
          '';

          # The XFCE desktop (docs/desktop.md). There is no xfce4-session: the
          # entrypoint starts the window manager, panel, desktop and settings
          # daemon itself, so nothing can log out, lock the screen or restore
          # a saved session. Each program is nixpkgs' wrapped one, which
          # brings its own GSettings schemas, pixbuf loaders and GIO modules.
          #
          # Two rebuilds keep large, unused dependencies out of the image:
          # the terminal's vte without libsystemd (62 MB), and the settings
          # daemon without colord and xapp (282 MB: scanner and printer
          # libraries, and parts of MATE).
          xfce4-terminal = pkgs.xfce4-terminal.override {
            vte = pkgs.vte.override { systemdSupport = false; };
          };
          xfce4-settings = pkgs.xfce4-settings.override {
            withColord = false;
            xapp = null;
          };

          # `chromium`, for everything on the desktop that opens a browser
          # (the panel launcher, the menu, a link clicked in another program,
          # the command typed in a terminal): a window or tab in the
          # session's own Chromium, never a second browser with another
          # profile and no remote debugging. Chromium hands the request to
          # the running instance of the same profile and exits. While the
          # entrypoint is restarting Chromium there is none: wait for it.
          session-chromium = pkgs.writeShellScriptBin "chromium" ''
            profile="''${BROWSER_PROFILE_DIR:-/data/chrome}"
            for _ in $(${pkgs.coreutils}/bin/seq 1 100); do
              [ -L "$profile/SingletonLock" ] && break
              ${pkgs.coreutils}/bin/sleep 0.2
            done
            if [ ! -L "$profile/SingletonLock" ]; then
              echo "chromium: the session's browser is not running" >&2
              exit 1
            fi
            exec ${pkgs.chromium}/bin/chromium --no-sandbox --user-data-dir="$profile" "$@"
          '';

          # What the desktop looks up by name: programs, their menu entries
          # and icons, D-Bus services (xfconfd, Thunar), the MIME database,
          # Xfce's default settings. One tree, like a system profile, behind
          # XDG_DATA_DIRS and XDG_CONFIG_DIRS; ./desktop goes in front of it.
          desktop = pkgs.buildEnv {
            name = "xfce-desktop";
            paths = [
              pkgs.xfwm4
              pkgs.xfce4-panel
              pkgs.xfdesktop
              xfce4-settings
              pkgs.xfconf
              pkgs.thunar
              xfce4-terminal
              pkgs.mousepad
              pkgs.ristretto
              pkgs.xfce4-appfinder
              pkgs.xfce4-exo
              pkgs.garcon
              pkgs.libxfce4ui
              pkgs.adwaita-icon-theme
              pkgs.hicolor-icon-theme
              pkgs.shared-mime-info
              # Chromium's icon, for the launcher in ./desktop (its own menu
              # entry would start a browser outside the session's profile).
              (pkgs.runCommand "chromium-icons" { } ''
                mkdir -p $out/share
                ln -s ${pkgs.chromium}/share/icons $out/share/icons
              '')
            ];
            pathsToLink = [
              "/bin"
              "/etc/xdg"
              "/share"
            ];
            ignoreCollisions = true;
            # The caches the packages bring describe one package each.
            postBuild = ''
              if [ -d $out/share/applications ] && [ ! -L $out/share/applications ]; then
                rm -f $out/share/applications/mimeinfo.cache
                ${pkgs.desktop-file-utils}/bin/update-desktop-database $out/share/applications
              fi
              if [ -d $out/share/icons/hicolor ] && [ ! -L $out/share/icons/hicolor ]; then
                rm -f $out/share/icons/hicolor/icon-theme.cache
                ${pkgs.gtk3.out}/bin/gtk-update-icon-cache -f -t $out/share/icons/hicolor
              fi
            '';
          };

          # What a terminal on the desktop has to work with. Python and Node
          # are the ones websockify and the MCP server already run on.
          shell-tools = [
            pkgs.bashInteractive
            pkgs.coreutils
            pkgs.findutils
            pkgs.gnugrep
            pkgs.gnused
            pkgs.gawk
            pkgs.diffutils
            pkgs.less
            pkgs.which
            pkgs.file
            pkgs.tree
            pkgs.procps
            pkgs.ncurses
            pkgs.gnutar
            pkgs.gzip
            pkgs.bzip2
            pkgs.xz
            pkgs.zip
            pkgs.unzip
            pkgs.curl
            pkgs.wget
            pkgs.gitMinimal
            pkgs.openssh
            pkgs.nano
            pkgs.jq
            pkgs.ripgrep
            pkgs.python3
            pkgs.nodejs_22
          ];

          # The image's fonts, and which of them the generic names mean.
          # Without the aliases "sans-serif" and "monospace" resolve to
          # whatever comes first, a serif: in the desktop's menus, in a
          # terminal (whose cells are then a letter and a half wide), and in
          # web pages that ask for a generic family.
          fonts-conf = pkgs.writeText "fonts.conf" ''
            <?xml version="1.0"?>
            <!DOCTYPE fontconfig SYSTEM "urn:fontconfig:fonts.dtd">
            <fontconfig>
              <include>${
                pkgs.makeFontsConf {
                  fontDirectories = [
                    pkgs.dejavu_fonts
                    pkgs.noto-fonts
                    pkgs.noto-fonts-color-emoji
                  ];
                }
              }</include>
              ${pkgs.lib.concatMapStrings
                (
                  { generic, font }:
                  ''
                    <alias binding="same"><family>${generic}</family><prefer><family>${font}</family></prefer></alias>
                  ''
                )
                [
                  { generic = "sans-serif"; font = "DejaVu Sans"; }
                  { generic = "sans"; font = "DejaVu Sans"; }
                  { generic = "system-ui"; font = "DejaVu Sans"; }
                  { generic = "serif"; font = "DejaVu Serif"; }
                  { generic = "monospace"; font = "DejaVu Sans Mono"; }
                  { generic = "mono"; font = "DejaVu Sans Mono"; }
                ]
              }
            </fontconfig>
          '';

          runtime = pkgs.writeShellApplication {
            name = "browser-entrypoint";
            runtimeInputs = [
              # First: `chromium` is the session's browser.
              session-chromium
              browser-mcp
              pkgs.caddy
              pkgs.dbus
              desktop
              # exec-server.sh: mcp-exec, and find to prune its old logs.
              mcp-exec-pkg
              pkgs.python3Packages.websockify
              # Owns the clipboard for files put on it (browser/clipboard.js).
              pkgs.xclip
              pkgs.xorg.xdpyinfo
              # The clipboard operations of desktop_execute (nut.js runs it).
              pkgs.xsel
              # Windows and the screen from a script or a terminal; the
              # entrypoint maximises Chromium with wmctrl.
              pkgs.wmctrl
              pkgs.xdotool
              pkgs.xev
              pkgs.xprop
              pkgs.xrandr
              pkgs.xset
              pkgs.xwininfo
              xvnc
            ]
            ++ shell-tools;
            text = ''
              export NOVNC_WEB=${pkgs.novnc}/share/webapps/novnc
              export CADDYFILE=${./browser/Caddyfile}
              export CHROMIUM_BIN=${pkgs.chromium}/bin/chromium
              export DBUS_SESSION_CONF=${pkgs.dbus}/share/dbus-1/session.conf
              export XDG_CONFIG_DIRS=${./desktop}/xdg:${desktop}/etc/xdg
              export XDG_DATA_DIRS=${./desktop}/share:${desktop}/share
              export DESKTOP_BASHRC=${./desktop/bashrc}
              export SHELL=${pkgs.bashInteractive}/bin/bash
              export TZDIR=${pkgs.tzdata}/share/zoneinfo
              export EXEC_SERVER=${./browser/exec-server.sh}
              export SSL_CERT_FILE=${pkgs.cacert}/etc/ssl/certs/ca-bundle.crt
              export FONTCONFIG_FILE=${fonts-conf}
              exec ${pkgs.bash}/bin/bash ${./browser/entrypoint.sh} "$@"
            '';
          };

          # The build-time smoke tests' window manager maximises their
          # terminals; it is not in the image.
          openbox-rc = pkgs.writeText "openbox-rc.xml" ''
          <?xml version="1.0" encoding="UTF-8"?>
          <openbox_config xmlns="http://openbox.org/3.4/rc">
            <applications>
              <application type="normal"><maximized>yes</maximized></application>
            </applications>
          </openbox_config>
          '';

          # Proof, at build time, that desktop_execute works for real: the
          # packaged server's nut.js against the image's own Xvnc
          # (test/desktop-smoke.mjs). The Dockerfile builds it before the
          # image. x86_64 only, like the addon. The window manager here is
          # openbox, maximising the test's terminal, and is not in the image:
          # XFCE needs a session bus and a home, which the build sandbox has
          # not. desktop_execute on the image's XFCE is checked once the image
          # is built (test/desktop-image-smoke.sh).
          desktop-smoke =
            pkgs.runCommand "desktop-smoke"
              {
                nativeBuildInputs = [
                  pkgs.nodejs_22
                  pkgs.openbox
                  pkgs.xdpyinfo
                  pkgs.xrandr
                  pkgs.xsel
                  pkgs.xterm
                  xvnc
                ];
              }
              ''
                export HOME=$TMPDIR/home DISPLAY=:98
                export FONTCONFIG_FILE=${pkgs.makeFontsConf { fontDirectories = [ pkgs.dejavu_fonts ]; }}
                mkdir -p "$HOME" /tmp/.X11-unix
                Xvnc :98 -geometry 1280x800 -depth 24 -nolisten tcp -ac \
                  -rfbport 5998 -localhost -UseIPv6=0 -SecurityTypes None -AcceptSetDesktopSize &
                trap 'kill $(jobs -p) 2>/dev/null || true' EXIT
                for _ in $(seq 1 100); do
                  xdpyinfo >/dev/null 2>&1 && break
                  sleep 0.1
                done
                xdpyinfo >/dev/null
                openbox --sm-disable --config-file ${openbox-rc} &
                DESKTOP_JS=${browser-mcp}/lib/node_modules/browser-mcp/desktop.js \
                  node ${./test/desktop-smoke.mjs}
                touch $out
              '';

          # The same for shell commands: mcp-exec as packaged, started by
          # exec-server.sh as the entrypoint starts it, called over HTTP as
          # mcp-js calls it (test/exec-smoke.mjs): a command end to end, a
          # window opened on the image's Xvnc, and requests that look like a
          # web page's refused. The Dockerfile builds it before the image.
          exec-smoke =
            pkgs.runCommand "exec-smoke"
              {
                nativeBuildInputs = [
                  mcp-exec-pkg
                  pkgs.bash
                  pkgs.findutils
                  pkgs.nodejs_22
                  pkgs.openbox
                  pkgs.xdpyinfo
                  pkgs.xterm
                  pkgs.xwininfo
                  xvnc
                ];
              }
              ''
                export HOME=$TMPDIR/home DISPLAY=:97
                export FONTCONFIG_FILE=${pkgs.makeFontsConf { fontDirectories = [ pkgs.dejavu_fonts ]; }}
                mkdir -p "$HOME" /tmp/.X11-unix
                Xvnc :97 -geometry 1280x800 -depth 24 -nolisten tcp -ac \
                  -rfbport 5997 -localhost -UseIPv6=0 -SecurityTypes None &
                trap 'kill $(jobs -p) 2>/dev/null || true' EXIT
                for _ in $(seq 1 100); do
                  xdpyinfo >/dev/null 2>&1 && break
                  sleep 0.1
                done
                xdpyinfo >/dev/null
                openbox --sm-disable --config-file ${openbox-rc} &
                EXEC_SERVER=${./browser/exec-server.sh} node ${./test/exec-smoke.mjs}
                touch $out
              '';
        in
        {
          inherit
            browser-mcp
            desktop
            exec-smoke
            runtime
            xvnc
            ;
          mcp-exec = mcp-exec-pkg;
          default = runtime;
        }
        // pkgs.lib.optionalAttrs pkgs.stdenv.hostPlatform.isx86_64 { inherit desktop-smoke; }
      );
    };
}
