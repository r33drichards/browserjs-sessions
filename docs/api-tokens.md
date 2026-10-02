# API tokens

For clients that cannot sign in through Pomerium: the Terraform provider,
scripts, CI, another service, an agent that is given a session's MCP
endpoint. A user makes a token in the browser and gives it to the client;
the client uses it on a host of its own, `api.<domain>`, where the backend
checks it. Design: section 6.2 of
[plans/2026-10-02-session-policies-design.md](plans/2026-10-02-session-policies-design.md).
Contract: [contracts/policy/backend-api.yaml](contracts/policy/backend-api.yaml)
and [`deploy/base/crd-apitoken.yaml`](../deploy/base/crd-apitoken.yaml).

## The whole flow

```
API=https://api.computeruse.site

# 1. Once, signed in, in the browser's console on https://app.computeruse.site
#    (or on the token page): make a token. It is shown this one time.
await (await fetch('/api/tokens', {method: 'POST', body: JSON.stringify(
  {name: 'ci', scopes: ['sessions:read', 'sessions:connect']})})).json()
# => {"id": "k3xw5qj2m7ab", "token": "bjs_k3xw5qj2m7ab_...", "token_url": "https://api.computeruse.site/oauth/token", ...}

CLIENT_ID=k3xw5qj2m7ab
CLIENT_SECRET=bjs_k3xw5qj2m7ab_...        # the token itself

# 2a. Use the token directly:
curl -H "Authorization: Bearer $CLIENT_SECRET" $API/v1/sessions

# 2b. Or exchange it for an access token of an hour (OAuth client credentials):
ACCESS=$(curl -s -u "$CLIENT_ID:$CLIENT_SECRET" -d grant_type=client_credentials \
  -d scope=sessions:connect $API/oauth/token | jq -r .access_token)

# 3. Call a session's MCP endpoint, with either:
curl -H "Authorization: Bearer $ACCESS" -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"curl","version":"0"}}}' \
  $API/s-abcdefghij/mcp
```

An MCP client is configured with the URL `https://api.<domain>/<id>/mcp` and
the header `Authorization: Bearer <token>`. The session's URL for people,
`https://sessions.<domain>/<id>/mcp`, is unchanged: there Pomerium signs the
user in, and a token is not accepted.

This is what the fleet backend offers (`libs/fleet/backend/handlers/svc.go`,
`user_keys.go`): a key is a client ID and a secret shown once, exchanged at
a `token_url` for a bearer token, which a proxy checks before passing the
request to the workload with `Authorization` removed; a key can be bound to
one namespace. Here the token is the client, the backend is the token
endpoint (there is no Keycloak), the workload is a session's mcp-js, and
the binding is to one session.

## Turning it on

Settings of the backend, empty by default:

| `API_URL` | `ALLOWED_EMAILS` | |
|---|---|---|
| empty | any | Off. No token endpoints, no API host. Today's behaviour. |
| set | empty | The host of `API_URL` is the API's alone, and refuses every credential. No token endpoints. This is `deploy/gke`. |
| set | set | On. This is `deploy/local`. |

`API_URL` is the API host's base URL with no path (`https://api.computeruse.site`);
it must be the `from` of the `api` routes in the overlay's
`pomerium-config.yaml`. An overlay that has the routes must set it: otherwise
the backend would take requests to that host for the app's.

`ALLOWED_EMAILS` is who may use the product, comma-separated: **the same
addresses as the `policy` in `pomerium-config.yaml`**. Pomerium is not asked
on the API host, so the backend has to know the list itself. It is checked
on every request, so taking a user off both lists ends their API access with
the backend's restart, not when their tokens expire. A test
(`backend/internal/auth/deploy_test.go`) fails when the two lists of an
overlay differ. Change both in one commit.

`API_SIGNING_KEY` signs access tokens: 32 random bytes in base64, read from
the Secret `api-tokens`, key `signing-key`, if there is one:

```
kubectl -n browserjs-sessions create secret generic api-tokens \
  --from-literal=signing-key="$(openssl rand -base64 32)"
```

Without the Secret the backend makes a key at each start. Access tokens then
end when the backend restarts and their clients ask for new ones, which an
OAuth client does on a 401. API tokens are not affected either way.
Changing the key ends every access token.

