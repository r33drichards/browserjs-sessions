# Metrics

The backend serves Prometheus metrics at `/metrics` on a port of its own
(`METRICS_ADDR`, default `:9090`; `off` for none). The port is not in the
Service, Pomerium does not route to it, and the NetworkPolicy `backend`
opens it only to the collectors of Managed Service for Prometheus.

Nothing reads these back to decide anything. A collector that is down or
late changes nothing a user sees; why idleness in particular is not decided
from them is in [stateless-backend.md](stateless-backend.md#idleness-from-telemetry-instead).

No label carries an address or an owner: the repository and its logs are
public. The one label that grows with use is `session` (a session's ID), on
three gauges whose series exist only while the replica has something to say
of the session, and go within about a minute of its last use there or of
its deletion.

## What is served

| Metric | Type | Labels | Says |
|---|---|---|---|
| `browserjs_http_requests_total` | counter | `component`, `route`, `code` | requests answered |
| `browserjs_http_request_duration_seconds` | histogram | `component`, `route` | time to the last byte; a stream or viewer counts when it closes |
| `browserjs_http_requests_in_flight` | gauge | `component`, `route` | requests being answered now |
| `browserjs_session_last_activity_timestamp_seconds` | gauge | `session` | when this replica last saw the session used |
| `browserjs_session_open_connections` | gauge | `session` | connections and calls this replica holds open to it |
| `browserjs_session_calls_in_flight` | gauge | `session` | calls this replica is proxying to it |
| `browserjs_session_wakes_total` | counter | `result` | wakes by a request: `ok`, `refused`, `stopped`, `failed`, `timeout`, `error` |
| `browserjs_session_wake_duration_seconds` | histogram | `result` | from the wake to the pod answering |
| `browserjs_session_sleeps_total` | counter | `reason`, `result` | sleeps: reason `idle`, `sleep`, `credit`, `payment-method`, `blocked`; result `ok`, `changed`, `error` |
| `browserjs_session_sleep_duration_seconds` | histogram | `reason` | the sleep, snapshot included |
| `browserjs_policy_gate_held_total` | counter | | looks at a running session whose first policy was not in force |
| `browserjs_policy_gate_wait_seconds` | histogram | | how long a session was held for its first policy |
| `browserjs_activity_writes_total` | counter | `kind`, `result` | writes of activity to the cluster: the API-server load of the stateless design |
| `browserjs_leader` | gauge | | 1 on the replica that runs the passes |
| `browserjs_passes_total` | counter | `pass`, `result` | passes run: `idle`, `billing`, `billing-delete`, `balance` |

Plus Go's and the process's own (`go_*`, `process_*`).

`component` and `route` are a fixed list (`metrics.Classify`): `proxy` with
`mcp`, `mcp_stream`, `upload`, `vnc`, `files`, `other`; `api` with
`sessions`, `session`, `session_sleep_wake`, `session_policy`, `vnc_ticket`,
`billing`, `tokens`, `policies`, `token_exchange`, `other`; `web/page`;
`other/healthz`, `other/webhook`.

The per-session gauges are one replica's view. Across replicas, take the
`max` of the timestamp and the `sum` of the other two, by `session`. The
sum over all replicas of `browserjs_leader` should be 1.

For a canary: error ratio is
`sum(rate(browserjs_http_requests_total{code=~"5.."}[5m])) / sum(rate(browserjs_http_requests_total[5m]))`,
by `pod`; latency is `histogram_quantile` over
`browserjs_http_request_duration_seconds_bucket` for `route="mcp"`; wakes
that fail are `browserjs_session_wakes_total{result!="ok",result!="refused"}`.

## Collecting them on GKE

`deploy/gke/monitoring.yaml` is a `PodMonitoring` for the backend, scraped
every 30 s by Managed Service for Prometheus. No Prometheus is self-hosted.

It is **not applied yet**: its line in `deploy/gke/kustomization.yaml` is
commented out, because on a cluster that does not serve the resource the
whole apply fails, and this cluster was not looked at. To turn it on:

1. Check that managed collection is on:
   `kubectl get crd podmonitorings.monitoring.googleapis.com` and
   `kubectl -n gmp-system get daemonset collector`. GKE turns it on by
   default for Standard clusters created at 1.27 or later.
2. `infra/main/cluster.tf` does not say either way. To make it explicit, the
   cluster resource takes
   `monitoring_config { managed_prometheus { enabled = true } }`
   (`monitoring.googleapis.com` is already in `apis.tf`). On a cluster where
   it is already on, that changes nothing.
3. Uncomment `- monitoring.yaml` and deploy.

The NetworkPolicy lets in pods labelled `app.kubernetes.io/name: collector`
in namespace `gmp-system`. That label was not checked against a cluster: if
the target shows as down in Cloud Monitoring, compare it with
`kubectl -n gmp-system get pods --show-labels`.

### Cost

Managed Service for Prometheus charges per sample ingested, about $0.06 per
million at this volume (check the current price). One replica serves about
500 series with 20 sessions (most of them histogram buckets); at one scrape
every 30 s that is about 43 million samples a month, so **about $2.60 a
month per replica**, and about $0.25 more per 20 sessions. The collectors
already run. These figures are estimates, not a bill.
