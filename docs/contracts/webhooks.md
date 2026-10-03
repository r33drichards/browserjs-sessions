# Session tool-call webhooks

Configure one webhook per session in its **Webhook** tab, or through the API:

```
PUT /v1/sessions/{id}/webhook
Authorization: Bearer <API token with sessions:write>
Content-Type: application/json

{
  "url": "https://example.com/tool-calls",
  "batch_size": 100,
  "flush_interval_seconds": 5,
  "signing_secret": "at-least-sixteen-bytes",
  "filter": ""
}
```

The app uses the corresponding `/api/sessions/{id}/webhook` routes. GET
requires `sessions:read` and returns the configuration (or null when disabled),
with `has_signing_secret` in place of the secret. PUT validates the complete
configuration and filter before saving; it returns 204. Omit `signing_secret`
to retain the existing value, or send an empty string to clear it. DELETE
requires `sessions:write`, returns 204, and disables exports. Session ownership
and session-bound tokens apply to all three routes.

Settings are persisted as `SessionPolicy.spec.webhook`. The policy operator
validates them again, including for direct Kubernetes writes. Invalid direct
writes refuse new captured calls for that session until corrected. Settings apply when reconciled;
changing or disabling a destination affects new capture only. Previously
accepted events retain their original destination, filter, and signing secret
and continue delivery until acknowledged. Configured tool calls depend on a
durable capture commit; ingestion failures refuse execution.

## Events and batches

Outer MCP `tools/call` requests, including `run_js`, are durably recorded by
the backend before they are forwarded. Nested browser and shell authorization attempts,
including denied attempts, are recorded inline by the session-facing decision gateway. These are distinct
events: one `run_js` can produce several nested authorization events. Results,
screenshots, and return values are not exported. An authorization event records
the decision, not execution success. If the gateway cannot reach the engine,
it records a refused attempt with `decision_error` before returning failure.

The destination receives an uncompressed JSON POST:

```json
{
  "version": 1,
  "batch_id": "7ddc473b-8b51-4190-bf2a-a7e91e3b09b7",
  "session_id": "s-abcdefghij",
  "events": [{
    "id": "unique-event-id",
    "session_id": "s-abcdefghij",
    "timestamp": "2026-10-03T12:00:00Z",
    "type": "tool_call",
    "stage": "authorization",
    "server": "exec",
    "tool": "exec",
    "arguments": {"bin": "git", "args": ["status"]},
    "allowed": true
  }]
}
```

Outer calls have `stage: "request"`, `server: "mcp-js"`, and a JSON-RPC
`request_id`, with no `allowed` field. Outer arguments over 1 MiB are omitted
and marked `arguments_truncated: true`. Requests over 16 MiB are refused before forwarding. Nested arguments too
large for a 2 MiB delivery are omitted with `arguments_truncated: true`; the
event itself is retained.

A batch contains at most `batch_size` events (1–500; default 100), also capped
at 2 MiB. Partial batches wait `flush_interval_seconds` (1–60; default 5).
Full batches wake delivery immediately. Nested events are durably recorded
before an authorization verdict reaches mcp-js. Filter evaluation and delivery
retries add latency. Ordering across independent destination versions is not
guaranteed.

## Rego filters

An empty filter includes every event. Otherwise, use the existing restricted
Rego policy contract: `package browserjs.policy`, define `allow_tool_call`,
no references to `data`, no `with`, and the enforcement capabilities allowlist.
The filter's `input` is one event from the schema above. Only boolean `true`
includes it; false, undefined, or other values omit it. Evaluation errors and
timeouts retain events and retry evaluation; they never discard events.
Filtering selects delivery without changing the authorization verdict.

Export only denied browser or shell calls:

```rego
package browserjs.policy
import rego.v1

default allow_tool_call := false

allow_tool_call if {
  input.stage == "authorization"
  input.allowed == false
}
```

Export outer calls and shell calls:

```rego
package browserjs.policy
import rego.v1

allow_tool_call if input.server in {"mcp-js", "exec"}
```

## Signing and delivery

