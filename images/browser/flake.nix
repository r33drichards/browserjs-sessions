{
  description = "Persistent, VNC-viewable Chromium exposed as MCP behind mcp-js";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
  # Shell commands for run_js. Pinned to the commit of
  # https://github.com/r33drichards/mcp-exec/pull/6 (--reject-browser-requests,
  # which browser/exec-server.sh depends on); move to master once it is merged.
  inputs.mcp-exec = {
    url = "github:r33drichards/mcp-exec/38bb517fc037cd2d9a82ab3cd1e28b2d1d2be0ad";
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

          runtime = pkgs.writeShellApplication {
            name = "browser-entrypoint";
            runtimeInputs = [
              browser-mcp
              pkgs.bash
              pkgs.caddy
              pkgs.chromium
              pkgs.coreutils
              # exec-server.sh: mcp-exec, and find to prune its old logs.
              mcp-exec-pkg
              pkgs.findutils
              pkgs.gnused
              pkgs.openbox
              pkgs.procps
              pkgs.python3Packages.websockify
              # Owns the clipboard for files put on it (browser/clipboard.js).
              pkgs.xclip
              pkgs.xorg.xdpyinfo
              # The clipboard operations of desktop_execute (nut.js runs it).
              pkgs.xsel
              xvnc
            ];
            text = ''
              export NOVNC_WEB=${pkgs.novnc}/share/webapps/novnc
              export CADDYFILE=${./browser/Caddyfile}
              export OPENBOX_RC=${./browser/openbox-rc.xml}
              export EXEC_SERVER=${./browser/exec-server.sh}
              export SSL_CERT_FILE=${pkgs.cacert}/etc/ssl/certs/ca-bundle.crt
              export FONTCONFIG_FILE=${pkgs.makeFontsConf {
                fontDirectories = [
                  pkgs.dejavu_fonts
                  pkgs.noto-fonts
                  pkgs.noto-fonts-color-emoji
                ];
              }}
              exec ${pkgs.bash}/bin/bash ${./browser/entrypoint.sh} "$@"
            '';
          };

          # Proof, at build time, that desktop_execute works for real: the
          # packaged server's nut.js against the image's own Xvnc and openbox
          # (test/desktop-smoke.mjs). The Dockerfile builds it before the
          # image. x86_64 only, like the addon.
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
                openbox --sm-disable --config-file ${./browser/openbox-rc.xml} &
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
                openbox --sm-disable --config-file ${./browser/openbox-rc.xml} &
                EXEC_SERVER=${./browser/exec-server.sh} node ${./test/exec-smoke.mjs}
                touch $out
              '';
        in
        {
          inherit browser-mcp runtime xvnc exec-smoke;
          mcp-exec = mcp-exec-pkg;
          default = runtime;
        }
        // pkgs.lib.optionalAttrs pkgs.stdenv.hostPlatform.isx86_64 { inherit desktop-smoke; }
      );
    };
}
