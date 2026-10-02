{
  description = "browserjs policy operator: dev shell for its tests";
  # The revision the repository's own flake.lock pins.
  inputs.nixpkgs.url = "github:NixOS/nixpkgs/c59305bab2065cfecc4944690d9eedbb56f3a9fa";
  outputs = { self, nixpkgs }:
    let
      # OPA as released, at the version the OPA Deployment and the Dockerfile
      # pin: nixpkgs' open-policy-agent is built from source and its test
      # phase fails (design, section 11). Checksums are the release's own
      # .sha256 files.
      opaVersion = "1.9.0";
      opaAssets = {
        aarch64-darwin = { name = "opa_darwin_arm64_static"; sha256 = "0134337a52bd255a2202eac4b4b85348fd4a77f94e8dab8ebcb2399ec018f4c0"; };
        x86_64-darwin = { name = "opa_darwin_amd64"; sha256 = "1122d0176604cd055d8f88b2b4d4019c469891d37e0bce9c1306e001e656ad2e"; };
        x86_64-linux = { name = "opa_linux_amd64_static"; sha256 = "66fa66f3b730b2fb086003863428b382b2898d343adb4b5dfab5598b4d739eed"; };
        aarch64-linux = { name = "opa_linux_arm64_static"; sha256 = "e7fdc5f823d5156cd449d6242b97b237cacbcbe4f531743d695c8d413d9aebb3"; };
      };
      forAll = f: nixpkgs.lib.genAttrs (builtins.attrNames opaAssets) (s: f s nixpkgs.legacyPackages.${s});
      opaFor = system: pkgs: pkgs.stdenvNoCC.mkDerivation {
        pname = "opa-bin";
        version = opaVersion;
        src = pkgs.fetchurl {
          url = "https://github.com/open-policy-agent/opa/releases/download/v${opaVersion}/${opaAssets.${system}.name}";
          inherit (opaAssets.${system}) sha256;
        };
        dontUnpack = true;
        installPhase = "install -Dm755 $src $out/bin/opa";
      };
    in {
      packages = forAll (system: pkgs: { opa = opaFor system pkgs; });
      devShells = forAll (system: pkgs: {
        default = pkgs.mkShell {
          packages = [
            (opaFor system pkgs)
            (pkgs.python312.withPackages (p: with p; [
              # nixpkgs' kopf fails one of its own tests against the aiohttp
              # it ships with (a DeprecationWarning turned into an error).
              (kopf.overridePythonAttrs (_: { doCheck = false; }))
              aiohttp pyyaml pytest pytest-asyncio pytest-aiohttp
            ]))
            pkgs.uv
          ];
        };
      });
    };
}
