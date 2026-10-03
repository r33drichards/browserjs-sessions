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
writes disable exports for that session. Settings apply when reconciled;
changing or disabling a destination discards its queued events, so old events
are not forwarded to a new destination. Enforcement does not depend on exports.

## Events and batches

Outer MCP `tools/call` requests, including `run_js`, are observed by the backend
as their body is forwarded. Nested browser and shell authorization attempts,
including denied attempts, come from OPA decision logs. These are distinct
events: one `run_js` can produce several nested authorization events. Results,
screenshots, and return values are not exported. An authorization event records
the decision, not execution success. Attempts that never reach a policy engine
cannot produce an authorization event.

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
and marked `arguments_truncated: true`. The proxy captures up to 16 MiB of a
request body; requests over that limit are not exported. Individual nested
events too large for a 2 MiB delivery are omitted with a warning.

A batch contains at most `batch_size` events (1–500; default 100), also capped
at 2 MiB. Partial batches wait `flush_interval_seconds` (1–60; default 5).
Outer request events are uploaded after the proxied request finishes.
OPA first collects nested calls in gzip batches every 1–2 seconds; receipt,
filter evaluation, and retries add latency. Ordering across outer requests and
OPA replicas is not guaranteed.

## Rego filters

An empty filter includes every event. Otherwise, use the existing restricted
Rego policy contract: `package browserjs.policy`, define `allow_tool_call`,
no references to `data`, no `with`, and the enforcement capabilities allowlist.
The filter's `input` is one event from the schema above. Only boolean `true`
includes it; false, undefined, or other values omit it. Evaluation errors and
timeouts omit the batch and log a warning. Filtering never changes enforcement.

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

A 2xx response acknowledges delivery. Other statuses and transport failures
are retried up to eight attempts, with exponential delays capped at 60 seconds
and a 10-second HTTP timeout. Deduplicate by event ID; the same event can arrive
again after ingestion retries or collector restarts.

This is a **best-effort export**, not a durable audit log. The operator holds
at most 16 MiB of queued event bytes in memory. It returns 503 on queue pressure
so OPA retries ingestion, but OPA also has a bounded 8 MiB in-memory buffer.
Outer request uploads have at most eight simultaneous background requests and
are omitted when busy or the collector is unavailable. Restarts, oversized
events, filter errors, configuration changes, and exhausted retries can lose
events. Warnings record local drops and failed deliveries. A durable delivery
store is required before promising lossless exports.
