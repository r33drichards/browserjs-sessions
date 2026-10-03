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
2. installs Agent Sandbox (v1.0.4, because v1.0.5 was released without its
   controller image; `SANDBOX_VERSION=…` to change);
3. builds `browserjs/backend:dev`, and the two session images if they are not
   in Docker already (the browser image is a Nix build of about 14 GB of disk:
   it is never rebuilt automatically);
4. loads the images into the cluster;
5. makes a throwaway CA and certificate in `.local/tls/`, and the Secrets;
6. applies `deploy/local` and waits for everything to be ready.

To use `kubectl` yourself: `export KUBECONFIG=$PWD/.local/kubeconfig`.

## Sign in

First trust the throwaway CA, once (it asks for your password; undo it with
`security remove-trusted-cert .local/tls/ca.crt`):

```bash
security add-trusted-cert -r trustRoot -k ~/Library/Keychains/login.keychain-db .local/tls/ca.crt
```

Clicking through the browser's warning instead is not enough: the session's
screen is a websocket to another host (`sessions.localtest.me`), and a
browser gives no way to accept an untrusted certificate for that, so the
screen stays on "Connecting". Restart the browser after trusting the CA.

Then open <https://app.localtest.me>.

Dex offers three ways in:

- **Log in with Email**: the test users `alice@example.com`, `bob@example.com`
  and `admin@example.com` (an admin), password `test`. Local only.
  `mallory@example.com` can sign in at Dex but is not allowed into the app.
- **Google**, **GitHub**: the real providers, if the Keychain had the
  credentials. The Google app is in testing mode and admits only its listed
  test user.

Who gets in after signing in is the list of email addresses in
`deploy/local/pomerium-config.yaml` (`policy:` on the MCP route, shared with
the app route); everyone else gets Pomerium's 403 page. A GitHub account is
known by its primary verified email, which has to be on the list. After
editing the list, `kubectl apply -k deploy/local`.

| What | Where |
|---|---|
| The app | `https://app.localtest.me` |
| A session's MCP endpoint | `https://sessions.localtest.me/<id>/mcp` ([session-urls.md](session-urls.md)) |
| Pomerium's sign-in host | `https://authenticate.localtest.me` |
| Dex | `http://localhost:5556/dex` |

`*.localtest.me` resolves to 127.0.0.1 with no `/etc/hosts` entry.

A certificate made before the sessions had one host does not name
`sessions.localtest.me`: `hack/local-up.sh` then makes a new CA and
certificate, and the new CA has to be trusted again (above).

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
(cd computer-use-mcp/browser && nix develop -c npm ci)   # once, for puppeteer-core
nix develop -c node test/browser-e2e.mjs
```

Screenshots and results go to `.local/` (`OUT_DIR` to change). It also runs
`test/mcp-client.mjs` (below) for the session's owner and for another user.

An MCP client through Pomerium: the MCP SDK's client does the discovery, the
OAuth flow and the calls; the script signs the user in at Dex. It needs an
existing session of alice's (`KEEP=1 node test/browser-e2e.mjs` leaves one and
prints its ID):

```bash
nix develop -c node test/mcp-client.mjs https://sessions.localtest.me/<id>/mcp alice@example.com   # works
nix develop -c node test/mcp-client.mjs https://sessions.localtest.me/<id>/mcp bob@example.com     # 404: not his
nix develop -c node test/mcp-client.mjs <id>.sessions.localtest.me alice@example.com               # the session's old host (deprecated): works
```

`test/mcp-oauth.mjs` takes the same arguments and walks the same sign-in one
request at a time, printing what each step answered.

To point a real client at a local session, give it
`https://sessions.localtest.me/<id>/mcp` and the CA
(`NODE_EXTRA_CA_CERTS=$PWD/.local/tls/ca.crt` for Node-based clients). Only
clients whose client ID document is under `claude.ai` are accepted. This has
not been tried with Claude Code.

## Changing things

- Backend or UI: run `hack/local-up.sh` again; it rebuilds the image and
  restarts the backend.
- Manifests: `kubectl apply -k deploy/local` (or run the script).
- Session images: build `browserjs/mcp-js:dev` or `browserjs/browser:dev`
  yourself, then run the script to load them. New sessions use the new image.
  After the next browser image build, delete `deploy/local/blueprint.yaml`
  and its entry in `deploy/local/kustomization.yaml`: it only exists because
  the current local image predates a fix in its entrypoint.
- The images built here are arm64. Production images for GKE (amd64) are
  built in CI.

## Tear down

```bash
nix develop -c hack/local-down.sh
```

Deletes the cluster with every session and disk in it. The Docker images and
`.local/tls/` stay; colima keeps running (`colima stop` to stop it).
