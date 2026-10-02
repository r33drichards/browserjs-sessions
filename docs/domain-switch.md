# Moving the deployment from browserjs.com to computeruse.site

The product is still called browserjs. Only the domain its hosts are under
changes:

| | before | after |
| --- | --- | --- |
| App | `https://app.browserjs.com` | `https://app.computeruse.site` |
| Pomerium's sign-in host | `https://authenticate.browserjs.com` | `https://authenticate.computeruse.site` |
| The API (API tokens; off today) | `https://api.browserjs.com` | `https://api.computeruse.site` |
| Dex (and its issuer) | `https://dex.browserjs.com/dex` | `https://dex.computeruse.site/dex` |
| A session | `https://sessions.browserjs.com/<id>/mcp` | `https://sessions.computeruse.site/<id>/mcp` |
| A session, the deprecated form | `https://<id>.sessions.browserjs.com/mcp` | `https://<id>.sessions.computeruse.site/mcp` |

The address (`8.231.155.139`, the reserved one), the cluster, the project,
the images, the sessions and their disks stay as they are.

## The design

Three changes, each a pull request. The first is safe at any time, the
second is the switch, the third is optional and deletes things.

1. **A second zone** (`infra/main`, `additional_domains`). Cloud DNS gets a
   zone for `computeruse.site` with the same six records as `browserjs.com`,
   to the same address. Nothing is served under it. Additions only.
2. **The switch** (`deploy/gke`, `deploy.yml`, `infra/main`, tests, docs).
   Every host moves at once. `browserjs.com` stops being served: its zone
   and records stay, untouched, and still point at the address, but Pomerium
   has no route for those hosts and the certificate does not name them.
3. **Removing browserjs.com** (`infra/main`, `previous_domain`). Deletes the
   old zone and its records. Not needed for anything; do it when nobody is
   expected to come back.

Why all hosts at once, and not both domains side by side: Dex has one issuer
URL, and the tokens it signs, Pomerium's client registration with it and the
OAuth apps' callbacks all name it. Pomerium has one authenticate URL, and
its cookies are per host. Half a move is a deployment where sign-in works on
neither.

Why the old hosts are dropped rather than redirected:

- A redirect needs the old names on the certificate, so every renewal would
  depend on a domain that is being retired, and on the issuers solving in
  two zones.
- It would not rescue what matters. An MCP client is configured with
  `https://sessions.browserjs.com/<id>/mcp` and holds a token that Pomerium
  issued for exactly that resource (the client was told the resource by the
  protected-resource metadata at that host). Sent to another host by a 307
  or 308, a client either does not follow (a `POST`, cross-origin), or
  follows and drops its `Authorization` header as HTTP clients do when the
  origin changes, or presents a token for the wrong resource. In each case
  the user ends up re-adding the connector, which is the fix anyway.
- Two people use this deployment. Telling them is cheaper than the routes.

If a redirect for people's bookmarks is wanted later (`app.browserjs.com` to
`app.computeruse.site`, browsers only), it is a Pomerium `redirect` route
plus the old name on the certificate, in a change of its own.

### What `infra/main` does with two domains

- Phase 1 leaves `google_dns_managed_zone.this` and
  `google_dns_record_set.public` (browserjs.com) exactly as they are and adds
  `google_dns_managed_zone.domains["computeruse.site"]` and
  `google_dns_record_set.domains["computeruse.site/<key>"]`.
- Phase 2 changes which domain the outputs and names are built from
  (`domain = "computeruse.site"`) and says where the old one is
  (`previous_domain = "browserjs.com"`). Every zone and record keeps its
  address in the state, so its plan is empty, and so is the plan of
  reverting it. There are no `moved` blocks on purpose: a zone that is
  deleted and made again gets other nameservers than the registrar has.
- cert-manager's DNS-01 role is granted on the project, not on a zone, so it
  can already write the challenge records in the new zone. What does pin a
  zone is `hostedZoneName` in `deploy/gke/issuers.yaml`; Phase 2 changes it.
- A name added to `local.public_names` in `edge.tf` gets a record in every
  zone.

### What is not a hostname and does not change

