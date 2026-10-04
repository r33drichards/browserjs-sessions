{ pkgs }:
let
  inherit (import ./packages.nix { inherit pkgs; }) source python opa mcpjs;
  backend = pkgs.buildGoModule {
    pname = "webhooks-backend"; version = "test";
    src = source + "/backend";
    vendorHash = "sha256-QrsJy+XVsqBiSd6P9Gyp1Pt2Vr1ERAWwpLIMMX5PrTI=";
    subPackages = [ "cmd/server" ];
    env.CGO_ENABLED = "0";
    doCheck = false;
  };
  image = name: contents: config: pkgs.dockerTools.buildLayeredImage {
    inherit name contents config;
    tag = "test";
    extraCommands = ''mkdir -p tmp; chmod 1777 tmp'';
  };
in {
  sandbox = pkgs.dockerTools.pullImage {
    imageName = "registry.k8s.io/agent-sandbox/agent-sandbox-controller";
    imageDigest = "sha256:c241245d7ff0068784ec0423f35efeb77c70ab08592533ca5b0cd7c36f8b2020";
    sha256 = "sha256-Iqm2pOqN1ASLZoqBitSkdAjFrAdeQV8uObKvzgZ71Hs=";
    finalImageTag = "v1.0.4";
    os = "linux"; arch = "amd64";
  };
  backend = image "webhooks/backend" [ backend pkgs.cacert ] {
    Entrypoint = [ "${backend}/bin/server" ];
    User = "65532:65532";
  };
  operator = image "webhooks/operator" [ python opa source pkgs.cacert ] {
    Env = [ "PATH=${python}/bin:${opa}/bin" "PYTHONPATH=${source}/images/policy-operator"
      "POLICY_CONTRACT_DIR=${source}/docs/contracts/policy" "OPA_BIN=${opa}/bin/opa" ];
  };
  opa = image "webhooks/opa" [ opa ] { Entrypoint = [ "${opa}/bin/opa" ]; };
  redis = image "webhooks/redis" [ pkgs.redis pkgs.busybox ] {
    Env = [ "PATH=${pkgs.redis}/bin:${pkgs.busybox}/bin" ];
  };
  mcpjs = image "webhooks/mcpjs" [ mcpjs python source ] {
    Entrypoint = [ "${mcpjs}/bin/mcp-v8" ];
    Env = [ "PATH=${python}/bin:${mcpjs}/bin" ];
  };
}