Endpoints must use public HTTPS on port 443. Credentials in the URL, private
IP literals, redirects, and DNS answers containing non-public addresses are
refused. DNS checks apply to actual connection resolution, with no DNS cache
or environment proxy. TLS verification remains enabled.

Every delivery has `X-Computer-Use-Batch-ID`. With a secret configured it also
has `X-Computer-Use-Timestamp` (Unix seconds) and `X-Computer-Use-Signature`
(`sha256=<hex>`). Verify HMAC-SHA256 over `timestamp + "." + raw request body`,
using constant-time comparison and a suitable timestamp tolerance. Each retry
uses the same body and batch ID, with a fresh timestamp/signature.

A 2xx response acknowledges delivery. Every other status and transport failure
retries indefinitely, with exponential delays capped at five minutes and a
10-second HTTP timeout. Retry count and next-attempt time survive restarts.
The same persisted batch ID and body are used for every retry. A crash after
the receiver accepts but before acknowledgement is committed replays the batch.

This is **durable at-least-once delivery** for accepted, filter-selected events.
Receivers must deduplicate event IDs transactionally with their own processing
to obtain exactly-once effects. No HTTP sender can ensure exactly-once receiver
processing when an acknowledgement can be lost.

## Durable capture and deployment

The singleton policy operator uses Redis Streams with a durable prepared-batch
record. The bundled Redis StatefulSet retains its 10 GiB PVC, uses AOF with
`appendfsync always`, and forbids eviction. Each atomic queue mutation is followed
by `WAITAOF 1 0 2000` on the same connection before acceptance is acknowledged.
Pending events, prepared batches, destination snapshots, retry state, and receipts
survive Redis and operator process crashes. External Redis must support WAITAOF
(Redis 7.2+) and use these persistence and eviction settings; startup refuses a
weaker configuration. Set `WEBHOOK_REDIS_URL`, `WEBHOOK_REDIS_PASSWORD`, and
optionally `WEBHOOK_REDIS_PREFIX` to configure it. The offline CLI does not connect
to Redis and never captures live calls.

The `opa` Service retains its existing name and port but routes to the
operator's inline decision gateway. The actual OPA replicas are behind
`opa-engine`; only the operator can reach them. Session network policies forbid
bypassing the recorder. The operator discovers replica EndpointSlices for
`opa-engine`. OPA asynchronous decision logging is no longer the capture source.
The authenticated `/logs` endpoint remains compatible with older log producers,
but those producers' in-memory buffering is outside the durable capture guarantee.

For configured outer calls, the backend checks the saved webhook configuration
and requires the collector to have applied that same configuration before it
acknowledges capture. It refuses the call on recorder unavailability, disk
failure, or capacity pressure. The gateway likewise refuses a successful nested
decision until its event is committed. An event may be recorded for a call that
subsequently fails or never executes; events describe attempts, not successful
execution.

The outbox refuses new acceptance when pending event bytes would exceed 256 MiB
or Redis cannot persist the mutation. It never evicts an accepted event. Captured calls are
therefore refused rather than silently lost. A permanently unreachable endpoint
or a permanently failing filter retains its backlog until repaired, and can
eventually prevent new captured calls. Successful filter exclusions are an
intentional terminal disposition and are not delivered. Completed event IDs
remain as durable receipts; storage exhaustion rejects new acceptance.

Disabling a webhook stops new capture but does not cancel previously accepted
batches. Changing the URL or signing secret applies to newly accepted events;
old events keep their original configuration. Deleting a session also preserves
its accepted backlog.

Deploy the updated CRD, network policies, both OPA Services, backend, and
policy-operator image together. The recorder becomes part of the authorization
path: while the singleton operator is unavailable, nested decisions are refused,
including for sessions without exports. Do not use `emptyDir` for Redis or
scale the operator above one replica. Redis has a 1 GiB memory limit and no
automatic failover; provision more memory and storage as receipt history grows.
The guarantee assumes Redis and its PVC honour successful fsync; volume destruction or storage
corruption requires backup recovery.
