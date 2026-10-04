{ pkgs }:
let
  inherit (pkgs) lib;
  source = lib.cleanSource ../..;
  python = pkgs.python312.withPackages (p: [ p.aiohttp p.redis p.pyyaml
    (p.kopf.overridePythonAttrs (_: { doCheck = false; })) ]);
  opa = pkgs.stdenvNoCC.mkDerivation {
    pname = "webhook-test-opa";
    version = "1.9.0";
    src = pkgs.fetchurl {
      url = "https://github.com/open-policy-agent/opa/releases/download/v1.9.0/opa_linux_amd64_static";
      sha256 = "66fa66f3b730b2fb086003863428b382b2898d343adb4b5dfab5598b4d739eed";
    };
    dontUnpack = true;
    installPhase = "install -Dm755 $src $out/bin/opa";
  };
  mcpjs = pkgs.stdenv.mkDerivation {
    pname = "webhook-test-mcpjs";
    version = "0.21.0-rc.4";
    src = pkgs.fetchurl {
      url = "https://github.com/r33drichards/mcp-js/releases/download/v0.21.0-rc.4/mcp-v8-linux";
      sha256 = "ad6808520def87c724e652b68f69b1d403abfb88b99354ef3e5af2e22916ccc2";
    };
    dontUnpack = true;
    nativeBuildInputs = [ pkgs.autoPatchelfHook ];
    buildInputs = [ pkgs.stdenv.cc.cc.lib pkgs.openssl pkgs.zlib ];
    installPhase = "install -Dm755 $src $out/bin/mcp-v8";
  };
in { inherit source python opa mcpjs; }
