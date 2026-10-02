# browserjs sessions

Per-user browserjs sessions (Chromium + mcp-js) as Kubernetes Agent Sandbox
resources, with a web UI. Design and plan: `docs/plans/`.

Dev shell: `nix develop`. Backend tests: `cd backend && go test ./...`.
The whole system on a local cluster: `docs/local-development.md`.
Production on GKE: `docs/gke-deployment.md` (the cluster itself: `infra/README.md`).
Moving files to and from a session's browser: `docs/file-transfer.md`.
