{ pkgs }:
let
  inherit (import ./packages.nix { inherit pkgs; }) source python;
  sandboxManifest = pkgs.fetchurl {
    url = "https://github.com/kubernetes-sigs/agent-sandbox/releases/download/v1.0.4/sandbox-with-extensions.yaml";
    sha256 = "8cbe7f4c252463667e2286993bd735cb33bb2e47d7f304b92aa2ca97f57052c0";
  };
  images = import ./images.nix { inherit pkgs; };
in pkgs.testers.runNixOSTest {
  name = "webhooks-k3s-nspawn";
  nodes = {};
  containers.cluster = { ... }: {
    system.stateVersion = "26.05";
    documentation.enable = false;
    documentation.man.enable = false;
    documentation.nixos.enable = false;
    networking.firewall.enable = false;
    virtualisation.systemd-nspawn.options = [ "--capability=all" ];
    services.k3s = {
      enable = true;
      package = pkgs.k3s_1_34;
      role = "server";
      disable = [ "traefik" "metrics-server" ];
      images = [ pkgs.k3s_1_34.airgap-images ] ++ builtins.attrValues images;
      extraFlags = [ "--snapshotter=native" "--flannel-backend=host-gw"
        "--node-ip=10.20.0.1" "--advertise-address=10.20.0.1" "--flannel-iface=eth0"
        "--kubelet-arg=feature-gates=KubeletInUserNamespace=true" "--kubelet-arg=fail-swap-on=false" ];
    };
    systemd.services.test-network = {
      wantedBy = [ "multi-user.target" ];
      before = [ "k3s.service" ];
      serviceConfig = { Type = "oneshot"; RemainAfterExit = true; };
      script = ''
        ${pkgs.iproute2}/bin/ip link add eth0 type dummy
        ${pkgs.iproute2}/bin/ip address add 10.20.0.1/24 dev eth0
        ${pkgs.iproute2}/bin/ip link set eth0 up
        ${pkgs.iproute2}/bin/ip route add default dev eth0
      '';
    };
    systemd.services.k3s = { after = [ "test-network.service" ]; requires = [ "test-network.service" ]; };
    environment.systemPackages = [ pkgs.kubectl pkgs.curl pkgs.jq python ];
    environment.variables.KUBECONFIG = "/etc/rancher/k3s/k3s.yaml";
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
    systemd.services.webhook-receiver = {
      wantedBy = [ "multi-user.target" ]; after = [ "webhook-tls.service" ]; requires = [ "webhook-tls.service" ];
      serviceConfig = { StateDirectory = "webhook-receiver";
        ExecStart = "${python}/bin/python ${source}/test/webhooks-nixos/receiver.py"; };
    };
    environment.etc."webhook-call.py".source = "${source}/test/policy/mcp_call.py";
    environment.etc."render-webhooks".text = ''
      #!${pkgs.runtimeShell}
      export SOURCE=${source} PYTHON=${python}/bin/python SANDBOX_MANIFEST=${sandboxManifest}
      exec ${python}/bin/python ${source}/test/webhooks-nixos/manifests.py
    '';
    environment.etc."render-webhooks".mode = "0755";
  };
  testScript = builtins.readFile ./k3s-test.py;
}