`browserjs.com/pool` (a node label, `infra/main/cluster.tf`, the backend,
`hack/gke-status.sh`), `browserjs.dev/owner` (a label), the project
`browserjs-sessions`, the cluster `browserjs`, the namespace, the service
accounts, the image registry, the Terraform provider's name.

## Where the domain is written

Found with `git grep -n 'browserjs\.com'`. "Literal" means the name is
spelled out and Phase 2 edits it; nothing in `deploy/gke` is templated.

| Where | What | How |
| --- | --- | --- |
| `infra/main/terraform.tfvars`, `variables.tf` | `domain` (and its default) | variable; `edge.tf` builds the six names, the zone and the outputs `hostnames`, `certificate_dns_names`, `dns_*` from it |
| `infra/main/tests/offline.tftest.hcl` | expected names | literal |
| `deploy/gke/certificate.yaml` | `dnsNames`, six | literal |
| `deploy/gke/issuers.yaml` | `hostedZoneName: browserjs-com`, both issuers | literal (the zone's name, not the domain) |
| `deploy/gke/pomerium-config.yaml` | `authenticate_service_url`, `idp_provider_url`, `from:` of twelve routes | literal |
| `deploy/gke/dex-config.yaml` | `issuer`, the client's `redirectURIs`, both connectors' `redirectURI` | literal |
| `deploy/gke/patch-backend.yaml` | `PUBLIC_URL`, `SESSION_URL_TEMPLATE`, `LEGACY_SESSION_URL_TEMPLATE`, `POMERIUM_JWKS_URL`, `API_URL` | literal. `SIGN_OUT_URL` is not set: its default is a path, `/.pomerium/sign_out` |
| `deploy/gke/blueprint.yaml` | `MCP_V8_PUBLIC_URL` | parameterised: `{{ .SessionURL }}`, which the backend renders from `SESSION_URL_TEMPLATE` when it creates a session |
| `deploy/gke/warmpool.yaml` | `MCP_V8_PUBLIC_URL` | literal: `https://sessions.<domain>/$(SESSION_ID)` |
| `deploy/gke/kustomization.yaml`, `deploy/gke-staging-issuer/kustomization.yaml` | comments | literal |
| `deploy/local`, `deploy/local-test`, `deploy/base` | `localtest.me`, `example.com` | not this domain; unchanged |
| `.github/workflows/deploy.yml` | `-servername app.browserjs.com` in the check of the served certificate | literal. `EDGE_IP` is a literal too and does not change |
| repository variables and secrets | `GCP_PROJECT_ID`, `GCP_REGION`, `GCP_WIF_PROVIDER`, `TOFU_STATE_BUCKET`, `TOFU_PLAN_SA`, `TOFU_APPLY_SA`, `DEPLOY_SA`, `IMAGE_REGISTRY`, `IMAGE_PUSH_SA`; `DEX_GOOGLE_CLIENT_ID`, `DEX_GOOGLE_CLIENT_SECRET`, `DEX_GITHUB_CLIENT_ID`, `DEX_GITHUB_CLIENT_SECRET` | none holds the domain. The two `DEX_GITHUB_*` change only if a new GitHub OAuth App is made (step 3) |
| `test/smoke.sh` | `DOMAIN`, default `browserjs.com` | parameterised; Phase 2 changes the default |
| `test/integration.py`, `test/*.mjs` | `localtest.me` | the local cluster; unchanged |
| `backend/internal/sessions/deploy_test.go` | the URLs the blueprint is rendered with for the comparison with `warmpool.yaml` | literal |
| `backend/` otherwise | nothing: every URL comes from the environment | parameterised |
| `web/` | nothing: the UI shows what the API returns | parameterised |
| `terraform-provider-browserjs/` | default `endpoint`, `https://api.browserjs.com`, in code, tests, docs and examples | literal |
| `docs/*.md`, `infra/README.md` | prose and commands | literal. `docs/plans/` is history and is left alone |

## Runbook

Where it stands: steps 1 to 3 are done. Phase 1 is merged and applied (7
added). computeruse.site is delegated to `ns-cloud-d1` … `ns-cloud-d4.googledomains.com`
and both public resolvers return them, with no DS record. Google has the new
redirect URI. Next is step 4.

"You" is whoever has the Namecheap account, the two OAuth apps and the right
to merge. Every merge to `main` that touches `infra/main` starts `infra
apply` by itself; `deploy` is started by hand.

Read the whole of step 6 before starting it: it is the only step with an
outage, and the order inside it matters.

### 1. Merge Phase 1 (you)

Before merging, read the pull request's `infra plan` run: **7 to add, 0 to
change, 0 to destroy** (one `google_dns_managed_zone.domains`, six
`google_dns_record_set.domains`). Anything else: stop.

Merge. `infra apply` runs. Its summary has

```
additional_dns_name_servers = {
  "computeruse.site" = [ "ns-cloud-?1.googledomains.com.", … four names ]
}
```

Verify, with one of those names in place of `$NS`:

```sh
NS=ns-cloud-?1.googledomains.com          # from the output
for h in api app authenticate dex sessions x.sessions; do
  dig +short "$h.computeruse.site" "@$NS"  # each prints 8.231.155.139
done
dig +short app.browserjs.com               # unchanged: 8.231.155.139
DOMAIN=browserjs.com test/smoke.sh         # still passes
```

Rollback: revert the pull request. The apply deletes the new zone and its
records and nothing else. Do not do this after step 2 without first undoing
step 2.

### 2. Point computeruse.site at Cloud DNS (you, at Namecheap)

<https://ap.www.namecheap.com/domains/domaincontrolpanel/computeruse.site/domain>:
Nameservers, **Custom DNS**, the four names from step 1 without their
trailing dots, save.

- They are **not** the nameservers of browserjs.com. Cloud DNS gives each
  zone its own set (the letter in `ns-cloud-a1` … `ns-cloud-e1` differs).
  With the wrong set, every name answers `REFUSED`.
- This replaces whatever Namecheap served for the domain: a parking page,
  mail forwarding, any record made there. Recreate what is still wanted in
  the Cloud DNS zone first.
- On the same page, Advanced DNS, DNSSEC must be **off** (the zone is not
  signed: `enable_dnssec = false`). A DS record left at the registry makes
  every validating resolver, and Let's Encrypt, answer `SERVFAIL`.

Verify (minutes, at worst a day):

```sh
dig +short NS computeruse.site @1.1.1.1    # the four Google names
dig +short NS computeruse.site @8.8.8.8
dig +short DS computeruse.site @1.1.1.1    # nothing
dig +short app.computeruse.site @1.1.1.1   # 8.231.155.139
dig +short app.computeruse.site @8.8.8.8
dig +short CAA computeruse.site @8.8.8.8   # nothing (or one that allows letsencrypt.org)
dig +trace +nodnssec sessions.computeruse.site | tail -5
```

Do not go on until both public resolvers give the address. Let's Encrypt
looks the challenge records up from the registry down; a delegation that has
not arrived fails the validation, and five failures in an hour lock the
account out for the rest of it.

At this point `https://app.computeruse.site` reaches Pomerium and gets a
certificate warning (the certificate is for browserjs.com) and no route.
That is expected and harmless.

Rollback: at Namecheap, Nameservers back to **Namecheap BasicDNS**.

### 3. OAuth apps: Google done, GitHub at the switch (you)

Dex's callback becomes `https://dex.computeruse.site/dex/callback`.

- **Google: done.** The new URI is on the OAuth client's Authorized redirect
  URIs, beside `https://dex.browserjs.com/dex/callback` and the localhost
  one. Google says a change takes from five minutes to a few hours to take
  effect, so it was made ahead. The app is in testing mode; the listed test
  users stay as they are.
- **GitHub: not now.** An OAuth App has one "Authorization callback URL" and
  cannot hold both. It is changed in step 6, in the browser, right before
  the deploy; from that moment GitHub sign-in on the old domain is broken,
  and changing it back is the rollback.

  Option, if GitHub sign-in on the old domain must keep working up to the
  deploy and through a rollback: register a second OAuth App with the new
  callback, and in step 6 put its ID and secret in the repository secrets
  `DEX_GITHUB_CLIENT_ID` and `DEX_GITHUB_CLIENT_SECRET` instead of editing
  the first app (the deploy writes them to the cluster on every run). The
  rollback is then the old pair of secrets, so keep them.

Verify: nothing until step 7. Rollback: remove the URI from Google.

### 4. Bring Phase 2 up to date (whoever maintains the pull request)

Rebase it on `main`. If other changes have added public names since
(the apex from #31), Phase 2 must move them too: after the
rebase

```sh
git grep -n 'browserjs\.com' -- deploy .github test site web backend terraform-provider-browserjs \
  | grep -v 'browserjs\.com/pool'
```

must print nothing, and `git grep -n 'browserjs-com' -- deploy` must print
nothing.

Then read its `infra plan` run (re-run the check if it ran before step 1's
apply): **No changes**. Not "0 to destroy": no changes at all. The outputs
`hostnames`, `certificate_dns_names`, `dns_zone_name` and `dns_name_servers`
change their values and `additional_dns_name_servers` now lists
browserjs.com; outputs are not infrastructure.

If the plan wants to destroy or replace a `google_dns_managed_zone` or a
`google_dns_record_set`: stop, do not merge.

### 5. Tell the users (you)

Two people. At the switch everyone is signed out, the app's address
changes, and every MCP connector has to be added again with the session's
new URL (see "What changes for sessions"). Pick a time when no agent is in
the middle of something.

### 6. The switch (you)

Expect a few minutes during which neither domain works: from the moment the
deploy applies the manifests until Let's Encrypt has issued the certificate
for the new names (typically two to five minutes; the deploy waits up to
fifteen).

1. Merge Phase 2. `infra apply` runs and changes nothing; wait for it to be
   green. Nothing that is served has changed yet.
2. GitHub, in the browser, right before the deploy: Settings, Developer
   settings, OAuth Apps, the app, "Authorization callback URL" from
   `https://dex.browserjs.com/dex/callback` to
   `https://dex.computeruse.site/dex/callback`, Update application. (Or, if
   a second app was made, swap the two secrets instead.)
3. Start the deploy on `main`:
   `gh workflow run deploy.yml --ref main -f confirm=deploy -f issuer=production`.
   Do **not** use `issuer=staging` to rehearse: it would replace the working
   certificate with an untrusted one.

What the run does, in order: applies the issuers (the solver's zone becomes
`computeruse-site`); applies `deploy/gke` (Pomerium's and Dex's
configuration, the backend's environment, the warm pool's template and the
Certificate with the new names, at once; the outage starts here); waits for
`certificate/pomerium-tls` to be Ready; sees that Pomerium serves another
certificate than the Secret holds and restarts it; restarts the backend so
that it fetches Pomerium's keys under the new name.

If the Certificate step fails: its log has the Challenge's reason. "TXT
record not found" or `NXDOMAIN`: step 2 has not propagated; wait and run the
deploy again (the manifests are already applied; only issuance is retried).
The old certificate stays in the Secret until a new one is issued, so
rolling back (below) restores service at once.

Rollback, at any point after the merge:

1. Revert the Phase 2 pull request and merge the revert. `infra apply`
   changes nothing (read the revert's plan: **No changes**).
2. GitHub: the callback URL back to `https://dex.browserjs.com/dex/callback`
   (or the old secrets back).
3. Run the deploy. If the certificate was never reissued, service is back as
   soon as Pomerium has its old configuration. If it was, cert-manager
   issues one for the old names again, in the old zone, which is still
   delegated: the same few minutes. (Let's Encrypt issues at most five
   certificates a week for the same set of names; each switch in either
   direction costs one.)

Sessions made while the new domain was live keep its URL in their pods (see
below); after a rollback their upload URLs are wrong. The script takes the
hosts from `OLD_HOST` and `NEW_HOST`, so it can point the other way, but the
workflow does not pass them: that would be a change to it.

### 7. Verify (you)

None of this needs `kubectl`. The deploy run's own log is the first check:
its "Certificate" step ends with `certificate.cert-manager.io/pomerium-tls
condition met`, and its summary shows the pods.

```sh
# DNS: the delegation, and every name at the address.
dig +short NS computeruse.site @1.1.1.1     # ns-cloud-d1 … d4.googledomains.com.
dig +short DS computeruse.site @1.1.1.1     # nothing
for h in api app authenticate dex sessions x.sessions; do
  printf '%-32s %s %s\n' "$h.computeruse.site" \
    "$(dig +short "$h.computeruse.site" @1.1.1.1)" "$(dig +short "$h.computeruse.site" @8.8.8.8)"
done                                        # 8.231.155.139 twice on every line

# The certificate that is served: issuer, dates, names.
echo | openssl s_client -connect app.computeruse.site:443 -servername app.computeruse.site 2>/dev/null |
  openssl x509 -noout -issuer -dates -ext subjectAltName
# issuer: Let's Encrypt (not "(STAGING)"); notBefore: today;
# DNS: api., app., authenticate., dex., sessions., *.sessions. of computeruse.site,
# and no browserjs.com name.

# Each host presents it (curl verifies the chain and the name).
for h in api app authenticate dex sessions s-smoke00000.sessions; do
  curl -sS -o /dev/null -w "%{http_code} %{ssl_verify_result} $h\n" "https://$h.computeruse.site/"
done                                        # ssl_verify_result 0 on every line

test/smoke.sh                               # the default is computeruse.site now; "0 failed"
curl -sS https://dex.computeruse.site/dex/.well-known/openid-configuration | grep -o '"issuer":"[^"]*"'
# "issuer":"https://dex.computeruse.site/dex"

# The old domain is not served: a certificate error, by name.
curl -sS -o /dev/null https://app.browserjs.com/ ; echo "exit $?"   # exit 60
```

For more than that (the Certificate's conditions, pods, logs):
`gh workflow run cluster-info.yml --ref main`, which changes nothing.

Then, in a browser:

1. <https://app.computeruse.site> redirects to
   `authenticate.computeruse.site`, then to `dex.computeruse.site`. Sign in
   with **Google**. The UI appears, with the sessions that were there
   before.
2. Sign out, sign in with **GitHub**. "redirect_uri mismatch" from either
   provider is step 3 not done for that provider.
3. Create a session. Its MCP URL (Copy MCP URL) is
   `https://sessions.computeruse.site/<id>/mcp`, and its screen connects.
4. `claude mcp add --transport http browserjs https://sessions.computeruse.site/<id>/mcp`:
   the browser opens `https://sessions.computeruse.site/.pomerium/mcp/authorize?…`,
   the tools list. Call `get_artifact_upload_url`: the URL it returns starts
   with `https://sessions.computeruse.site/<id>/api/artifact-uploads/`.
5. `https://app.browserjs.com`: a certificate error. That is the old domain
   not being served.

### 8. Afterwards

- Existing sessions and connectors: "What changes for sessions" below.
- Google: remove `https://dex.browserjs.com/dex/callback` from the OAuth
  client once a rollback is no longer on the table.
- Open pull requests: "What the open pull requests must change" below.
- The deprecated `*.sessions.` hosts were kept for URLs handed out under
  browserjs.com. Nobody was ever given one under computeruse.site, so they
  can be retired now by the procedure in `docs/session-urls.md` ("Old hosts
  (deprecated)").
- Namecheap: leave browserjs.com's nameservers alone for as long as going
  back should stay possible.

### 9. Phase 3, optional: DESTRUCTIVE (a pull request of its own)

Remove `previous_domain = "browserjs.com"` from
`infra/main/terraform.tfvars`. The plan: **7 to destroy** (the zone
`browserjs-com` and its six records), nothing else. After it, browserjs.com
resolves to nothing, and going back means making the zone again and setting
its new nameservers at Namecheap. In the same pull request or a later one,
delete the resources `google_dns_managed_zone.this` and
`google_dns_record_set.public`, the variable and the locals that only they
use from `edge.tf`: with `previous_domain` unset they make nothing.

Do it only once nothing is expected to come back to the old domain, and
decide first what the domain itself is for (let it lapse, park it, or keep
the zone for a redirect).

## What changes for sessions

**Kept.** The Sandboxes, their IDs, names, owners, disks (the browser's
profile, logged-in sites, artifacts), sleep and wake, snapshots.

**Signed out.** Everyone who signs in. Dex's issuer changes, Pomerium's sessions are
cookies on hosts that are no longer served, and Pomerium's MCP client
registrations and refresh tokens were issued for resources under the old
host.

**MCP URLs.** A session's URL is built from `SESSION_URL_TEMPLATE` each time
it is shown, so the UI shows `https://sessions.computeruse.site/<id>/mcp`
for old sessions too, and that URL works at once. The old URL does not, and
is not redirected. Every connector has to be added again:

- Claude Code: `claude mcp remove <name>`, then
  `claude mcp add --transport http <name> https://sessions.computeruse.site/<id>/mcp`.
- claude.ai: remove the connector, add it with the new URL, sign in.

**Upload URLs of sessions made before the switch.** This is the one thing
that does not follow by itself. mcp-js makes one-time upload URLs from
`MCP_V8_PUBLIC_URL`, which is written into a session's Sandbox when the
session is created, and a change to the blueprint only reaches new sessions
(`docs/gke-deployment.md`). So in a session made before the switch
`get_artifact_upload_url` goes on returning
`https://sessions.browserjs.com/<id>/api/artifact-uploads/…`, which nothing
serves. MCP itself, the screen and downloads are unaffected. Either accept
it for old sessions (anything new is fine), or rewrite the value with the
workflow `session public url` (`.github/workflows/session-public-url.yml`,
which runs `hack/session-public-url.sh`; no `kubectl` of your own needed):

1. See what it would do; this changes nothing:
   `gh workflow run session-public-url.yml --ref main -f session=all`.
   The run's summary lists each session that still has the old host as
   `WOULD` (stopped: would be changed) or `SKIP` (running or asleep).
2. Stop the session in the UI. A stop, not a sleep: only a session stopped
   by its user is changed. A running one is not touched because it is not
   known whether the controller would replace its pod; a sleeping one has a
   snapshot of a pod with the old value and would wake with it. A stop
   deletes the snapshots.
3. `gh workflow run session-public-url.yml --ref main -f session=s-… -f confirm=patch`
   (or `-f session=all` for every stopped session). The summary says
   `CHANGED`. The patch is a JSON patch that tests the old value before it
   replaces it, on that one environment variable and nothing else.
4. Resume the session: a cold start, with the tabs restored from disk. Check
   with `get_artifact_upload_url`: the URL starts with
   `https://sessions.computeruse.site/<id>/`.

UNVERIFIED on the cluster: the script is tested against made-up Sandboxes
only. Do one session that does not matter first. If the API server refuses
the patch (an admission policy, a field the controller owns), the run fails
and the Sandbox is unchanged; the fallback is to leave old sessions as they
are.

Delete the workflow and the script when no session from before the move is
left.

**The warm pool.** Its template changes (`MCP_V8_PUBLIC_URL`), so the pool
replaces its warm pod. A session created in that minute starts cold.

## What the open pull requests must change

Whichever lands second rebases. After the switch the rule is: no
`browserjs.com` in `deploy/`, `.github/`, `test/`, `site/` (the check in
step 4).

- **API tokens** (#43, merged; off in production): the API host moves with
  the rest, to `https://api.computeruse.site`, and the Terraform provider's
  default endpoint with it. A token is not tied to a host, but whatever
  holds the endpoint is: `BROWSERJS_ENDPOINT` or `endpoint =` set to the old
  host has to change. Before turning tokens on (`ALLOWED_EMAILS`), nothing
  else to do.
- **#31, the site on the apex**, and **#32**, its deploy: the certificate's
  name and Pomerium's route become `computeruse.site`. Its
  `google_dns_record_set.site` uses `google_dns_managed_zone.this[0]` and
  `var.domain`; after Phase 2 those are two different domains (`this` is the
  previous domain's zone), so it must use
  `local.zones[var.domain].name` instead. The VitePress configuration
  (hostname, canonical URLs, sitemap) and every `browserjs.com` in the
  content become `computeruse.site`; the product name in the text stays
  browserjs. The apex of browserjs.com has no record and gets none.
- **Policy tracks** (#44 and the rest): nothing of theirs names the domain
  except through the files above; rebase, and run the check in step 4.
