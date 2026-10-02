# Local development

The whole system on a kind cluster on this machine: Pomerium, Dex, the backend
and real session pods. Everything runs from the repository root, inside the
Nix dev shell.

## What you need

- Docker. On this Mac that is colima: `nix develop -c colima start --cpu 6 --memory 12`.
- About 12 GB of free disk before the first run. The scripts check and stop if
  there is less; a full disk corrupts colima's data volume.
- Ports 443 and 5556 free on 127.0.0.1.
- For sign-in with Google and GitHub (optional; the test users work without):
  the OAuth apps' credentials in the macOS Keychain as generic passwords named
  `browserjs-sessions-google-client-id`, `-google-client-secret`,
  `-github-client-id`, `-github-client-secret`. Both apps must have the
  callback `http://localhost:5556/dex/callback`. The script reads them at run
  time into a Kubernetes Secret; they are never written to a file.

## Bring it up

```bash
nix develop -c hack/local-up.sh
```

Safe to run again: it creates what is missing and updates the rest. It

1. creates the kind cluster `browserjs` (kubeconfig in `.local/kubeconfig`;
   your own kubeconfig is not touched);
2. installs Agent Sandbox (v1.0.4; `SANDBOX_VERSION=…` to change);
3. builds `browserjs/backend:dev`, and the two session images if they are not
   in Docker already (the browser image is a Nix build of about 14 GB of disk:
   it is never rebuilt automatically);
4. loads the images into the cluster;
5. makes a throwaway CA and certificate in `.local/tls/`, and the Secrets;
6. applies `deploy/local` and waits for everything to be ready.

To use `kubectl` yourself: `export KUBECONFIG=$PWD/.local/kubeconfig`.

## Sign in

Open <https://app.localtest.me>. The certificate is signed by the throwaway
CA, so the browser warns; accept it for `app.`, `authenticate.` and each
session host you open, or trust `.local/tls/ca.crt`.

Dex offers three ways in:

- **Log in with Email**: the test users `alice@example.com`, `bob@example.com`
  and `admin@example.com` (an admin), password `test`. Local only.
- **Google**, **GitHub**: the real providers, if the Keychain had the
  credentials. The Google app is in testing mode and admits only its listed
  test user.

| What | Where |
|---|---|
| The app | `https://app.localtest.me` |
| A session's MCP endpoint | `https://<id>.sessions.localtest.me/mcp` |
| Pomerium's sign-in host | `https://authenticate.localtest.me` |
| Dex | `http://localhost:5556/dex` |

`*.localtest.me` resolves to 127.0.0.1 with no `/etc/hosts` entry.

## Tests

Backend unit tests:

```bash
nix develop -c bash -c 'cd backend && go test ./...'
```

The backend and session pods end to end, without Pomerium (about 3.5 minutes):

```bash
nix develop -c python3 test/integration.py
```

It switches the backend to `deploy/local-test` (which trusts the test's own
signing keys), runs, and switches back to `deploy/local`. Do not sign in while
it runs: the backend does not accept Pomerium's identities until it is done.
Results: `.local/integration-results.json`.

The UI through Pomerium and Dex, in a headless Chrome:

```bash
(cd images/browser/browser && nix develop -c npm ci)   # once, for puppeteer-core
nix develop -c node test/browser-e2e.mjs
```

Screenshots and results go to `.local/` (`OUT_DIR` to change).

An MCP client's sign-in, walked by hand, against an existing session of
alice's (`KEEP=1 node test/browser-e2e.mjs` leaves one and prints its ID):

```bash
SKIP_DISCOVERY=1 nix develop -c node test/mcp-oauth.mjs <id>.sessions.localtest.me alice@example.com
```

Without `SKIP_DISCOVERY=1` it stops at the discovery documents, which Pomerium
v0.33.3 does not serve on a wildcard host: a real MCP client (Claude Code, the
MCP Inspector) cannot connect to a local session yet. See the plan, Phase 4.

## Changing things

- Backend or UI: run `hack/local-up.sh` again; it rebuilds the image and
  restarts the backend.
- Manifests: `kubectl apply -k deploy/local` (or run the script).
- Session images: build `browserjs/mcp-js:dev` or `browserjs/browser:dev`
  yourself, then run the script to load them. New sessions use the new image.

## Tear down

```bash
nix develop -c hack/local-down.sh
```

Deletes the cluster with every session and disk in it. The Docker images and
`.local/tls/` stay; colima keeps running (`colima stop` to stop it).
