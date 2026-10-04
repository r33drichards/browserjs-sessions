{
  description = "browserjs sessions";
  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
  outputs = { self, nixpkgs }:
    let
      systems = [ "aarch64-darwin" "x86_64-darwin" "x86_64-linux" "aarch64-linux" ];
      forAll = f: nixpkgs.lib.genAttrs systems (s: f nixpkgs.legacyPackages.${s});
    in {
      checks.x86_64-linux.webhooks-container = import ./test/webhooks-nixos {
        pkgs = nixpkgs.legacyPackages.x86_64-linux;
      };
      devShells = forAll (pkgs: {
        default = pkgs.mkShell {
          packages = with pkgs; [
            go gopls nodejs_22 kubectl kind kustomize colima docker-client jq curl
          ];
        };
        # The SDK (sdk/): the Rust core and what packages it for Python,
        # JavaScript and Go. `nix develop .#sdk -c cargo test` in sdk/.
        sdk = pkgs.mkShell {
          packages = with pkgs; [
            cargo rustc clippy rustfmt maturin python3 go nodejs_22
          ] ++ pkgs.lib.optionals pkgs.stdenv.hostPlatform.isDarwin [ pkgs.libiconv ];
          # For the prebuilt Node addon of the UniFFI runtime (@ubjs/node),
          # which expects libgcc_s and libstdc++ on the loader's path.
          LD_LIBRARY_PATH = pkgs.lib.optionalString pkgs.stdenv.hostPlatform.isLinux
            (pkgs.lib.makeLibraryPath [ pkgs.stdenv.cc.cc.lib ]);
        };
      });
    };
}
