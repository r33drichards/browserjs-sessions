# Send tool calls to a webhook

Open a session and choose its **Webhook** tab. Enter a public HTTPS URL, then
choose how many events to send per batch and how long to wait before sending
a partial batch. Click **Save webhook**. Use **Disable webhook** to stop exports.

Events include outer tool calls such as `run_js`, plus nested browser and shell
authorization attempts, including denied calls. A single `run_js` request can
therefore produce several events. Events contain tool arguments; execution
results and screenshots are excluded.

## Filter which events are sent

Leave **Rego filter** empty to export every event. To select events, write a
module with `package browserjs.policy` and an `allow_tool_call` rule. The
filter receives one event as `input`, and includes it only when the rule
returns boolean `true`.

For example, send only denied authorization attempts:

```rego
package browserjs.policy
import rego.v1

default allow_tool_call := false

allow_tool_call if {
  input.stage == "authorization"
  input.allowed == false
}
```

To send only shell calls:

```rego
package browserjs.policy
import rego.v1

allow_tool_call if input.server == "exec"
```

Filters use the same language restrictions as [session policies](/reference/policy).
A filter selects exports; it does not change what the agent is allowed to do.

## Receive a batch

Your endpoint receives a JSON POST like this:

```json
{
  "version": 1,
  "batch_id": "unique-batch-id",
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

Return a 2xx status to acknowledge receipt. Delivery failures are retried up
to eight times. Deduplicate by event ID because retries can produce duplicates.
The batch ID also appears in `X-Computer-Use-Batch-ID`.

Outer calls have `stage: "request"` and `server: "mcp-js"`; they have no
`allowed` field. Browser and shell events have `stage: "authorization"`.
That stage records a policy decision, not whether execution succeeded.

## Verify signatures

Set an optional signing secret of 16–256 bytes. Each delivery then includes
`X-Computer-Use-Timestamp` (Unix seconds) and `X-Computer-Use-Signature`
(`sha256=<hex>`). Calculate HMAC-SHA256 over the timestamp, a period, and the
raw request body. Compare signatures in constant time and check the timestamp
against a tolerance suitable for your receiver.

Leaving the secret field blank when saving keeps the existing secret.

## Delivery limits

Batch size is 1–500 events, with a 2 MiB byte limit. The partial-batch interval
is 1–60 seconds. Nested calls first pass through OPA's 1–2 second collection
interval. Outer request events are uploaded after their proxied request finishes.
Queues are bounded and held in memory: restarts, queue pressure, oversized
events, filter failures, or exhausted retries can lose events. Changing or
disabling a webhook discards queued events. This export is best effort and
should not be used as a lossless audit log.

Outer arguments over 1 MiB are omitted and marked `arguments_truncated: true`.
Requests over 16 MiB and individual nested events exceeding the batch byte
limit are omitted.

Webhook endpoints must use HTTPS on port 443. Private addresses and redirects
are refused.

The API also supports `GET`, `PUT`, and `DELETE /v1/sessions/{id}/webhook`.
Reads require `sessions:read`; writes require `sessions:write`. PUT accepts
`url`, `batch_size`, `flush_interval_seconds`, optional `filter`, and optional
`signing_secret`. GET returns `has_signing_secret` in place of the secret.
