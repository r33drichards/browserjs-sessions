{
  description = "browserjs billing operator: dev shell for its tests";
  # The revision images/policy-operator pins: one Python and one kopf for
  # the two operators.
  inputs.nixpkgs.url = "github:NixOS/nixpkgs/c59305bab2065cfecc4944690d9eedbb56f3a9fa";
  outputs = { self, nixpkgs }:
    let
      systems = [ "aarch64-darwin" "x86_64-darwin" "x86_64-linux" "aarch64-linux" ];
      forAll = f: nixpkgs.lib.genAttrs systems (s: f nixpkgs.legacyPackages.${s});
    in {
      devShells = forAll (pkgs: {
        default = pkgs.mkShell {
          packages = [
            (pkgs.python312.withPackages (p: with p; [
              # nixpkgs' kopf fails one of its own tests against the aiohttp
              # it ships with (a DeprecationWarning turned into an error).
              (kopf.overridePythonAttrs (_: { doCheck = false; }))
              aiohttp pyyaml pytest pytest-asyncio pytest-aiohttp hypothesis
            ]))
            pkgs.uv
          ];
        };
      });
    };
}