It also needs, from track B of the plan: `crd-apitoken.yaml` in
`deploy/base/kustomization.yaml`, and the backend's Role on `apitokens`
(get, list, create, delete) and `apitokens/status` (patch).

To turn it on in production: add `ALLOWED_EMAILS` to
`deploy/gke/patch-backend.yaml`, make the Secret, and deploy.

## A token

`bjs_<id>_<secret>`, 60 characters.

- `id`: 12 characters of lower-case base32 (60 random bits). Public; it
  names the record, is what the logs and the token page show, and is the
  OAuth `client_id`.
- `secret`: 43 characters of base64url, 32 random bytes from `crypto/rand`.

The backend keeps the SHA-256 of the whole string, in an `APIToken` custom
resource named `tok-<id>`, and nothing else: the token is in the answer to
its creation and nowhere after. Losing it means making another.

A token:

- **acts as its owner**: their sessions and their policies, nothing else. It
  is never an admin, even an admin's: an admin's token reaches the admin's
  own sessions only, on the API and on MCP;
- **has scopes**:

  | Scope | |
  |---|---|
  | `sessions:read` | list and read sessions |
  | `sessions:write` | create, rename, sleep, wake, stop, resume, delete sessions |
  | `sessions:connect` | call a session's MCP endpoint: drive its browser |
  | `policies:read` | read a session's policy |
  | `policies:write` | write a session's policy and its management mode |

- **may be for one session** (`session_id` at creation): it is then refused
  for every other session, and for listing and creating sessions;
- **expires**: 90 days by default, 365 at most, 1 at least;
- **cannot make or revoke tokens**.

A user has at most 20 unexpired tokens. Expired ones are deleted when their
owner next makes one.

`sessions:connect` and `policies:write` are best kept apart. A token with
`policies:write` handed to the agent that the policy is meant to bound lets
the agent rewrite the policy. Give an agent a token with `sessions:connect`
alone, bound to its session.

## Making and revoking: `/api/tokens`, in the browser

On the app's host, signed in through Pomerium, as the UI's other calls.

| | |
|---|---|
| `GET /api/tokens` | The caller's tokens, newest first, without secrets. `?all=1` for an admin: everybody's. |
| `POST /api/tokens` | `{"name", "scopes", "expires_in_days", "session_id"}`. `201` with the token object, `token` (the only time it is shown) and `token_url`. `403` if the caller is not in `ALLOWED_EMAILS`, `409` at 20 tokens. |
| `DELETE /api/tokens/{id}` | Revokes. `204` whether or not there was one; somebody else's is left alone, unless the caller is an admin. |

A revoked token is refused by the very next request, and so are the access
tokens made from it: every request reads the token's record.

## The API host

Three things are served on `https://api.<domain>`, and nothing else: not the
UI, not `/api/...`, not the token endpoints, not a session's screen, not
file transfer. Pomerium routes only these paths to the backend and adds
nothing about the caller; the backend ignores cookies and Pomerium's
assertion on this host, and ignores a bearer token on every other.

The credential is `Authorization: Bearer`, with an API token or an access
token.

### `/v1/...`: the API

| | Scope |
|---|---|
| `GET /v1/me` | any |
| `GET /v1/sessions`, `GET /v1/sessions/{id}` | `sessions:read` |
| `POST /v1/sessions`, `PATCH` and `DELETE /v1/sessions/{id}` | `sessions:write` |
| `POST /v1/sessions/{id}/sleep`, `POST /v1/sessions/{id}/wake` | `sessions:write` |
| `GET /v1/sessions/{id}/policy` | `policies:read` |
| `PUT` and `DELETE /v1/sessions/{id}/policy`, `PUT /v1/sessions/{id}/policy/management` | `policies:write` |
| `POST /v1/policies/validate`, `POST /v1/policies/evaluate`, `GET /v1/policy-presets` | any |

They are the handlers of `/api/...`, with the token's owner as the caller.
The policy routes exist once the policy backend (track C) does; until then
they are 404. The list is in `backend/internal/auth/apihost.go`; a route
added to the app's API is not on the API host until it is added there.

