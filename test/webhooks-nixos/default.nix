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
  resource = {
    apiVersion = "browserjs.dev/v1alpha1";
    kind = "SessionPolicy";
    metadata.name = "s-abcde";
    spec = {
      sessionRef.name = "s-abcde";
      kind = "rego";
      source = "package browserjs.policy\nimport rego.v1\nallow_tool_call := true\n";
      webhook = { url = "https://webhook.example.test/events"; batch_size = 2;
        flush_interval_seconds = 5; signing_secret = "container-signing-secret"; filter = ""; };
    };
  };
  initial = pkgs.writeText "session-policy.json" (builtins.toJSON resource);
  policies = pkgs.writeText "mcpjs-policies.json" (builtins.toJSON {
    mcp_tools = {
      pre = [{ url = "http://127.0.0.1:8080"; policy_path = "browserjs/hooks/s-abcde/mcp_tools/pre"; }];
      policies = [{ url = "http://127.0.0.1:8181"; policy_path = "browserjs/decision/s-abcde/mcp_tools"; }];
    };
  });
  upstream = pkgs.writeText "upstream.json" (builtins.toJSON [{
    name = "exec"; transport = "stdio"; command = "${python}/bin/python";
    args = [ "${source}/test/webhooks-nixos/upstream.py" ];
  }]);
  opaConfig = pkgs.writeText "opa.json" (builtins.toJSON {
    services.operator = { url = "http://127.0.0.1:8080"; credentials.bearer.token = "bundle-secret"; };
    bundles.browserjs = { service = "operator"; resource = "/bundles/browserjs.tar.gz";
      polling = { min_delay_seconds = 1; max_delay_seconds = 2; }; };
  });
in pkgs.testers.runNixOSTest {
  name = "tool-call-webhooks-container";
  # Only nspawn containers: no kernel image, QEMU, KVM, Docker, or kind.
  nodes = {};
  containers.stack = { ... }: {
    system.stateVersion = "26.05";
    networking.firewall.enable = false;
    networking.extraHosts = "93.184.216.34 webhook.example.test";
    environment.systemPackages = [ python pkgs.curl pkgs.jq pkgs.redis ];
    systemd.services.webhook-tls = {
      wantedBy = [ "multi-user.target" ];
      serviceConfig = { Type = "oneshot"; RemainAfterExit = true; StateDirectory = "webhook-tls"; };
      script = ''
        ${pkgs.iproute2}/bin/ip address add 93.184.216.34/32 dev lo
        ${pkgs.openssl}/bin/openssl req -x509 -newkey rsa:2048 -nodes -days 2 \
          -subj /CN=webhook.example.test -addext subjectAltName=DNS:webhook.example.test \
          -keyout /var/lib/webhook-tls/key.pem -out /var/lib/webhook-tls/cert.pem
      '';
    };
    systemd.services.redis-test = {
      wantedBy = [ "multi-user.target" ];
      serviceConfig = { StateDirectory = "redis-test";
        ExecStart = "${pkgs.redis}/bin/redis-server --bind 127.0.0.1 --dir /var/lib/redis-test --appendonly yes --appendfsync always --save '' --maxmemory-policy noeviction --requirepass redis-test-secret"; };
    };
    systemd.services.webhook-receiver = {
      wantedBy = [ "multi-user.target" ]; after = [ "webhook-tls.service" ]; requires = [ "webhook-tls.service" ];
      serviceConfig = { StateDirectory = "webhook-receiver";
        ExecStart = "${python}/bin/python ${source}/test/webhooks-nixos/receiver.py"; };
    };
    systemd.services.webhook-collector = {
      wantedBy = [ "multi-user.target" ]; after = [ "redis-test.service" "webhook-tls.service" ];
      wants = [ "redis-test.service" ]; requires = [ "webhook-tls.service" ];
      environment = {
        PYTHONPATH = "${source}/images/policy-operator";
        POLICY_CONTRACT_DIR = "${source}/docs/contracts/policy";
        OPA_BIN = "${opa}/bin/opa";
        WEBHOOK_REDIS_URL = "redis://127.0.0.1:6379/0";
        WEBHOOK_REDIS_PASSWORD = "redis-test-secret";
        BUNDLE_TOKEN = "bundle-secret";
        OPERATOR_API_TOKEN = "api-secret";
        SSL_CERT_FILE = "/var/lib/webhook-tls/cert.pem";
      };
      preStart = ''
        if [ ! -f /var/lib/webhook-collector/resource.json ]; then
          cp ${initial} /var/lib/webhook-collector/resource.json
          chmod u+w /var/lib/webhook-collector/resource.json
        fi
      '';
      serviceConfig = { StateDirectory = "webhook-collector";
        ExecStart = "${python}/bin/python ${source}/test/webhooks-nixos/collector.py"; };
    };
    systemd.services.opa-test = {
      wantedBy = [ "multi-user.target" ]; after = [ "webhook-collector.service" ];
      environment.OPERATOR_TOKEN = "opa-secret";
      serviceConfig.ExecStart = "${opa}/bin/opa run --server --addr 127.0.0.1:8181 --authentication=token --authorization=basic --config-file ${opaConfig} ${source}/docs/contracts/policy/system-authz.rego";
    };
    systemd.services.mcpjs = {
      wantedBy = [ "multi-user.target" ]; after = [ "opa-test.service" "webhook-collector.service" ];
      serviceConfig = { StateDirectory = "mcpjs";
        ExecStart = "${mcpjs}/bin/mcp-v8 --http-port 8088 --session-db-path /var/lib/mcpjs/sessions --policies-json ${policies} --mcp-config ${upstream}"; };
    };
    environment.etc."webhook-call.py".source = "${source}/test/policy/mcp_call.py";
  };
  testScript = builtins.readFile ./test.py;
}
