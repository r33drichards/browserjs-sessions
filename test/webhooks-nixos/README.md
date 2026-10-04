# Webhook pipeline in NixOS containers

Run on an x86_64 Linux host:

```sh
nix build .#checks.x86_64-linux.webhooks-container -L
```

This uses `pkgs.testers.runNixOSTest` with `containers.stack` and an empty
`nodes` set. It boots systemd-nspawn, not QEMU, and needs no KVM, Docker, or
Kubernetes cluster. GitHub Actions runs it on `ubuntu-24.04`.

The Nix daemon needs these settings, as required by the NixOS container test
driver:

```ini
auto-allocate-uids = true
extra-system-features = nixos-test uid-range
extra-experimental-features = auto-allocate-uids cgroups
```

The workflow configures these and enables unprivileged user namespaces on
Ubuntu. macOS can evaluate the check, but running it requires a Linux builder
with those settings.

The container runs pinned MCPJS v0.21.0-rc.4 and OPA 1.9.0, the production
policy-operator code, authenticated Redis with AOF/always/noeviction, and an
HTTPS receiver with a generated certificate trusted by the collector. Its
public-looking address exists only on the container's loopback interface;
production URL/DNS checks, TLS verification, signing, and delivery are exercised
without a resolver override or an external webhook.

The collector bootstrap supplies watched SessionPolicy JSON from a file instead
of requiring Kubernetes. The upstream MCP fixture records actual executions;
the receiver persists deliveries and deduplicates event effects. Backend API
ownership and outer-request capture remain covered by the existing Go suites.

Assertions cover full/partial batches, signatures, allowed and denied calls,
Rego filtering, lost acknowledgements, identical retries across collector
restart, Redis SIGKILL/AOF recovery, persistence failures blocking execution,
and disabling capture while preserving accepted backlog. The result includes
the driver report, service journal, and receiver deliveries; CI uploads its
build log even when the test fails.
