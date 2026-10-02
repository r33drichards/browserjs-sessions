{
  description = "Persistent, VNC-viewable Chromium exposed as MCP behind mcp-js";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs =
    { self, nixpkgs }:
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
            nativeBuildInputs = [ pkgs.makeWrapper ];
            postFixup = ''
              wrapProgram "$out/bin/browser-mcp" --prefix PATH : ${pkgs.nodejs_22}/bin
            '';
          };

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
              pkgs.gnused
              pkgs.openbox
              pkgs.procps
              pkgs.python3Packages.websockify
              pkgs.xorg.xdpyinfo
              xvnc
            ];
            text = ''
              export NOVNC_WEB=${pkgs.novnc}/share/webapps/novnc
              export CADDYFILE=${./browser/Caddyfile}
              export OPENBOX_RC=${./browser/openbox-rc.xml}
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
        in
        {
          inherit browser-mcp runtime xvnc;
          default = runtime;
        }
      );
    };
}
