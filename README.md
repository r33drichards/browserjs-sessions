# Computer Use

A JavaScript REPL for cloud computer use.

A session is a desktop container: Chromium on a Linux desktop with a
persistent disk, isolated by gVisor, that an agent drives through one MCP tool
(`run_js`) and a person can watch and take over in the browser. Idle sessions
sleep to a snapshot and cost nothing; the next request wakes them where they
were. Policies written in Rego decide what an agent's tool calls may do.

- The hosted service: <https://app.computeruse.site>
- Documentation (tutorials, guides, reference): <https://computeruse.site>
- Engineering notes for this repository: [`docs/`](docs/)

## How it fits together

```
 browser / MCP client
        |  HTTPS
        v
   Pomerium ---- Dex ---- Google, GitHub        sign-in; identity headers
        |
        v
    backend (Go) -------- Kubernetes API        sessions are Sandbox objects
        |   \                                    (kubernetes-sigs/agent-sandbox)
        |    +---- policy operator + OPA        Rego policy for every tool call
        v
  session pod (gVisor, one per session, its own disk)
    mcp-js   run_js: a V8 REPL whose state persists between calls
    browser  Chromium, Xvnc + noVNC, desktop control (nut.js), shell (mcp-exec)
```

| Directory | What is in it |
|---|---|
| `backend/` | Go: the API, the owner check, the VNC and MCP proxy, sleep and wake, API tokens, billing |
| `web/` | React and Cloudscape: the admin interface, with noVNC for the screen |
| `images/` | the two session images (`browser`, `mcp-js`) and the two operators (`policy-operator`, `billing-operator`) |
| `deploy/` | kustomize: `base`, `local` (kind) and `gke` (the hosted service) |
| `infra/` | OpenTofu for the Google Cloud project: cluster, registry, DNS, snapshots bucket |
| `terraform-provider-browserjs/` | a Terraform/OpenTofu provider for sessions and their policies |
| `site/` | the public documentation site (VitePress) |
| `hack/`, `test/` | scripts for the local cluster and for operations; end-to-end tests |

Some names still say `browserjs` (the Kubernetes namespace and labels, the
Terraform provider, the `bjs_` prefix of API tokens, the Google Cloud project
of the hosted service). That was the project's first name; they are kept so
that nothing deployed has to move.

## Quick start: a local cluster

You need [Nix](https://nixos.org/download) and a running Docker (on macOS,
`nix develop -c colima start --cpu 6 --memory 12`), about 12 GB of free disk,
and ports 443 and 5556 free on 127.0.0.1.

```sh
git clone https://github.com/r33drichards/computer-use && cd computer-use
nix develop -c hack/local-up.sh
```

That creates a kind cluster, installs Agent Sandbox, builds the images and
applies `deploy/local`. Trust the throwaway certificate authority it made and
open <https://app.localtest.me>; sign in as `alice@example.com`, password
`test`. The details, and how to tear it down:
[`docs/local-development.md`](docs/local-development.md).

kind has no gVisor and no Pod Snapshots: local sessions run under the ordinary
runtime and always wake cold. Do not expose a local cluster to anyone you do
not trust.

## Deploy your own on GKE

[`docs/deploy-your-own.md`](docs/deploy-your-own.md) lists what to change
(project, domain, who may sign in) and the order to do it in. In short:
`infra/bootstrap/bootstrap.sh` once by hand, OpenTofu in `infra/main` from
GitHub Actions, then the `images` and `deploy` workflows.

`deploy/gke` and `infra/main/terraform.tfvars` are the configuration of the
hosted service as it runs, not a template: the domain, the project and the
allowed e-mail addresses in them are ours.

## Develop

```sh
nix develop                                  # Go, Node, kubectl, kind, kustomize
(cd backend && go test ./...)
(cd web && npm ci && npm test)
(cd terraform-provider-browserjs && go test ./...)
(cd infra/main && nix shell nixpkgs#opentofu -c sh -c 'tofu init -backend=false && tofu test')
```

[`CONTRIBUTING.md`](CONTRIBUTING.md) says how changes are proposed and what
the checks on a pull request do.

## Security

Please report vulnerabilities privately: [`SECURITY.md`](SECURITY.md).

## Licence

[Apache License 2.0](LICENSE). The container images bundle other people's
software under its own licences, some of it copyleft:
[`THIRD_PARTY.md`](THIRD_PARTY.md).
