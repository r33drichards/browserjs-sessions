# Usage collection and storage: options and recommendation

Research only: no product code, no cloud resources, nothing deployed. It
answers the product owner's feedback on the merged billing design
([design](2026-10-02-metering-billing-design.md),
[tracks](2026-10-02-metering-billing-tracks.md),
[contracts](../contracts/billing/README.md)):

> "I don't think the backend's in-memory copy should exist. It probably also
> shouldn't have the CRD in the cluster either, but let's just keep that
> [for now]. What's the usage counts, and where are we storing that? It
> might be just better to look for off-the-shelf and existing solutions for
> usage and collecting it. [...] Maybe we just send a webhook to another
> service of ours whose sole purpose is to collect and store usage. Maybe we
> can just store that in Bigtable."

Prices were read on 2026-10-02 from the vendors' pages, region us-west1
where it applies; monthly figures are the hourly rate times 730. Anything
not confirmed from a primary source that day is marked **UNVERIFIED**.
Marketing pricing pages (not Google's) were read through a page summariser:
check the number on the linked page before committing money to it.

---

## 1. Recommendation (the one page)

**Build the small usage service the owner describes, and store it in
Firestore, not Bigtable.**

- **A `usage` service of ours** (one small Go Deployment) is the only
  writer and reader of usage and credit. It takes usage events over HTTP,
  deduplicates them, applies the metering step that is already specified
  and tested (`metering.md`, 18 vectors), and answers "may this account
  start, or keep running?" from the store on every question. It holds
  nothing in memory between requests.
- **Storage: Firestore (Native mode, the project's default database, in
  us-west1).** $0 a month today, about $3 to $6 a month at 1,000 accounts,
  nothing to run, strongly consistent, with the transactions a balance
  needs. **Bigtable is the wrong tool at this size: its minimum is one node
  at $0.65 an hour, $474.50 a month, 2.7 times the project's whole fixed
  bill of $178, before a byte is stored**, and it has no free tier and
  cannot scale to zero. Firestore gives the same shape (a serverless
  Google key-value store with key-ordered scans) for nothing.
- **The backend's in-memory copy goes.** The balance is read from the
  usage service at each decision. That is affordable because the check
  runs only at create, resume and wake (section 5), each of which already
  takes seconds; it does not run on every proxied call.
- **Who reports usage: an observer, by heartbeat.** The component that
  watches Sandboxes every 60 s stays, and posts what it saw to the usage
  service instead of writing `Account.status`. Start and stop events from
  the backend are not used: a lost "stop" would bill for ever, a lost
  heartbeat only gives free time (section 6).
- **CRDs "for now": `Account` only** (identity, Stripe customer, card
  state, flags: what the backend writes). Grants and usage periods move
  into the usage store with the events, because a balance is grants minus
  usage and both must change in one transaction in one place.
- **Off the shelf: not now.** No product removes what we still have to
  build (the gate, the stop sequence, Stripe payment to credit). The best
  semantic match, OpenMeter, needs Kafka, ClickHouse, Postgres and Redis
  self-hosted, which does not fit one small system node; Stripe's own
  credits apply only when an invoice is finalised and cannot refuse
  anything. Events are sent in CloudEvents form so that moving to OpenMeter
  (hosted, from about $25 a month) later is a change of destination, not of
  design.

**What it changes in the current build** (section 7): the `Ledger`
interface of `testing.md` keeps its shape and gets an HTTP implementation
instead of an informer; track A's operator shrinks to the observer; a new
small track builds the service; track B deploys it and adds Firestore to
`infra/main`; tracks C, E, F and G do not change. The fakes and the
card-gate scenarios are untouched.

### Table 1: build or buy

| Option | Runs where, on what | Cost at our size | Balance check | Grants with expiry | Stripe | Still ours to build | Verdict |
|---|---|---|---|---|---|---|---|
| **Our usage service + Firestore** | 1 small pod; Firestore | $0 now, ~$3 to $6/mo at 1,000 accounts | strongly consistent point read, in the transaction that debits | yes: the step of `metering.md`, as specified | payments only, as designed | the service (the step is specified and has vectors) | **recommended** |
| OpenMeter, self-hosted (Apache-2.0) | 6 processes + Kafka, ClickHouse, Postgres, Redis, Svix (11 containers in its quickstart) | compute we do not have | `GET .../entitlements/{feature}/value` gives `hasAccess`, `balance`; asynchronous pipeline, lag undocumented | yes: priority, expiry, rollover | native (invoices, Checkout, portal) | dollars as a unit, payment to grant, monthly disk charge, the gate, the stop sequence | best semantics, does not fit the cluster |
| OpenMeter hosted (Kong Konnect Metering & Billing) | SaaS | from $25/mo + $20 per million events + 0.4% of billing volume; 30-day trial, no free tier found | the same API, over the internet | yes | native | the same | the fallback if we stop wanting to own this |
| Lago, self-hosted (AGPL-3.0) | 7 containers; Postgres + Redis | compute only | wallet `balance` moves when an invoice is finalised; the per-minute "ongoing balance" is a **Premium** (paid, quote-only) feature | per wallet (up to 5 wallets per customer) | native | a plan and subscription per account, the gate, a live balance of our own | no: the live balance is behind the paid licence |
| Stripe Billing Meters + Credit Grants | SaaS | 0.7% of Billing volume | `credit_balance_summary`, but credits "apply to invoices only at the time of finalization"; meter events are asynchronous | yes: priority, expiry, max 100 unused grants | it is Stripe | **the whole live balance**, plus the gate; needs a metered subscription per customer | no: cannot enforce, not prepaid |
| Autumn Cloud (useautumn; OSS is Apache-2.0) | SaaS on top of Stripe | free to $8K monthly billing volume, then $375/mo; Stripe's 0.7% on top | synchronous `balances.check`, with a reserve and finalise flow | credits yes; burn order between plan and top-up credit undocumented | built on it (Checkout for top-ups) | the gate, the stop sequence, the disk charge | credible hosted option; young company; a remote call on the wake path |
| Flexprice (AGPL-3.0) | Postgres, Kafka, ClickHouse, Redis, Temporal + 3 processes | hosted: free 100k events/mo; prepaid credits are on the $500/mo tier | `/wallets/:id/balance/real-time` | yes | yes | the gate | no: heaviest stack, credits on the paid tier |
| Kill Bill (Apache-2.0) | JVM, "at least 4GiB of RAM", SQL database | compute | invoice-centric; a wallet with expiry exists only in the paid Aviate add-on | no (OSS) | plugin | most of it | no |
| Metronome (now Stripe's) | SaaS | 0.8% of billing volume + $0.04 per 1,000 events (about $120/mo at 3M events); minimum UNVERIFIED | balance API; alerts "within minutes" | yes | native | the gate | no: priced for larger companies |
| Orb | SaaS | sales only, UNVERIFIED | credits balance "as soon as events are ingested" | yes | yes | the gate | no: no public price |
| Amberflo | SaaS | $99/mo capped at 10k events; $599/mo for 500k | UNVERIFIED | UNVERIFIED | UNVERIFIED | the gate | no: event caps |

Also looked at and set aside: Polar.sh (a merchant of record that replaces
Stripe Checkout, 5% + 50¢, and "doesn't block usage if the customer
exceeds their balance"), Stigg and Schematic (hosted entitlements; free
tiers exist, credit semantics UNVERIFIED), Lotus (last release March 2023),
Unkey (no per-customer credit ledger). Sources in section 8.

### Table 2: where to store it (us-west1, 2026-10-02)

"Today" is 2 accounts. "1,000 accounts" assumes 200 sessions awake on
average 4 hours a day and 2,000 kept sessions (the arithmetic is in
section 4).

| Store | Minimum $/month | Today | At 1,000 accounts | Transactions for a balance | Pods and ops for us | Verdict |
|---|---|---|---|---|---|---|
| **Firestore Native, Standard** | $0 | $0 | ~$3 to $6 | yes, serializable, multi-document | none | **events and balances** |
| Bigtable, 1 node | **$474.50** + $0.17/GiB SSD | ~$475 | ~$476 | single-row only (Bigtable docs, not re-read today) | none | no: 2.7x the whole bill |
| Cloud SQL Postgres `db-f1-micro` | $9.37 (10 GiB SSD) | $9.37 | ~$10 | yes | proxy or connector; **shared core, no SLA** | second choice if we want SQL |
| Cloud SQL Postgres `db-custom-1-3840` | $51.01 (HA $102.02) | $51 | ~$51 | yes | the same; smallest with an SLA | too much for two users |
| Postgres in the cluster, 10 GiB disk | $1.00 (pd-balanced) + the pod | ~$1 | ~$1.50 | yes | **a stateful pod on the one system node; backups, upgrades and restore are ours** | no: money data on a pet |
| Spanner, 100 processing units | $65.70 + $0.30/GiB | ~$66 | ~$69 | yes | none | no |
| AlloyDB, 1 vCPU / 8 GB | $113.65 + $0.30/GiB | ~$114 | ~$117 | yes | none | no |
| BigQuery | $0 | $0 | ~$0 ingest and storage | no; seconds per query | none | later, as the analytics copy only |
| Cloud Storage as an append log | $0 | $0 | ~$16 unbatched | none across objects | none | export target, not the ledger |
| Memorystore Redis, 1 GiB basic | $35.77 | $36 | $36 | not durable | none | no |

### Decisions for the product owner

| # | Decision | Recommended default |
|---|---|---|
| 1 | Build the usage service, or adopt a product | Build. Send events in CloudEvents form so OpenMeter stays an option. |
| 2 | Storage | Firestore Native, default database, us-west1. Not Bigtable ($474.50 a month minimum). |
| 3 | What stays a CRD "for now" | `Account` only (what the backend writes). `Grant` and `UsagePeriod` are never deployed: credit and usage live in the usage store. |
| 4 | Who reports usage | The observer, by heartbeat every 60 s for awake sessions. No start/stop events. |
| 5 | Disk charge cadence | Hourly per kept session instead of every 60 s (it is $1.40 a month; per-minute writes for sleeping sessions would be most of the Firestore bill). A contract change to `metering.md`. |
| 6 | Usage service unreachable | Wake and resume are allowed, create is refused with 503 `metering_unavailable`, nothing is stopped. Unrecorded time is free. |
| 7 | Observer: its own process or inside the usage service | Its own (track A's operator, reduced), as the owner described: "send a webhook to another service". |
| 8 | The wake rate limit and the cluster cap (rows 11 and 12 of `enforcement.md`) count in the backend's memory | Leave them: they are rate limiters that reset safely, not a copy of anyone's money. |
| 9 | A copy in BigQuery for analysis | Not now. Firestore's managed export loads into BigQuery when wanted. |
| 10 | Backups | Firestore point-in-time recovery (7 days) and a daily backup schedule; this replaces the `billing-export` CronJob for usage and credit. |

---

## 2. Off-the-shelf systems, measured against this need

The need: usage events in; a prepaid dollar balance per customer made of
grants with expiry; a quick and reliable "may this customer start or keep
running?"; Stripe for collecting money; tiny scale; runs on a cluster with
one small system node and a 12-CPU quota, or is hosted free or cheaply.

**None of them removes our enforcement.** Every one leaves us the refusal
at create and wake, the stop sequence, and the glue from a Stripe payment
to a credit. What a product would replace is the ledger: event dedupe,
the burn-down, the balance read. That part is about a page of specified,
vector-tested arithmetic.

### OpenMeter

- Kong acquired OpenMeter (announced 2025-09-03); OpenMeter Cloud is now
  "Konnect Metering & Billing". The open-source repository is active
  (release `v1.0.0-beta.235` on 2026-10-02, still tagged beta), Apache-2.0,
  about 2,400 stars.
- Stores: usage events in ClickHouse, customers, entitlements and billing
  in Postgres, with Kafka between; Redis for dedupe; Svix for webhooks.
  The quickstart runs 11 containers (six of its own processes). The Helm
  chart's bundled Kafka and ClickHouse are "for development deployments".
  No minimum CPU or memory is documented.
- Credit: metered entitlements with grants (`amount`, `priority`,
  `effectiveAt`, `expiration`, rollover, recurrence); burn order is
  priority, then nearest expiry, then oldest: our order, with a priority
  in front.
- Balance: `GET /api/v2/customers/{id}/entitlements/{feature}/value`
  returns `hasAccess`, `balance`, `usage`, `overage`. "Current values are
  always real time", but the path is API, Kafka, worker, ClickHouse:
  eventually consistent, and the lag is not documented (UNVERIFIED).
- Dedupe: CloudEvents `source` + `id`; 32 days with the Redis driver; off
  unless configured.
- Stripe: a native app (invoices, tax, Checkout, portal). A "buy a pack,
  get a grant" flow was not found (UNVERIFIED either way).
- Entitlements count meter units, not dollars: we would meter a cost value
  or minutes, and handle the monthly disk charge as a periodic event.
- Hosted: Konnect Plus "from $25/month plus usage", metering "$20 per
  million events", billing "0.4% of billing volume"; a 30-day trial; no
  permanent free tier found.

### Lago

- AGPL-3.0, about 10,600 stars, release `v1.53.0` on 2026-09-08. The
  lightest to self-host: Postgres and Redis, seven long-running containers
  (API, front end, worker, clock, PDF, database, Redis). ClickHouse and
  Kafka only for very high volume.
- Wallets: up to 5 per customer with priority and `expiration_at`, paid and
  granted credits, recurring top-ups.
- The catch: `balance_cents` "updates each time an invoice is finalized".
  The "ongoing balance" that "refreshes every minute" is marked a premium
  feature, "only available to users with a premium license", and Premium
  has no published price. Credits apply to subscription invoices only, so
  every account needs a Lago plan and subscription.
- Dedupe: `transaction_id` (with the subscription); a duplicate is a 422.
- Stripe: native (customer sync, a payment intent per invoice, checkout
  link for a card, top-up credited when "payment is confirmed").

### Stripe Billing Meters and Credit Grants

- Credit Grants have `expires_at`, `priority`, `category`, `effective_at`;
  at most 100 unused grants per customer.
- `GET /v1/billing/credit_balance_summary` returns an available and a
  ledger balance. But "credits apply to invoices only at the time of
  finalization", and meter event summaries "might not immediately reflect
  recently received meter events". **The number Stripe can give us is not
  the live balance.**
- **It cannot refuse anything.** There is no gate; alerts are webhooks on a
  usage threshold, and credit-balance alerts were still an early-access
  sign-up.
- Not prepaid: grants apply only to subscription items with metered
  prices, so every customer needs a metered subscription, and a paid pack
  is our own flow (one-off invoice, `invoice.paid`, create the grant).
- Meter events: `identifier` is unique "within a rolling period of at
  least 24 hours"; 1,000 calls a second; timestamps within 35 days.
- 0.7% of Billing volume.
- So the design's first sentence stands: "Stripe does not stop anything.
  Stopping is ours." Stripe stays payments only. Mirroring usage to a
  Stripe meter later, for the customer's invoice history, is possible and
  is not needed.

### The rest

- **Flexprice** (AGPL-3.0, about 6,900 stars): wallets with expiry and
  priority and a `/balance/real-time` endpoint, on Postgres, Kafka,
  ClickHouse, Redis and Temporal. Hosted: free for 100k events a month, but
  prepaid credits and the Stripe integration are listed on the $500 a
  month tier.
- **Kill Bill** (Apache-2.0): a JVM that wants 4 GiB and bills in arrears
  by invoice; the wallet with expiry is in the paid Aviate add-on.
- **Metronome** (acquired by Stripe, completed 2026-01-14 per a secondary
  source): 0.8% of billing volume plus $0.04 per 1,000 events; 34-day
  dedupe window. **Orb**: balance updates on ingestion; price by sales
  only. **Amberflo**: $99 a month capped at 10k events, which one session
  awake for a week exceeds.
- **Autumn**: the closest hosted fit. A synchronous check
  (`POST /v1/balances.check`, optionally check and record in one call, and
  a reserve then finalise flow), built on Stripe including Checkout for
  top-ups, free up to $8K of monthly billing volume. Against it: a young
  company with no tagged releases, an undocumented burn order between plan
  credit and top-ups, a call over the internet on the wake path, and
  self-hosting needs Postgres, Redis, ClickHouse and Supabase.

---

## 3. The usage service

One Deployment, `usage`, in the `browserjs-sessions` namespace; Go, in the backend's
module (`backend/cmd/usage`, `backend/internal/usage`), because track D
already ports the metering step to Go for its fake ledger and the vectors
run against it. Reached only from inside the cluster (NetworkPolicy: the
observer and the backend in; Firestore's API out). No state in the
process: two replicas are safe, one is enough.

### Events

CloudEvents 1.0 JSON, so the same body is accepted by OpenMeter if we ever
move.

```json
{
  "specversion": "1.0",
  "type": "session.observed",
  "source": "observer/<cluster>",
  "id": "<session id>/<tick as unix seconds>",
  "time": "2026-10-02T10:01:00Z",
  "subject": "<account name: the owner hash>",
  "data": {
    "session": "<session id>",
    "awake": true,
    "readySince": "2026-10-02T09:58:12Z",
    "diskGB": 5
  }
}
```

- `id` is the idempotency key: the session and the tick it was seen at.
  The same observation posted twice is one event.
- `type`: `session.observed` (every 60 s, awake sessions) and
  `session.kept` (hourly, every session that exists, for the disk charge).
  A session that is asleep produces no per-minute events.
- It is an **observation at an instant**, not an interval and not a start
  or a stop. The service turns two consecutive observations into charged
  seconds with the gap rule of `metering.md` (`MAX_GAP`): a gap that is too
  long is not charged. Nothing the emitter forgets can charge anyone.

### Ingestion

`POST /v1/events` takes a batch: one tick's observations. Per account in
the batch, one Firestore transaction:

1. read the account's ledger document;
2. drop observations whose `id` is already recorded or whose `time` is not
   after the session's `lastSeen` (a replay or a late arrival);
3. run the step of `metering.md`: seconds, money with the carry, debit of
   the live grants in expiry order, overdraft, level, `exhaustedAt`;
4. write the ledger document and create the event documents (create-only:
   a second writer loses the transaction and retries from 1).

The observer retries a failed post with the same ids for up to `MAX_GAP`;
after that it gives up and the next tick's gap is free. At-least-once
delivery, exactly-once effect.

### Storage layout (Firestore)

| Collection | Document | Holds |
|---|---|---|
| `ledgers` | `<account>` | what `Account.status` held in the merged design: `sessions` (lastSeen, awake), `carry`, `consumed` by grant, `balanceMicros`, `level`, `exhaustedAt`, `observedAt`, `overdraftMicros`, the current period's totals, and the live grants (a few per account, far under the 1 MiB document limit) |
| `grants` | `<grant key>` | the immutable record of each credit (source, amount, validFrom, expiresAt, Stripe reference, revoked). Created only if absent: the key is what stops a payment or a sign-up being credited twice. `signup` grants are never deleted. |
| `events` | `<session>_<tick>` | the raw observation and what it was charged. TTL of 13 months on an `expireAt` field. |
| `periods` | `<account>_<period start>` | the closed period's totals (the `UsagePeriod` of the merged design) |

Balances and events are in the same store on purpose. A balance is
derived from grants and events; if they live apart (events in Firestore,
grants in etcd; or usage in one product and credit in Stripe), every debit
is a write to two systems with no transaction across them, and the
question "which is right" has to be answered by a reconcile job. Stripe
remains the record of what was **paid**; the usage store is the record of
what was **credited and used**, and the Stripe reconcile of `stripe.md`
re-makes credits from payments if the store is ever lost.

### Read API

| Route | Used by | Answers |
|---|---|---|
| `GET /v1/accounts/{account}/balance` | the backend, at create, resume and wake, and for `GET /api/billing` | `balanceMicros`, `level`, `exhaustedAt`, `observedAt`, `stale`, `plan`, `balances` by source, `burnMicrosPerHour`: the `LedgerView` of `testing.md`. One document read. |
| `GET /v1/accounts?level=exhausted` | the backend's 30 s sweep | the accounts at zero, with `exhaustedAt`. One query, billed per result, so it costs almost nothing while nobody is at zero. |
| `PUT /v1/grants/{key}` | the backend, from Stripe webhooks and the sign-up decision (`Ledger.EnsureGrant`) | `created`, or the existing grant |
| `POST /v1/grants:revoke` | the backend (`Ledger.Revoke`) | |
| `GET /v1/accounts/{account}/usage?period=` | the backend, for `/api/billing/usage` | period totals and the per-session breakdown |
| `POST /v1/events` | the observer | per-event `accepted` or `duplicate` |

Authentication between the pods: NetworkPolicy plus the caller's projected
ServiceAccount token, checked with a `TokenReview` (no shared secret).

### Replay, backfill, export

- **Replay**: re-posting any stored or logged batch is a no-op (ids).
- **Rebuild**: a command reads `grants` and `events` in time order and
  recomputes every `ledgers` document; the result must equal what is
  stored. That is the audit and the repair tool, and it is why the raw
  events are kept.
- **Backfill** after an outage is deliberately not done: unobserved time
  is free, as in the merged design.
- **Export**: Firestore's managed export to Cloud Storage on a schedule,
  loadable into BigQuery for analysis; point-in-time recovery for
  mistakes.

---

## 4. Storage on GCP

### Bigtable, precisely

From `cloud.google.com/bigtable/pricing`, Oregon, 2026-10-02: a node is
**$0.65 an hour**, so one node is 0.65 x 730 = **$474.50 a month**
(Enterprise Plus: $0.85 an hour, $620.50). SSD storage $0.17 per GiB-month,
HDD $0.026. The minimum is one node; autoscaling's minimum "must be greater
than zero"; there is no free tier on the page. It is built for tens of
thousands of rows a second per node; we will write a few events a second
at 1,000 accounts. It also has no multi-row transaction, so "insert the
event and debit the balance" would not be atomic. The owner's instinct (a
managed Google key-value store, written by one small service) is right;
the product of that shape at our size is Firestore.

### Firestore, with the arithmetic

Prices (Standard edition, Oregon): reads $0.03, writes $0.09, deletes
$0.01 per 100,000; storage $0.15 per GiB-month. Free every day: 50,000
reads, 20,000 writes, 20,000 deletes, and 1 GiB stored. "Firestore allows
exactly one free database per project", the default one, so the usage
store should be the project's `(default)` database (the project has no
Firestore database today, as far as `infra/main` shows).

- Today: a few thousand writes a month. **$0.**
- At 1,000 accounts, with decision 5 (disk hourly):
  - awake: 200 sessions x 240 minutes x 30 days = 1.44M observations a
    month, each one event document and one ledger write: 2.9M writes;
  - disk: 2,000 sessions x 730 hours = 1.46M event documents, plus one
    ledger write per account per hour, 0.73M: 2.2M writes;
  - reads: one ledger read per transaction (2.2M), the enforcement reads
    (tens of thousands), the sweep's query (86,400): about 2.4M;
  - 5.1M writes less the free 0.6M = 4.5M x $0.90 per million = $4.05;
    reads 2.4M less 1.5M free = $0.27; storage about $0.15 per GiB after
    the first.
  - **About $4 to $6 a month.** Not keeping one document per raw event
    (one document per account per tick instead) brings it to about $3.
- Without decision 5, a 60 s write for each of 2,000 sleeping sessions is
  86M writes a month, about $78: the reason for decision 5.

Consistency: reads return the latest version; transactions are
serializable. No latency figure is published; a point read in the same
region is typically some tens of milliseconds at worst (UNVERIFIED, to be
measured in the service's first test).

### Cloud SQL Postgres, the runner-up

`db-f1-micro` $7.67 a month plus 10 GiB SSD $1.70 = $9.37; `db-g1-small`
$27.25; "shared CPU machine types are not covered by the Cloud SQL SLA".
The smallest with an SLA, `db-custom-1-3840`, is $51.01, or $102.02 with
HA. Backups $0.08 per GiB-month. It would give SQL (ad hoc questions about
usage are easier) for $9 to $51 a month and a connector or proxy sidecar.
Choose it only if the team would rather write SQL than Firestore
transactions; the service's API hides the choice either way.

### The others

- **Postgres in the cluster**: the disk is $1.00 a month, but it is a
  stateful pod on the one system node, and backup, restore, upgrades and
  the day the node is replaced are ours. For the record of people's money
  that is the wrong place to save $0 to $9.
- **Spanner** $65.70 (100 processing units), **AlloyDB** $113.65: correct
  and far too large.
- **BigQuery**: ingest and storage are free at this volume (Storage Write
  API free to 2 TiB a month, 10 GiB stored free), but a balance check is a
  query that takes seconds. Good as a later copy for analysis.
- **Cloud Storage as a log**: no transaction across objects; fine as the
  export target.

### Reaching it from GKE without keys

The repository already uses Workload Identity Federation with the
Kubernetes ServiceAccount itself as the IAM principal
(`infra/main/snapshots.tf`: `local.workload_pool_prefix`, no Google
service account, no key). The same for the usage service:

```hcl
# infra/main/usage.tf (sketch; not applied)

# apis.tf: add "firestore.googleapis.com" to the services.

resource "google_firestore_database" "usage" {
  name                              = "(default)" # the one database with the free quota
  location_id                       = var.region  # us-west1; cannot be changed later
  type                              = "FIRESTORE_NATIVE"
  point_in_time_recovery_enablement = "POINT_IN_TIME_RECOVERY_ENABLED"
  delete_protection_state           = "DELETE_PROTECTION_ENABLED"
  deletion_policy                   = "ABANDON"
}

resource "google_firestore_field" "events_ttl" {
  database   = google_firestore_database.usage.name
  collection = "events"
  field      = "expireAt"
  ttl_config {}
}

resource "google_firestore_backup_schedule" "usage_daily" {
  database  = google_firestore_database.usage.name
  retention = "1209600s" # 14 days
  daily_recurrence {}
}

# The usage service's Kubernetes ServiceAccount is the principal.
resource "google_project_iam_member" "usage_firestore" {
  project = var.project_id
  role    = "roles/datastore.user"
  member  = "principal://${local.workload_pool_prefix}/subject/ns/${var.sessions_namespace}/sa/usage"
}
```

Only the `usage` ServiceAccount gets the role: the backend and the
observer reach the data through the service. Point-in-time recovery data
is billed at $0.15 per GiB-month with no free quota: cents. If Cloud SQL
were chosen instead: `google_sql_database_instance`, `google_sql_user`
with `type = "CLOUD_IAM_SERVICE_ACCOUNT"`, the Go connector with IAM
database authentication, and roles `cloudsql.client` and
`cloudsql.instanceUser`; whether a bare `principal://` identity can log in
that way is UNVERIFIED, so plan on a Google service account there.

---

## 5. Enforcement without an in-memory copy

### Where the check runs (read from the code and the contract)

`enforcement.md`, "Where each check is made", and the code agree: the
billing decision is made **per start, not per call**.

| Path | Code | When it asks |
|---|---|---|
| Create | `api.create`, before `Store.CreateWithPolicy` | every create |
| Resume | `api.patch` | every resume |
| Wake on a call | `proxy.Waker.await` (`backend/internal/proxy/waker.go`), the `sessions.Asleep` branch, before `Store.Wake` | only when a proxied request finds the session asleep; concurrent callers share one wait (`singleflight`), so one check per wake |
| A call to a running session | `Waker.EnsureAwake` returns from `remembered` or sees `Running` and forwards | **no billing read.** The draining refusal reads the session's own annotation. |
| The stop sequence | the backend's 30 s sweep | one query: accounts at zero |

So the read is on paths that then wait seconds for a pod (a wake polls the
cluster once a second until the session runs). The design's reason for
the informer, "create and wake must answer in milliseconds", does not hold
against that: tens of milliseconds are invisible there.

### What a read costs

| Read | Latency | Consistency |
|---|---|---|
| The merged design: informer cache | microseconds | up to 60 s old (the tick), plus a watch event |
| **Usage service, then one Firestore document** | one in-cluster hop plus a point read: some tens of ms (UNVERIFIED until measured) | the balance as of the last recorded observation; strongly consistent |
| `Account` read straight from the API server (card state) | single-digit ms; the Waker already makes one such read a second while waiting | the object as stored |
| Cloud SQL query through the connector | a few ms (UNVERIFIED) | strongly consistent |
| OpenMeter or Autumn hosted API | an internet round trip, 50 to 200 ms (UNVERIFIED) | eventually consistent, lag undocumented |

The card state (`Account.spec`) is likewise read from the API server at
the decision, with no informer. `ByCustomer` and `ByPaymentMethod` (used
only by Stripe webhooks) become a list with a label selector.

### When the store cannot be reached

The merged rule (rows 6 and 7 of the decision table) needs "the last known
balance". Two different failures hide in "stale", and they separate
cleanly once there is a service:

- **The observer is behind, the service answers.** The service returns the
  stored balance with `stale: true` (`observedAt` older than
  `BILLING_STALE_AFTER`). Rows 6 and 7 apply exactly as written: credit
  when last counted means carry on; none means refuse; nothing is stopped.
- **The service or Firestore does not answer** (two attempts, two seconds
  in all). The backend knows nothing and keeps nothing, so the rule has to
  be by action (decision 6): **wake and resume are allowed** (the session
  exists, so its owner passed the card gate, and what is at risk is
  minutes at $0.20 an hour); **create is refused** with 503
  `metering_unavailable` and `Retry-After`; **the sweep stops nothing**.
  Time the store did not record is free. This keeps both of the design's
  principles: never over-bill, never lock a customer out of their work for
  our fault.

---

## 6. Who emits usage

| | Observer heartbeats (recommended) | Start and stop from the backend |
|---|---|---|
| What is sent | "session S was awake at T" every 60 s | "S started at T1", "S stopped at T2" |
| A lost event | that minute is free | a lost stop **bills for ever**; a lost start bills nothing |
| Emitter crashes | no events: free time, visible as a stale `observedAt` | sessions keep running with an open interval; someone must close it, by observing: a second mechanism that is the observer |
| State changes the emitter did not make | seen (node preempted, pod crash, `kubectl`, a restore that never becomes Ready) | missed: the backend asks for a transition, it does not see whether the pod is in fact up |
| "Never bill unobserved time" | by construction | only with a watchdog |
| Precision | to the tick (errors in the user's favour, `metering.md`) | to the second |
| Events | one per awake session-minute | two per run |

Start and stop events are cheaper and more precise, and wrong in the one
way billing must not be wrong. Every system in section 2 that takes
start/stop needs a heartbeat or a maximum duration to be safe; the
heartbeat alone is already enough. **Keep the observer.** It lists
Sandboxes once a minute and posts; it does no arithmetic and writes
nothing to the cluster. The backend may add `session.wake` events later as
information (for the wake rate limit, or to show starts in the usage
page); they would never be charged from.

---

## 7. What changes in the build

The owner's direction now, in its smallest form: **no in-memory copy;
usage and credit collected and stored by a dedicated service; the
`Account` CRD kept for what the backend writes.**

### Contracts (each in its own pull request, as the tracks require)

| Contract | Change |
|---|---|
| `metering.md` | The step is unchanged. "The operator implements this" becomes the usage service; "The tick" becomes the observer's post; the state lives in the `ledgers` document. With decision 5: a second gap constant for the disk charge, and the disk vectors re-cut. |
| `metering-vectors.json`, `spike/meter_ref.py` | Unchanged for awake time; disk cases adjusted for decision 5. |
| new `usage-api.yaml` | The routes and the event of section 3. |
| `crd-account.yaml` | `status` (balance, meter, period) is removed; `spec` stays. |
| `crd-grant.yaml`, `crd-usageperiod.yaml` | Retired before they are deployed; their fields become the `grants` and `periods` documents. |
| `enforcement.md` | "Reads Account objects through an informer" becomes a read per decision; the inputs table's sources; the two cases of "stale" (section 5); the sweep's query. The table's rows, the answers and the stop sequence are unchanged. |
| `testing.md` | **The interfaces keep their signatures.** `Ledger`'s comment changes from "the balance the operator computes" to "the usage service's". The fakes and all four scenarios are unchanged. |
| `deploy.md` | The `usage` Deployment, its ServiceAccount and NetworkPolicy; Firestore and its role; the operator's Role loses `accounts/status` and gains nothing; `billing-export` now exports `accounts` only. |
| `backend-api.yaml`, `stripe.md`, `ui-states.md`, `catalogue.yaml`, `legal-pages.md` | Unchanged. |

### Tracks

| Track | Change | Size |
|---|---|---|
| **A, operator** | Becomes the observer: keeps the pass and the observation, posts batches to `/v1/events` with retry, keeps its liveness and its log line. Loses the step, the status write, periods, retention and the catalogue. | Smaller. The Python step and its property tests stay as the reference implementation. |
| **H, usage service (new)** | `backend/cmd/usage`, `backend/internal/usage`: the routes, the Firestore store behind an interface with an in-memory fake, the Go step (shared with D's fake ledger), periods, retention, the rebuild command. Tests: every vector; the same batch twice charges once; two writers on one account; the Firestore emulator in CI. | The new work: roughly what A loses, plus the HTTP surface. |
| **B, deployment** | Two CRDs fewer; the `usage` Deployment and policy; `infra/main/usage.tf`; the image in `images.yml`; the export CronJob reduced. | About the same. |
| **C, Stripe** | None, if it calls `Ledger.EnsureGrant` and `Revoke` only through the interface, as the tracks require. "Created only if absent" is the same promise from Firestore as from a CRD name. | None |
| **D, enforcement** | `billing/kube`: `Accounts` reads the API server directly (no informer); `Ledger` becomes an HTTP client of the usage service; the sweep uses the exhausted query; the unreachable rule of section 5. The decision function, the fakes, the scenario tests and the edits to `api`, `proxy`, `idle` and `sessions` are as planned. | Small: two implementations behind interfaces that already exist. |
| **E, UI**; **F, site**; **G, sign-up** | None: `backend-api.yaml` does not change. | None |

Because every track was told to reach state only through `Accounts`,
`Ledger`, `Stripe`, `Clock` and `Sessions`, the change lands behind
`Ledger` and inside track A. Work already done on the decision table, the
webhook handlers, the UI and the fakes is kept.

### Later, when "for now" ends

Moving `Account` out of the cluster as well is one more implementation of
the `Accounts` interface, on an `accounts` collection in the same store,
plus a one-time copy. Nothing above has to be redone for it.

---

## 8. Sources

Read 2026-10-02 unless noted.

Google Cloud (Oregon selected on each pricing page):

- Bigtable: https://cloud.google.com/bigtable/pricing ;
  https://docs.cloud.google.com/bigtable/docs/autoscaling ;
  https://docs.cloud.google.com/bigtable/docs/replication-overview
- Firestore: https://cloud.google.com/firestore/pricing ;
  https://docs.cloud.google.com/firestore/native/docs/understand-reads-writes-scale ;
  https://docs.cloud.google.com/firestore/native/docs/ttl ;
  https://docs.cloud.google.com/firestore/native/docs/pitr ;
  https://docs.cloud.google.com/firestore/native/docs/manage-data/export-import ;
  https://docs.cloud.google.com/firestore/native/docs/security/iam
- Cloud SQL: https://cloud.google.com/sql/pricing ;
  https://docs.cloud.google.com/sql/docs/postgres/connect-kubernetes-engine ;
  https://docs.cloud.google.com/sql/docs/postgres/iam-authentication
- Spanner: https://cloud.google.com/spanner/pricing . AlloyDB:
  https://cloud.google.com/alloydb/pricing . BigQuery:
  https://cloud.google.com/bigquery/pricing . Cloud Storage:
  https://cloud.google.com/storage/pricing . Disks:
  https://cloud.google.com/compute/disks-image-pricing . Memorystore:
  https://cloud.google.com/memorystore/docs/redis/pricing
- Workload Identity:
  https://docs.cloud.google.com/kubernetes-engine/docs/how-to/workload-identity
- OpenTofu resources (provider documentation, not Google pages):
  `https://registry.terraform.io/providers/hashicorp/google/latest/docs/resources/<name>`
  for `firestore_database`, `firestore_field`, `firestore_backup_schedule`,
  `sql_database_instance`, `sql_user`, `bigtable_instance`.

Metering and billing products:

- OpenMeter: https://github.com/openmeterio/openmeter ;
  https://openmeter.io/docs/open-source/architecture ;
  https://openmeter.io/docs/open-source/kubernetes ;
  https://openmeter.io/docs/billing/entitlements/grant ;
  https://openmeter.io/docs/billing/entitlements/entitlement ;
  https://openmeter.io/docs/metering/events/usage-events ;
  https://openmeter.io/docs/integrations/stripe/overview ;
  https://konghq.com/blog/news/kong-acquires-openmeter ;
  https://konghq.com/pricing ; https://openmeter.io/pricing
- Lago: https://github.com/getlago/lago ;
  https://getlago.com/docs/guide/wallet-and-prepaid-credits/overview ;
  https://getlago.com/docs/guide/events/ingesting-usage ;
  https://getlago.com/docs/integrations/payments/stripe-integration ;
  https://getlago.com/docs/pricing
- Stripe:
  https://docs.stripe.com/billing/subscriptions/usage-based/billing-credits ;
  https://docs.stripe.com/billing/subscriptions/usage-based/billing-credits/implementation-guide ;
  https://docs.stripe.com/billing/subscriptions/usage-based/recording-usage-api ;
  https://docs.stripe.com/billing/subscriptions/usage-based/alerts ;
  https://docs.stripe.com/api/billing/meter-event/create ;
  https://stripe.com/billing/pricing
- Flexprice: https://github.com/flexprice/flexprice ;
  https://flexprice.io/pricing . Kill Bill:
  https://docs.killbill.io/latest/getting_started ;
  https://docs.killbill.io/latest/aviate-wallet . Metronome:
  https://metronome.com/pricing ;
  https://docs.metronome.com/api-reference/usage/ingest-events.md . Orb:
  https://www.withorb.com/pricing ;
  https://docs.withorb.com/product-catalog/prepurchase . Amberflo:
  https://www.amberflo.io/pricing . Autumn:
  https://github.com/useautumn/autumn ; https://useautumn.com/pricing .
  Polar: https://polar.sh/docs/features/usage-based-billing/credits .
  Stigg: https://www.stigg.io/pricing . Schematic:
  https://schematichq.com/pricing

Not verified, and to be checked before relying on them: every latency in
milliseconds (no vendor publishes one); that Bigtable has only single-row
transactions (well known, not re-read today); OpenMeter's balance lag; the
minimum on Metronome's startup plan; whether the project already has a
Firestore database (none is declared in `infra/main`); Firestore's
availability SLA figure.
