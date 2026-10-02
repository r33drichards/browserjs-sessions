# API tokens

For clients that cannot sign in through Pomerium: the Terraform provider,
scripts, CI. A user makes a token in the browser and gives it to the client;
the client sends it to a host of its own, `api.<domain>`, where the backend
checks it. Design: section 6.2 of
[plans/2026-10-02-session-policies-design.md](plans/2026-10-02-session-policies-design.md).
Contract: `/tokens` in
[contracts/policy/backend-api.yaml](contracts/policy/backend-api.yaml) and
[`deploy/base/crd-apitoken.yaml`](../deploy/base/crd-apitoken.yaml).

```
curl -H "Authorization: Bearer $BROWSERJS_TOKEN" https://api.browserjs.com/v1/sessions
```

## Turning it on

Two settings of the backend, both empty by default:

| `API_URL` | `ALLOWED_EMAILS` | |
|---|---|---|
| empty | any | Off. No token endpoints, no API host. Today's behaviour. |
| set | empty | The host of `API_URL` is the API's alone, and refuses every token. No token endpoints. This is `deploy/gke`. |
| set | set | On. This is `deploy/local`. |

`API_URL` is the API host's base URL with no path (`https://api.browserjs.com`);
it must be the `from` of the `api` route in the overlay's
`pomerium-config.yaml`. An overlay that has the route must set it: otherwise
the backend would take requests to that host for the app's.

`ALLOWED_EMAILS` is who may use the product, comma-separated: **the same
addresses as the `policy` in `pomerium-config.yaml`**. Pomerium is not asked
on the API host, so the backend has to know the list itself. It is checked
on every request, so taking a user off both lists ends their API access with
the backend's restart, not when their tokens expire. A test
(`backend/internal/auth/deploy_test.go`) fails when the two lists of an
overlay differ. Change both in one commit.

It also needs, from track B of the plan: `crd-apitoken.yaml` in
`deploy/base/kustomization.yaml`, and the backend's Role on `apitokens`
(get, list, create, delete) and `apitokens/status` (patch).

To turn it on in production: add `ALLOWED_EMAILS` to
`deploy/gke/patch-backend.yaml` and deploy.

## A token

`bjs_<id>_<secret>`, 60 characters.

- `id`: 12 characters of lower-case base32 (60 random bits). Public; it
  names the record and is what the logs and the token page show.
- `secret`: 43 characters of base64url, 32 random bytes from `crypto/rand`.

The backend keeps the SHA-256 of the whole string, in an `APIToken` custom
resource named `tok-<id>`, and nothing else: the token is in the answer to
its creation and nowhere after. Losing it means making another.

A token:

- **acts as its owner**: their sessions and their policies, nothing else. It
  is never an admin, even an admin's;
- **has scopes**: any of `sessions:read`, `sessions:write`, `policies:read`,
  `policies:write`;
- **expires**: 90 days by default, 365 at most, 1 at least;
- **cannot make or revoke tokens**.

A user has at most 20 unexpired tokens. Expired ones are deleted when their
owner next makes one.

## Making and revoking: `/api/tokens`, in the browser

On the app's host, signed in through Pomerium, as the UI's other calls.

| | |
|---|---|
| `GET /api/tokens` | The caller's tokens, newest first, without secrets. `?all=1` for an admin: everybody's. |
| `POST /api/tokens` | `{"name", "scopes", "expires_in_days"}`. `201` with the token object and `token`, the only time it is shown. `403` if the caller is not in `ALLOWED_EMAILS`, `409` at 20 tokens. |
| `DELETE /api/tokens/{id}` | Revokes. `204` whether or not there was one; somebody else's is left alone, unless the caller is an admin. |

A revoked token is refused by the very next request: every request reads the
token's record.

## Using one: the API host

`https://api.<domain>/v1/...`, with `Authorization: Bearer <token>`. What is
there, and the scope each needs:

| | Scope |
|---|---|
| `GET /v1/me` | any |
| `GET /v1/sessions`, `GET /v1/sessions/{id}` | `sessions:read` |
| `POST /v1/sessions`, `PATCH` and `DELETE /v1/sessions/{id}` | `sessions:write` |
| `GET /v1/sessions/{id}/policy` | `policies:read` |
| `PUT` and `DELETE /v1/sessions/{id}/policy`, `PUT /v1/sessions/{id}/policy/management` | `policies:write` |
| `POST /v1/policies/validate`, `POST /v1/policies/evaluate`, `GET /v1/policy-schema.json`, `GET /v1/policy-presets` | any |

They are the handlers of `/api/...`, with the token's owner as the caller.
The policy routes exist once the policy backend (track C) does; until then
they are 404.

Nothing else is served on that host: not the UI, not `/api/...`, not the
token endpoints, not VNC tickets, not file transfer, nothing of a session's.
The list is in `backend/internal/auth/apihost.go`; a route added to the app's
API is not on the API host until it is added there. Pomerium routes only
`/v1/` to the backend and adds nothing about the caller; the backend ignores
cookies and Pomerium's assertion on this host, and a bearer token on every
other.

| Answer | When |
|---|---|
| `401 {"error":"invalid token"}` | No token, or one that is malformed, unknown, revoked, expired, or whose owner is not in `ALLOWED_EMAILS`. Always the same answer. |
| `403` | The token lacks the route's scope. |
| `404` | Not one of the routes above (asked after the token is checked), or not the caller's session. |
| `429`, with `Retry-After` | Too many failed attempts from this address: 10 at once, then one every 10 seconds. |
| `503` | The token could not be checked (the cluster's API did not answer). |

The address a failure is counted against is the last entry of
`X-Forwarded-For`, which is Pomerium's own word for where the connection
came from; with none, the connection's address.

`lastUsedTime` in the record's status is written at most once an hour per
token.

## Where a token must not go

A token is the owner's authority over their sessions' policies. One pasted
into a web page, into `/data/memory`, or into an agent's prompt hands that
agent the policy that was meant to bound it. Keep it in the CI system's
secret store or the environment of the tool that uses it
(`BROWSERJS_TOKEN`).

## Local

`deploy/local` has it on, at `https://api.localtest.me`. The local
certificate has to name that host: a cluster made before this has a
certificate without it, so remove `.local/tls` and run `hack/local-up.sh`
again, or use `curl -k`.

```
# in the browser's console on https://app.localtest.me, signed in:
await (await fetch('/api/tokens', {method: 'POST', body: JSON.stringify({name: 'me', scopes: ['sessions:read']})})).json()

curl --cacert .local/tls/ca.crt -H "Authorization: Bearer bjs_..." https://api.localtest.me/v1/sessions
curl --cacert .local/tls/ca.crt https://api.localtest.me/v1/sessions     # 401
```

## Production pieces

| | |
|---|---|
| DNS | `api.<domain>`, an A record to the edge address: `api` in `local.public_names` of `infra/main/edge.tf` |
| Certificate | `api.browserjs.com` in `deploy/gke/certificate.yaml` |
| Route | `api` in `deploy/gke/pomerium-config.yaml` |
| Backend | `API_URL` in `deploy/gke/patch-backend.yaml` |
