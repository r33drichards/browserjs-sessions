{ pkgs, gateway, execServer }:
let
  browser = pkgs.buildNpmPackage {
    pname = "computer-use-browser-mcp";
    version = "2.0.0";
    src = ../browser;
    npmDeps = pkgs.importNpmLock { npmRoot = ../browser; };
    npmConfigHook = pkgs.importNpmLock.npmConfigHook;
    dontNpmBuild = true;
    npmFlags = [ "--ignore-scripts" ];
    nativeBuildInputs = pkgs.lib.optionals pkgs.stdenv.hostPlatform.isLinux [ pkgs.patchelf ];
    postInstall = ''
      modules=$out/lib/node_modules/browser-mcp/node_modules
      rm -rf "$modules"/@nut-tree-fork/libnut-win32/build
      ${pkgs.lib.optionalString pkgs.stdenv.hostPlatform.isDarwin ''
        rm -rf "$modules"/@nut-tree-fork/libnut-linux/build "$modules"/clipboardy/fallbacks/linux
      ''}
      ${pkgs.lib.optionalString pkgs.stdenv.hostPlatform.isLinux ''
        rm -rf "$modules"/@nut-tree-fork/libnut-darwin/build
      ''}
    '' + pkgs.lib.optionalString
      (pkgs.stdenv.hostPlatform.isLinux && pkgs.stdenv.hostPlatform.isx86_64) ''
        patchelf --set-rpath ${pkgs.lib.makeLibraryPath [ pkgs.libx11 pkgs.libxtst pkgs.stdenv.cc.cc.lib ]} \
          "$out/lib/node_modules/browser-mcp/node_modules/@nut-tree-fork/libnut-linux/build/Release/libnut.node"
      '';
  };
  files = pkgs.runCommand "computer-use-mcp-config" {} ''
    mkdir -p $out/bin $out/code-mode
    cp ${../bin/start.mjs} $out/bin/start.mjs
    cp ${../code-mode/mcp_tools.rego} $out/code-mode/mcp_tools.rego
    cp ${../local-instructions.md} $out/local-instructions.md
  '';
in pkgs.writeShellApplication {
  name = "computer-use-mcp";
  runtimeInputs = [ pkgs.nodejs_22 ] ++ pkgs.lib.optionals pkgs.stdenv.hostPlatform.isLinux [ pkgs.xdpyinfo pkgs.xclip pkgs.xsel ];
  text = ''
    export MCP_V8_BIN=${pkgs.lib.getExe gateway}
    export MCP_EXEC_BIN=${pkgs.lib.getExe execServer}
    export COMPUTER_USE_BROWSER_SERVER=${browser}/lib/node_modules/browser-mcp/server.js
    exec ${pkgs.nodejs_22}/bin/node ${files}/bin/start.mjs "$@"
  '';
  meta.description = "Code-mode browser, desktop and exec MCP with bundled configuration";
}
