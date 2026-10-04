{ pkgs }:
pkgs.testers.runNixOSTest {
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
      images = [ pkgs.k3s_1_34.airgap-images ];
      extraFlags = [ "--snapshotter=native" "--flannel-backend=host-gw"
        "--kubelet-arg=fail-swap-on=false" ];
    };
    environment.systemPackages = [ pkgs.kubectl ];
    environment.variables.KUBECONFIG = "/etc/rancher/k3s/k3s.yaml";
  };
  testScript = ''
    start_all()
    cluster.wait_for_unit("k3s.service")
    cluster.wait_until_succeeds("kubectl get --raw=/readyz", timeout=180)
    cluster.wait_until_succeeds("kubectl get nodes -o jsonpath='{.items[0].status.conditions[?(@.type==\"Ready\")].status}' | grep True", timeout=180)
    cluster.succeed("kubectl get pods -A -o wide")
  '';
}