### `/<id>/mcp`: a session's MCP endpoint

The same path as on the sessions' host, and the same code behind it
(`backend/internal/proxy`): the API host checks the token, the scope
`sessions:connect` and the token's session, and hands the request to the
session proxy as the request to `https://sessions.<domain>/<id>/mcp` it
would have been, with the token's owner as the caller. So everything the
proxy does is done here too: the owner check (somebody else's session is
404), waking a sleeping session on a call and holding it awake, the event
stream on `GET`, the refusal of browser navigations and subresource loads
(`Sec-Fetch-Mode`), the hardening of what the pod answers, and the removal
of `Authorization` and cookies before the pod sees the request.

The upload URLs mcp-js hands out in tool results are on the sessions' host
(`https://sessions.<domain>/<id>/api/artifact-uploads/<token>`), where the
token in the path is the credential and no sign-in is asked for: a client
that got one over MCP can `PUT` to it as it is.

### `/oauth/token`: client credentials

`POST`, form-encoded, `grant_type=client_credentials`; `client_id` (the
token's `id`) and `client_secret` (the token) as HTTP Basic or in the form;
optionally `scope`, space-separated, a subset of the token's.

```
{"access_token": "eyJ...", "token_type": "Bearer", "expires_in": 3600, "scope": "sessions:connect"}
```

The access token is a JWT signed by the backend (HS256, `API_SIGNING_KEY`);
`iss` and `aud` are `API_URL`, `sub` the owner, `client_id` the API token.
It lasts an hour, or until the API token expires if that is sooner, and it
is only as good as the API token is at the moment of each request: the
record is read every time. There is no refresh token; ask again.

Both ways exist because they suit different clients. The token as a bearer
is the least a script or Terraform needs. The exchange is for clients that
already speak OAuth client credentials, and for handing a short-lived,
narrowed credential to something less trusted than where the token is kept.

### Answers

| Answer | When |
|---|---|
| `401 {"error":"invalid token"}` | No credential, or one that is malformed, unknown, revoked, expired, or whose owner is not in `ALLOWED_EMAILS`. Always the same answer. |
| `401 {"error":"invalid_client"}` | The same, at `/oauth/token`: unknown client and wrong secret are answered alike. |
| `400` | At `/oauth/token`: `invalid_request`, `unsupported_grant_type`, `invalid_scope`. |
| `403` | The token lacks the route's scope, or is for another session. |
| `404` | Not one of the routes above, or not the caller's session. |
| `429`, with `Retry-After` | Too many failed attempts from this address: 10 at once, then one every 10 seconds. |
| `503` | The token could not be checked (the cluster's API did not answer). |

The address a failure is counted against is the last entry of
`X-Forwarded-For`, which is Pomerium's own word for where the connection
came from; with none, the connection's address.

`lastUsedTime` in the record's status is written at most once an hour per
token.

## Where a token must not go

A token with `policies:write` is the owner's authority over their sessions'
policies. One pasted into a web page, into `/data/memory`, or into the
prompt of the agent the policy bounds hands that agent the policy. Keep
tokens in the CI system's secret store or the environment of the tool that
uses them (`BROWSERJS_TOKEN`).

## Local

`deploy/local` has it on, at `https://api.localtest.me`. The local
certificate has to name that host: a cluster made before this has a
certificate without it, so remove `.local/tls` and run `hack/local-up.sh`
again, or use `curl -k`.

```
curl --cacert .local/tls/ca.crt -H "Authorization: Bearer bjs_..." https://api.localtest.me/v1/sessions
curl --cacert .local/tls/ca.crt https://api.localtest.me/v1/sessions     # 401
```

## Production pieces

| | |
|---|---|
| DNS | `api.<domain>`, an A record to the edge address: `api` in `local.public_names` of `infra/main/edge.tf` |
| Certificate | `api.computeruse.site` in `deploy/gke/certificate.yaml` |
| Routes | `api`, `api-token`, `api-mcp` in `deploy/gke/pomerium-config.yaml` |
| Backend | `API_URL` in `deploy/gke/patch-backend.yaml`; `ALLOWED_EMAILS` when it is turned on |
| Secret | `api-tokens` (`signing-key`), optional |
