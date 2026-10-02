# Metering and billing: design

Status: **proposed 2026-10-02, waiting for the product owner.** The
questions that need a decision are section 11, each with a recommended
default. The contracts are in
[`docs/contracts/billing/`](../contracts/billing/README.md), the build
tracks in
[2026-10-02-metering-billing-tracks.md](2026-10-02-metering-billing-tracks.md).
No product code is written, nothing is deployed, and no Stripe account or
cluster was touched.

Facts about Stripe, Google, Pomerium, Dex and GKE were read from their own
documentation on 2026-10-02 and carry their URL. What could not be
confirmed there is marked **UNVERIFIED**.

## The request

> Build metering and billing with Stripe.

## Decisions that shaped the design

Settled by the product owner before this document:

1. **Four paid options and a free one**: subscriptions at $5, $20 and $100
   a month, pay as you go, and a small free allowance with no card. What
   each includes is proposed here (section 7).
2. **What is metered is awake session time.** Sleeping and stopped time is
   free. The $5 plan includes 40 hours a month.
3. **No automatic overage.** When the hours run out, sessions stop; the
   user can buy more as a one-off purchase.
4. **Sign-up opens to anyone** with a Google or GitHub account. Admins
   stay a list.
5. **Stripe test mode first.** There is no Stripe account yet. The product
   owner creates it and enters every key as a GitHub Actions secret; no key
   is ever in the repository, a log or a chat, and no agent handles one.
6. The pattern of the session-policies feature: design, written contracts,
   concurrent tracks, everything behind a flag that is off by default.

## Summary of the proposal

- **One unit, one mechanism.** The unit is the awake session-second. An
  account holds **grants** of seconds: the month's free allowance, a plan's
  hours for one paid period, a pack that was bought. Usage is taken from
  the grant that expires first. "Pay as you go" and "top-up" are the same
  thing: a prepaid pack of hours, bought with a one-off payment, valid 12
  months. There is no post-paid usage and no bill that can surprise anyone.
- **Stripe takes payments and runs subscriptions; it does not meter.**
  Checkout for subscribing and for packs, the Customer Portal for card,
  invoices, plan change and cancellation, webhooks to learn what was paid.
  Stripe's usage-based billing is not used: it bills in arrears, cannot
  stop a customer at a limit, and its credits do not apply to flat plans
  (section 3.3).
- **The ledger is ours, in custom resources.** `Account` (one per user),
  `Grant`, `UsagePeriod`. A new **billing operator** (kopf, one replica)
  is the only writer of the ledger: every 60 seconds it looks at every
  session's Sandbox, credits the seconds it saw awake, and debits grants,
  in one atomic update per account. The backend stays stateless: it reads
  the ledger to decide, and writes what Stripe tells it as Grants.
- **Never over-bill.** Only time that was observed twice, at most 150
  seconds apart, is counted. When the operator is down, time is free. When
  the cluster's objects are lost, purchases are rebuilt from Stripe and
  usage is forgotten.
- **Exactly once without a database.** A Grant's name is the hash of what
  caused it (a checkout session, a subscription period). Stripe may send an
  event twice, late or out of order: the handler re-reads Stripe's current
  state and creates the Grant if it is not there.
- **Enforcement** at three doors (create, resume, wake through the proxy)
  and one sweep that puts sessions to sleep, snapshot first, five minutes
  after the balance reaches zero. Paying users are let through when the
  ledger is stale; free users wait.
- **Three switches.** `BILLING` = `off` (default), `meter` (count and show,
  refuse nothing), `enforce`. `STRIPE_MODE` = unset, `test`, `live`.
  `OPEN_SIGNUP`, the last stage, with its own prerequisites (section 8).
- **Proposed catalogue**: Free 3 h and 1 session; Starter $5 for 40 h;
  Pro $20 for 180 h; Scale $100 for 1000 h (not sold until the CPU quota is
  raised); packs of 20 h for $5 and 100 h for $20.

```
 user ── Pomerium ── backend ──────────────── Stripe (Checkout, Portal)
                       │  reads Account.status      │
                       │  writes Account.spec,      │ webhook, signed
                       │  Grants (from Stripe)      ▼
                       │◄───────────── api.computeruse.site/stripe/webhook
                       ▼
             Account, Grant, UsagePeriod  (custom resources)
                       ▲
                       │ sole writer of Account.status
              billing operator ── watches ── Sandboxes (awake or not)
```

---

## 1. Findings

### 1.1 Stripe Checkout

- A Checkout Session has a `mode`: `subscription` "if the Checkout Session
  includes at least one recurring item", `payment` for one-off payments.
  Plans use the first, packs the second.
  (https://docs.stripe.com/api/checkout/sessions/create)
- `client_reference_id` is "a unique string to reference the Checkout
  Session ... can be used to reconcile the session with your internal
  systems". `metadata`, `subscription_data.metadata` and
  `payment_intent_data.metadata` are three separate maps. Whether the
  session's metadata is copied to the subscription or the PaymentIntent is
  **UNVERIFIED**, so all three are set.
- Fulfilment (https://docs.stripe.com/checkout/fulfillment): handle
  `checkout.session.completed` and `checkout.session.async_payment_succeeded`;
  "check the `payment_status` property to determine if it requires
  fulfillment"; "perform fulfillment only once per payment ... your
  `fulfill_checkout` function might be called multiple times, possibly
  concurrently, for the same Checkout Session"; "webhooks can sometimes be
  delayed ... trigger fulfillment from your landing page as well"; and
  "you can't rely on triggering fulfillment only from your checkout landing
  page".
- New subscriptions made by Checkout default to
  `billing_mode.type = flexible`. In that mode a cancellation at the end of
  the period shows as `cancel_at` being set, not `cancel_at_period_end`.
  (https://docs.stripe.com/customer-management/integrate-customer-portal)

### 1.2 Customer Portal

- `POST /v1/billing_portal/sessions` with `customer` and `return_url`
  gives a short-lived URL. Customers can "update payment methods", "update
  subscriptions", "cancel subscriptions immediately or at the end of the
  current billing period" and "pay, download, and view current and past
  invoices". (https://docs.stripe.com/customer-management)
- It is configured through the API
  (https://docs.stripe.com/api/customer_portal/configurations/create):
  `subscription_cancel.mode` (`at_period_end`), `subscription_update` with
  the list of products and prices a customer may switch between,
  `proration_behavior`, `billing_cycle_anchor` (`now` or `unchanged`), and
  `schedule_at_period_end.conditions` (for example
  `decreasing_item_amount`, which makes a downgrade wait).
- "You can't define multiple Prices with the same `product` and
  `recurring.interval` values" in a portal configuration: the three plans
  must be **three Products**.

### 1.3 Prices and lookup keys

A Price has a `lookup_key`, "used to retrieve prices dynamically from a
static string", and prices can be listed by `lookup_keys` (up to 10 per
call). (https://docs.stripe.com/api/prices/create,
https://docs.stripe.com/api/prices/list). So the repository names prices by
lookup key and never holds a price ID, which differs between test and live.

### 1.4 Webhooks

From https://docs.stripe.com/webhooks:

- Verify with "the event payload, the `Stripe-Signature` header, and the
  endpoint's secret"; "Stripe requires the raw body of the request"; the
  libraries "have a default tolerance of 5 minutes". The secret is per
  endpoint and differs between sandbox and live.
- Retries: "for up to three days with an exponential back off in live
  mode"; in a sandbox "three times over the course of a few hours".
- "Stripe doesn't guarantee the delivery of events in the order that
  they're generated."
- Duplicates happen: "guard against duplicated event receipts by logging
  the event IDs you've processed".
- "Your endpoint must quickly return a successful status code (`2xx`)".

Subscriptions (https://docs.stripe.com/billing/subscriptions/webhooks):
`customer.subscription.created`, `.updated` ("renewing a subscription ...
changing plans all trigger this event"), `.deleted`; `invoice.paid`
("confirm that its status is `active` before extending the customer's
access"); `invoice.payment_failed`. Statuses: `incomplete` (23 hours to
pay), `incomplete_expired`, `trialing`, `active`, `past_due`, `unpaid`,
`canceled`, `paused`. "When a subscription changes to `canceled` or
`unpaid`, revoke access."

Two changes in API version 2025-03-31 ("basil") matter to the code:

- "`current_period_start` and `current_period_end` fields are no longer
  available on the subscription resource"; they are on each subscription
  item.
  (https://docs.stripe.com/changelog/basil/2025-03-31/deprecate-subscription-current-period-start-and-end)
- An invoice's subscription is at
  `invoice.parent.subscription_details.subscription`, not
  `invoice.subscription`.
  (https://docs.stripe.com/changelog/basil/2025-03-31/adds-new-parent-field-to-invoicing-objects)

Refunds: "at a minimum, Stripe recommends that you listen for the
`refund.created` event" (https://docs.stripe.com/refunds). Disputes:
`charge.dispute.created`, `charge.dispute.closed` (`won`, `lost`).

Plan changes (https://docs.stripe.com/billing/subscriptions/prorations):
by default prorations are created and "positive prorations aren't
immediately billed"; `always_invoice` bills at once. A change between two
monthly prices keeps the billing date unless the anchor is reset.

### 1.5 Idempotency, test clocks, the CLI, sandboxes

- "All `POST` requests accept idempotency keys"; keys can be removed
  "after they're at least 24 hours old".
  (https://docs.stripe.com/api/idempotent_requests)
- Test clocks ("simulations") move a customer's time forward to test
  renewals and failures. Limits: three customers per clock, three
  subscriptions per customer, at most two billing intervals per advance;
  "only available in sandboxes".
  (https://docs.stripe.com/billing/testing/test-clocks/api-advanced-usage)
- `stripe listen --forward-to <url>` forwards a sandbox's events to a
  local server with its own signing secret, which "will not change between
  restarts"; `stripe trigger <event>` sends canned events.
  (https://docs.stripe.com/cli/listen)
- "After you create a Stripe account, Stripe places you in a sandbox."
  Every account has a "test mode sandbox" that shares settings with live;
  up to five further sandboxes isolate settings completely, and "for new
  integrations, use a general sandbox instead of the test mode sandbox".
  (https://docs.stripe.com/sandboxes)
- "Stripe recommends always using RAKs [restricted API keys] instead of
  unrestricted secret keys"; "create a separate restricted key for each
  service". (https://docs.stripe.com/keys/restricted-api-keys). The exact
  names of the permissions needed are **UNVERIFIED**.

### 1.6 Stripe's usage-based billing, and why it is not used

- Billing Meters aggregate "meter events" and bill them on the
  subscription's invoice: "track incurred usage over a determined time
  period, then charge the customer at the end of the period". Events are
  processed asynchronously.
  (https://docs.stripe.com/billing/subscriptions/usage-based/recording-usage-api)
- There is no hard stop. Alerts notify or trigger an invoice.
  (https://docs.stripe.com/billing/subscriptions/usage-based/alerts)
- Billing credits: "you can only apply credit grants to subscription items
  that use metered prices and report usage through Meters"; "credits apply
  to invoices only at the time of finalization"; funding them is the
  integrator's job (take a payment, then make the credit grant).
  (https://docs.stripe.com/billing/subscriptions/usage-based/billing-credits)
- Stripe's own comparison says of this product: "Credits are only
  reconciled at invoice time. Customers can exceed their balance during the
  cycle", "Prepaid credits and drawdown: not supported", "Real-time usage
  visibility: not supported", and steers new usage-based integrations to
  Metronome.
  (https://docs.stripe.com/billing/subscriptions/usage-based/compare-metronome)

Our shape is "included hours, prepaid packs, stop at zero". Stripe's
metering would give us none of the three: we would still need our own
real-time ledger to stop at zero, and would then be reporting the same
usage to a second system that can only disagree with it.

### 1.7 Stripe as code

- There is a provider published by Stripe, `stripe/stripe`, version 0.3.0
  (August 2026), generated from Stripe's API description; resources include
  products, prices (with `lookup_key`) and webhook endpoints. It is still
  0.x; whether it is generally available is **UNVERIFIED**, and whether its
  webhook resource exposes the signing secret is **UNVERIFIED**.
  (https://registry.terraform.io/providers/stripe/stripe/latest,
  https://docs.stripe.com/terraform/resources)
- The community provider `lukasaron/stripe` was archived in April 2026.

### 1.8 Stripe's costs, tax, fraud, and going live

- "2.9% + 30¢ per successful transaction for domestic cards"; Billing
  "0.7% of Billing volume"; a dispute costs $15; fees are not returned on a
  refund; the smallest charge is $0.50. (https://stripe.com/pricing,
  https://docs.stripe.com/currencies). Whether the Billing fee applies to a
  one-off Checkout payment is **UNVERIFIED**; the arithmetic assumes it
  does not.
- Stripe Tax: "0.5% per transaction, where you're registered to collect
  taxes". "Stripe Tax only collects tax in jurisdictions where you have an
  active registration", and its monitoring "highlights potential
  registration obligations, but it's up to you to confirm whether
  registration is actually required". Stripe's documents do not say tax
  must be collected from the first sale. (https://docs.stripe.com/tax/set-up,
  https://docs.stripe.com/tax/monitoring). Whether and where tax is owed is
  a question for an accountant, not this document.
- Card testing: fraudsters "create small amount payments"; Checkout has
  Stripe's own controls ("rate limiters, AI models, CAPTCHA triggers"), and
  the advice for the merchant is "requiring login or session validation
  before they can make a payment".
  (https://docs.stripe.com/disputes/prevention/card-testing). Every
  checkout here is behind a sign-in.
- To take live payments the business's website needs: "a description of
  what you're selling", "the purchase currency", "customer service contact
  information", a refund policy and a cancellation policy, "your website's
  privacy policy", "your business address".
  (https://docs.stripe.com/get-started/checklist/website)

### 1.9 Google, GitHub, Dex, Pomerium

- Google, "Testing" status: up to 100 test users, and authorisations
  expire after seven days, **except** that "if your app requests a subset
  of the following: name, email address, and user profile ... your users do
  not need to be in the trusted user list, they will not see a warning
  message, and their authorizations will not expire after 7 days".
  (https://support.google.com/cloud/answer/15549945). Dex asks for exactly
  those scopes. This contradicts the working assumption that the Testing
  status blocks arbitrary Google accounts today; it has to be tried with an
  account that is not a listed test user (two minutes of the product
  owner's time), and the app should be published either way.
- "In production": available to any Google account after "Publish app".
  For these scopes verification is optional, but "if you want your app to
  display an app name and logo on the OAuth consent screen, you will need
  to complete ... 'brand-verification'": a public homepage that links a
  privacy policy, and ownership of the domains shown in Search Console; it
  "usually takes 2-3 business days".
  (https://support.google.com/cloud/answer/13463073,
  https://developers.google.com/identity/protocols/oauth2/production-readiness/brand-verification).
  What an unverified app shows in place of its name is **UNVERIFIED**.
- GitHub OAuth apps have no publishing or review step: any GitHub user can
  authorise one.
  (https://docs.github.com/en/apps/oauth-apps/building-oauth-apps/creating-an-oauth-app)
- Dex's GitHub connector takes the email that is both primary and
  verified, and fails with "user has no verified, primary email" otherwise.
  Dex's Google connector passes Google's `email_verified` claim through and
  does **not** refuse a login when it is false.
  (https://github.com/dexidp/dex/blob/master/connector/github/github.go,
  https://github.com/dexidp/dex/blob/master/connector/google/google.go).
  The product identifies a user by email address, so this matters
  (section 8.2).
- Pomerium: `allow: and: - authenticated_user: true` allows any signed-in
  user. The route setting `allow_any_authenticated_user` does the same but
  "Pomerium will not enforce your centralized authorization policy for this
  route", so the policy form is used.
  `allow_public_unauthenticated_access: true` makes a route public, as the
  API host's routes already are. (https://www.pomerium.com/docs/internals/ppl,
  https://www.pomerium.com/docs/reference/routes/allow-any-authenticated-user,
  https://www.pomerium.com/docs/reference/routes/public-access). Whether
  Pomerium passes a webhook's body and `Stripe-Signature` header through
  untouched is **UNVERIFIED** and is tested first.

### 1.10 GKE and etcd as a place for a ledger

- The control plane's database is Google's; no way for a customer to back
  it up or restore it is documented (**UNVERIFIED** as a direct statement).
  Google's answer is Backup for GKE, which saves "Kubernetes resource
  manifests extracted from the cluster API server" and volume snapshots.
  (https://docs.cloud.google.com/kubernetes-engine/docs/add-on/backup-for-gke/concepts/backup-for-gke).
  Its price changed in March 2026 to a charge per namespace; the figure
  found, $9 per namespace-month, is from a search result and is
  **UNVERIFIED**.
- If the cluster is deleted and made again, every object that is not in
  git or a backup is gone: Accounts, Grants, tokens, policies, Secrets.
- Limits: one object at most 1.5 MiB; the whole database 6 GB.
  (https://docs.cloud.google.com/kubernetes-engine/quotas)
- For comparison: the smallest Cloud SQL Postgres is about $8 a month
  before storage and backups (**UNVERIFIED**, from a search result);
  Firestore's free tier is 1 GiB and 20,000 writes a day
  (https://docs.cloud.google.com/firestore/quotas).

---

## 2. Metering

The exact rules, the algorithm and 17 test vectors are
[`metering.md`](../contracts/billing/metering.md). This section is why.

### 2.1 What awake time is

A session is billable while its Sandbox is somebody's, is not being
deleted, has `operatingMode: Running` and is `Ready`: the state the app
calls "running". So:

| | Billed |
|---|---|
| A warm pod before a user takes it | no |
| Starting: waiting for a node, restoring a snapshot | no |
| Awake and used | yes |
| Awake and idle, the 15 minutes before sleep | **yes** |
| Going to sleep (the snapshot) | the last partial minute is free |
| Asleep, stopped, failed | no |

The idle tail is billed because it costs what an active minute costs: the
session holds its place on the node. The alternative, not billing idle
minutes, would need the backend's in-memory idle tracker to become part of
the ledger, and would make a session left open in a browser tab free. The
UI says how to stop the clock (stop the session). A shorter idle time for
free accounts is an open question (11.6).

### 2.2 How it is measured

Three ways were considered:

| | For | Against |
|---|---|---|
| The backend writes heartbeats | it already knows activity | the backend is the thing that restarts on every deploy; it would become the keeper of money state it was built not to keep; metering stops when the app is down even though sessions run |
| Kubernetes events | free | kept for an hour, dropped under load: not a record |
| **An operator samples the Sandboxes** | the Sandbox's status is the truth the UI shows; independent of the backend; one writer; trivially restartable | sampling loses partial minutes (in the user's favour) |

The operator looks every 60 seconds. It credits a session the time
between two looks only when it was awake at both and they were at most 150
seconds apart. The first look at a newly awake session credits back to
when it became Ready, if that was within 150 seconds. Everything else is
free: the tail before a sleep, a run shorter than a tick, and any stretch
when the operator was not looking.

Sampling, not deriving intervals from watch events, because a missed or
replayed watch event would have to be reasoned about, and a sample cannot
be wrong about more than its own interval.

### 2.3 Granularity and rounding

Whole seconds in the ledger, no rounding. The user sees minutes: used
rounded down, left rounded up. The expected under-count is about half a
minute per run, plus the start of any run first seen late.

### 2.4 Clock

The operator's wall clock, read once per pass. GKE nodes keep time with
Google's NTP. A step backwards, or a Ready time in the future, credits
nothing.

### 2.5 Idempotent aggregation

All of an account's mutable ledger (which sessions were awake at the last
look, how much of each grant is used, the period's totals) is in one
object's `status`, written by one writer in one update guarded by
`resourceVersion`. A tick either happened or did not. There is no second
object to fall out of step with, and re-running a tick that failed to
write changes nothing but `now`.

### 2.6 Downtime

Operator down for under 150 seconds: nothing is lost. Longer: the gap is
free for everyone, and enforcement sees a stale ledger (section 5.3).
Backend down: metering carries on; nothing is stopped at zero until it is
back, and that time is overdraft, counted but not charged.

### 2.7 Periods

A subscriber's period is Stripe's billing period, so the hours reset when
the card is charged. Everyone else's is the calendar month in UTC. An
interval that straddles a boundary is billed to the new period. Unused
plan and free hours do not carry over; purchased hours do, for 12 months.

---

## 3. Where state lives

### 3.1 The options

| | For | Against |
|---|---|---|
| **Custom resources and an operator** (recommended) | the project's rule and the product owner's earlier choice; no new service, secret, bill or failure mode; `kubectl get accounts` is the admin tool; RBAC per kind; deterministic names give create-once for free | etcd is not a database: no transactions across objects, no queries, no history, lost with the cluster unless exported |
| Cloud SQL (Postgres) | a real ledger: transactions, constraints, queries, point-in-time recovery | about $10 a month and a thing to patch, back up and connect to (a proxy sidecar or a private address, a password to rotate); the backend becomes a database client; local development needs one too |
| Firestore | no server, backed up by Google, free at this size | a new API and IAM path, an emulator for local work, and the ledger leaves the cluster where everything else is |
| Stripe as the system of record, cached | no ledger of ours | Stripe cannot hold usage that stops at zero (1.6); every decision would depend on a call to Stripe |

### 3.2 Being honest about etcd

- **Size.** An Account with 25 sessions, 100 grants and a month of days is
  a few tens of kilobytes at worst; the limit is 1.5 MiB. Lists inside it
  are capped in the schema. Ten thousand accounts with their grants and a
  year of closed periods are well under 1 GB of the 6 GB.
- **Write rate.** One status update a minute per account that has a
  session awake. The cluster can hold eleven awake sessions today, so about
  eleven writes a minute. At a thousand concurrent sessions it would be 17
  a second, which is where this design should be revisited, not before.
- **Atomicity.** There are no multi-object transactions, so the design
  does not need one: an account's ledger is one object, and Grants are
  immutable inputs to it.
- **Backup.** None by default. The plan adds a daily export of the three
  kinds to a versioned bucket (contract `deploy.md`), not Backup for GKE
  (its price and behaviour are less certain, and an export is a file a
  person can read).
- **What is lost if the cluster is recreated without an export.** Usage
  counted in the current period, free grants already used, admin grants,
  blocks. Not lost: anything paid for, because the backend's reconcile
  re-makes every plan and pack Grant from Stripe under the same names. The
  loss is the users' gain.
- **What it is not good for.** Reports across accounts (revenue, cohorts):
  those come from Stripe's dashboard, and usage reports from an export.

### 3.3 Recommendation

Custom resources, with Stripe as the record of money and the export as the
record of usage. Move to Postgres when there are a few thousand paying
accounts or a need for queries; the Grant and Account shapes map directly
onto tables.

### 3.4 The resources

| Kind | One per | Spec written by | Status written by | Kept |
|---|---|---|---|---|
| [`Account`](../contracts/billing/crd-account.yaml) | user (`acct-<owner hash>`) | backend: owner, Stripe customer, what Stripe last said of the subscription, exempt, blocked, terms | operator: plan, level, balance, the period's usage, the meter | while the user exists; 35 days after deletion |
| [`Grant`](../contracts/billing/crd-grant.yaml) | cause (`g-<hash of key>`) | backend (plan, purchase), operator (free), a person (admin); immutable but for `revoked` | operator, for display | 13 months after it expires |
| [`UsagePeriod`](../contracts/billing/crd-usageperiod.yaml) | account and closed period | operator, once | none | 13 months |

No ownerReferences between them and the Sandboxes: the ledger must outlive
a deleted session. All carry the owner label. Printer columns make
`kubectl get accounts` show owner, plan, level, balance, usage and period
end; `kubectl get grants` account, source, seconds, used, expiry, state.

Accounts are keyed by the email address, as sessions are. The risks of
that are section 8.2.

---

## 4. Stripe integration

The contract is [`stripe.md`](../contracts/billing/stripe.md).

### 4.1 What is used

| Need | Stripe feature |
|---|---|
| Subscribe to a plan | Checkout, `mode: subscription` |
| Buy a pack (pay as you go, top-up) | Checkout, `mode: payment` |
| Card, invoices, change plan, cancel | Customer Portal |
| Learn what was paid | webhooks, and a re-read of the object each time |
| Name what is sold | Products and Prices with lookup keys, made from `catalogue.yaml` |

### 4.2 Pay as you go: prepaid, not post-paid

| | Prepaid packs (recommended) | Post-paid metered price |
|---|---|---|
| Risk of not being paid | none | all of it: the usage has happened when the card is charged |
| Stops at a limit | by construction | only with our own ledger anyway |
| Surprise bills | impossible | the usual complaint |
| Mechanism | the same Grant as a top-up | a second billing path: meter events, metered subscription, arrears invoices, dunning |
| Abuse with stolen or empty cards | pays first | uses first |
| Friction | a purchase every so often | none |

The friction can be removed later with automatic top-up (charge the saved
card for a pack when the balance is low). It is not in v1: it is an
off-session charge with its own failure handling, and the product owner
asked for no automatic overage.

### 4.3 Exactly once, in any order

Every effect of a Stripe event is "make sure this Grant exists" or "write
down what the subscription looks like now". The Grant's name is a hash of
its cause (`purchase/<checkout session>`, `plan/<subscription>/<period
start>`), and the subscription is re-read from Stripe, not taken from the
event. A duplicate, a late event, or two events in the wrong order all end
in the same state. This replaces the list of processed event IDs Stripe
suggests, which would be state to keep.

The same two functions run from the webhook, from the page the user
returns to after paying (so a late webhook does not leave them waiting),
and from an hourly reconcile (so a missed webhook is repaired, and a lost
cluster is rebuilt).

### 4.4 Plan changes

Upgrades and downgrades happen in the portal, configured so that an
upgrade is charged at once and starts a new period (a new plan Grant; the
old one is superseded) and a downgrade waits for the period's end. That
this configuration behaves so is the first thing to confirm in the sandbox
with a test clock; the fallback is in the contract.

### 4.5 Failure, refund, dispute

- A failed renewal: the subscription is `past_due`; no new Grant is made;
  the user keeps the plan's limits, sees "payment failed", and runs out
  when whatever they have left is used. When Stripe gives up (a Dashboard
  setting: cancel after the retries), they are a free user.
- A refund (made by the product owner in the Dashboard): the Grant is
  revoked; what was used is not clawed back.
- A dispute: the Grant is revoked and the account blocked until the
  dispute is won or an admin unblocks it.

### 4.6 Objects as code

Recommendation: **a small idempotent command in the repository**
(`backend/cmd/stripe-setup`), run by a manual workflow, test mode first.

| | Setup command | Terraform (`stripe/stripe`) |
|---|---|---|
| Fit | five prices, four products, one portal configuration, found by fixed product IDs and lookup keys | general |
| State | none: Stripe is the state | a state file to keep, which would hold whatever secret attributes the provider exposes |
| Maturity | uses `stripe-go`, which the backend needs anyway | 0.3.0, generated, portal configuration resource not in Stripe's own list |
| Review | `--apply=false` prints the plan | `plan` |

The webhook endpoint is **not** made by code: its signing secret is shown
once in the Dashboard and has to go from there into a GitHub secret by the
product owner's hand; a script that created it would have the secret in
its output.

### 4.7 Tax

Not collected at the start: `automatic_tax` is off. Stripe's documents do
not require it from the first sale, and Stripe Tax only collects where the
business is registered. This is a business decision with legal weight;
open question 11.8.

### 4.8 Test and live

One switch, `STRIPE_MODE`, chooses which pair of secrets the deploy
workflow puts in the cluster; the backend refuses to start with a live key
in test mode or the reverse, and refuses webhook events of the other mode.
Prices are found by lookup key, so nothing else differs. The catalogue is
set up in live mode by the same workflow with a typed confirmation.

---

## 5. Enforcement

The decision table, the answers and the sweep are
[`enforcement.md`](../contracts/billing/enforcement.md).

### 5.1 Where

| Door | Today | Check |
|---|---|---|
| Create a session (UI, API token, Terraform) | `api.create` | blocked, balance, sessions limit, awake limit, capacity |
| Resume a stopped session | `api.patch` | blocked, balance, awake limit, capacity |
| Wake on use: an MCP call, the screen, an upload to a sleeping session | `proxy.Waker.EnsureAwake` | the same as resume |
| Already running | nothing per request | a sweep stops it |

The account charged is the session owner's. A token acts as its owner, so
token-driven and Terraform-driven use is the owner's use. Session policies
are not involved: a policy decides what an agent may do in a session, not
whether the session may run.

Reading, stopping and deleting are never refused.

### 5.2 Running out

| Balance | What happens |
|---|---|
| 20 % of the period's allowance (or 30 minutes) left | a warning banner, with "Buy hours" |
| zero | a banner: sessions sleep in 5 minutes; new starts are refused with "out of hours" |
| zero for 5 minutes | every running session is put to sleep the way an idle one is, **snapshot first**, marked `stopped-by: billing` |
| hours bought, or the period renews | sessions wake on use as from any sleep |

Nothing is deleted for lack of hours. The five minutes are free.

### 5.3 When something is down

- **Stripe unreachable**: nobody can buy or open the portal ("nothing was
  charged, try again"). Enforcement never calls Stripe, so nothing else
  changes.
- **Ledger stale** (the operator has not written for 10 minutes): paying
  users start and run as usual and are not charged for the time; free
  users cannot start sessions ("metering is unavailable"); nothing is
  stopped. The alternative, failing open for everyone, would make an
  operator outage free compute for any new account once sign-up is open.
- **Kubernetes API unreachable**: nothing in the product works; no
  special case.

### 5.4 Admins and exempt accounts

Addresses in `BILLING_EXEMPT_EMAILS` (by default the admins) are metered,
so their usage is visible, and never refused or stopped. An admin can also
give hours (`source: admin` Grant) or block an account with `kubectl`.
There is no admin UI in v1.

### 5.5 Capacity

The project's quota (12 CPUs, 250 GB SSD) allows two session nodes: about
eleven awake sessions beside the seven warm pods. With open sign-up that
is the scarce thing, so the table ends with admission control: at most
`MAX_AWAKE_SESSIONS` (10) awake in the cluster, of which non-paying
accounts may hold `FREE_AWAKE_CEILING` (5). Beyond that the answer is
"every desktop is in use right now, try again in a few minutes", and the
UI says the same. A queue is not in v1.

---

## 6. UI

States and copy: [`ui-states.md`](../contracts/billing/ui-states.md).
Cloudscape components as the app already uses them.

### 6.1 Usage and plan (`/billing`)

```
┌ Computer Use ────────────────────────── 12 h 30 min left ▾ ── me@x.com ┐
│ Sessions            Usage and plan              [Buy hours] [Manage billing] │
│ Usage and plan  ┌─ Hours ───────────────────────────────────────────────┐ │
│ API tokens      │ Hours used this period                                │ │
│                 │ ██████████████████████░░░░░░░░░  27 h 30 min of 40 h  │ │
│                 │ Resets 5 November 2026 · 12 h 30 min left             │ │
│                 │                                                       │ │
│                 │ Hours left      Purchased hours    Sessions   Awake   │ │
│                 │ 12 h 30 min     0                  2 of 3     up to 2 │ │
│                 │ Hours count while a session is awake. A sleeping or   │ │
│                 │ stopped session uses none. Stop it to stop the clock. │ │
│                 └───────────────────────────────────────────────────────┘ │
│                 ┌─ Plan ────────────────────────────────────────────────┐ │
│                 │ Starter · $5 a month · 40 hours                       │ │
│                 │ Renews 5 November 2026        [Change or cancel plan] │ │
│                 └───────────────────────────────────────────────────────┘ │
│                 ┌─ Usage ──────────────────── Period [5 Oct – 5 Nov ▾] ─┐ │
│                 │ h ▁▃▂▅▇▂▁▁▄▆▃▂ ...                    (hours by day)  │ │
│                 │ Session            Hours        Share                 │ │
│                 │ brave-otter        19 h 05 min  69 %                  │ │
│                 │ calm-heron          8 h 25 min  31 %                  │ │
│                 └───────────────────────────────────────────────────────┘ │
└──────────────────────────────────────────────────────────────────────────┘
```

A free user's Plan container is the upgrade path:

```
┌─ Plan ──────────────────────────────────────────────────────────────┐
│ Free · 3 hours a month · 1 session                                  │
│ ┌ Starter ───────┐ ┌ Pro ───────────┐   or buy hours as you go:     │
│ │ $5 a month     │ │ $20 a month    │   20 hours  $5                │
│ │ 40 hours       │ │ 180 hours      │   100 hours $20   [Buy hours] │
│ │ 3 sessions     │ │ 10 sessions    │                               │
│ │ [Subscribe]    │ │ [Subscribe]    │                               │
│ └────────────────┘ └────────────────┘                               │
└─────────────────────────────────────────────────────────────────────┘
```

### 6.2 Buying hours

```
┌ Buy hours ────────────────────────────────────────────┐
│ (•) 20 hours    $5      ( ) 100 hours   $20           │
│ Purchased hours are used after your plan's hours and  │
│ are valid for 12 months.                              │
│ ⓘ Test mode: no real money is taken.                  │
│                      [Cancel] [Continue to payment]   │
└───────────────────────────────────────────────────────┘
```

The browser goes to Stripe's page and returns to `/billing?checkout=...`,
which shows "Confirming your payment" and then "20 hours added".

### 6.3 Banners

```
⚠ 30 minutes of your hours left this period.            [Buy hours] [See plans] ✕
⛔ You are out of hours. Running sessions go to sleep at 14:32; nothing is lost.  [Buy hours]
⛔ You are out of hours. Your sessions are asleep and kept. Hours return on
   1 November, or buy more now.                          [Buy hours] [See plans]
⛔ Your last payment failed. Update your card to keep your plan.   [Update card]
```

### 6.4 Blocked create and blocked wake

```
Create session
┌ ⛔ You are out of hours.                    [Buy hours] [See plans] ┐
Name [ brave-otter        ]
Policy ( ... )
                                         [Cancel] [Create session ░░]

brave-otter                                              ● Asleep: out of hours
┌──────────────────────────────────────────────────────────────────┐
│        This session is kept as it was.                           │
│        It wakes when you have hours.                             │
│                  [Buy hours]  [See plans]                        │
└──────────────────────────────────────────────────────────────────┘
```

An agent calling a sleeping session's MCP endpoint gets an error whose
text says the owner is out of hours and where to add them. What Claude's
clients show for it is **UNVERIFIED** and is recorded by the backend track.

### 6.5 The public site

A pricing page with the plans and packs as a table, what counts as an
hour, and links to the terms, privacy, acceptable-use and refund pages
([`legal-pages.md`](../contracts/billing/legal-pages.md)).

---

## 7. Pricing arithmetic

### 7.1 What an awake hour costs

From `docs/warm-pool.md` and `docs/infrastructure.md`: a Spot session node
is about $0.13 an hour all in (machine, boot disk, NAT), an on-demand one
$0.21. Nine session pods fit a node today (memory decides). XFCE and a
terminal will make a session heavier, and nodes are never perfectly full:
one lingers about ten minutes after its last session sleeps, and the first
session on a node pays for all of it.

Node cost per awake session-hour, Spot:

| Sessions that fit a node | Node full | Node half full on average |
|---|---|---|
| 9 (today) | $0.015 | $0.029 |
| 6 (with a desktop, assumed) | $0.022 | $0.044 |
| 4 (pessimistic) | $0.033 | $0.066 |

Traffic (the screen's stream out at about $0.12 per GB, the browser's
fetches through NAT at about $0.045 per GB) is assumed at $0.01 an hour;
that is a guess to be measured.

- **Planning figure: $0.055 per awake hour** (6 per node, half full, plus
  traffic).
- **Pessimistic: $0.075** (4 per node, half full, plus traffic). On-demand
  nodes instead of Spot are in the same range ($0.08 at 6 per node).
- The figure the $5 plan was sized on (9 per node, full): $0.014.

Kept per session, awake or not: the 5 GB disk and a snapshot, **$0.55 a
month**.

Fixed: about **$178 a month** (system node, load balancer, the warm pool's
node and disks), whoever uses it.

Stripe: 2.9 % + $0.30 a charge, plus 0.7 % on subscriptions.

### 7.2 The proposed catalogue, at full use

"Full use" is every included hour used and every allowed session kept: the
worst customer.

| | Price | Hours | $/hour | Sessions kept / awake | Stripe | Net | Cost, planning | Margin, planning | Margin, pessimistic |
|---|---|---|---|---|---|---|---|---|---|
| Free | $0 | 3 | | 1 / 1 | | $0 | $0.72 | -$0.72 | -$0.78 |
| Starter | $5 | 40 | $0.125 | 3 / 2 | $0.48 | $4.52 | $3.85 | +$0.67 | -$0.13 |
| Pro | $20 | 180 | $0.111 | 10 / 4 | $1.02 | $18.98 | $15.40 | +$3.58 | -$0.02 |
| Scale | $100 | 1000 | $0.100 | 25 / 8 | $3.90 | $96.10 | $68.75 | +$27.35 | +$7.35 |
| Pack, 20 h | $5 | 20 | $0.250 | | $0.45 | $4.55 | $1.10 | +$3.45 | +$3.05 |
| Pack, 100 h | $20 | 100 | $0.200 | | $0.88 | $19.12 | $5.50 | +$13.62 | +$11.62 |

Cost = hours x the hourly figure + sessions x $0.55. For example Starter:
40 x 0.055 + 3 x 0.55 = $3.85. Packs carry no storage in the table; a
pay-as-you-go account's kept sessions (up to 3, $1.65 a month) are the
reason those sessions are deleted after 60 unused days.

A typical customer uses about half (assumption): Starter with 20 hours and
2 sessions costs $2.20 and leaves **+$2.32**; Pro with 90 hours and 4
sessions costs $7.15 and leaves **+$11.83**.

### 7.3 What the arithmetic says

- **The $5 for 40 hours plan holds**, with a thin margin against the worst
  customer and nothing to spare if a desktop session turns out to need a
  quarter of a node. At the density it was sized on (9 per node, full) it
  costs $2.21 at full use and leaves $2.31. It is the plan most exposed to
  storage: three kept disks are a third of its revenue.
- **Storage, not compute, is the cost of free and light users.** A free
  user who used their three hours costs $0.17 in compute and $0.55 for the
  disk they leave behind. Hence one session for free accounts, and
  deleting free sessions unused for 14 days (11.5).
- **Volume discounts have to be small.** Starter's rate is already near
  cost; Pro at 180 hours ($0.111) and Scale at 1000 ($0.10) are what the
  pessimistic column allows. 200 and 1200 would read better and lose money
  on the worst customer.
- **Packs are priced above every plan** ($0.20 to $0.25 against $0.10 to
  $0.125), so a regular user is better off subscribing, and packs carry
  the margin that plans do not.
- **Break-even on the fixed $178**: about 77 typical Starter subscribers,
  or 15 typical Pro. A hundred active free users cost about $72 a month.
- **Scale cannot be sold yet.** A thousand hours a month is 1.4 sessions
  awake around the clock on average and up to 8 at once, in a cluster with
  eleven places. It is in the catalogue with `enabled: false` until the
  CPU quota is raised.

### 7.4 Free allowance

**3 hours a month, 1 session, 1 awake, no card.** Enough to try the
product on three or four occasions; small enough that a hundred free users
cost less than fifteen Starter subscribers bring in; not worth farming
(3 hours of a fifth of a CPU). 2 hours is defensible; 5 raises the cost of
abuse by two thirds for little gain in conversion.

---

## 8. Open sign-up

The risky part. It is its own stage, last, switched separately
(`OPEN_SIGNUP` and one Pomerium policy change in one pull request), and
depends on the prerequisites of 8.5.

### 8.1 What changes

| | Today | Open |
|---|---|---|
| Pomerium policy on `app` and the session MCP routes | two email addresses | `authenticated_user: true` |
| API tokens (`ALLOWED_EMAILS`) | the same two addresses | any account that exists and is not blocked |
| Admins | `ADMIN_EMAILS` | unchanged |
| Google OAuth app | "Testing" | "In production", brand verified |
| GitHub OAuth app | no restriction | unchanged |
| First visit | sessions list | terms acceptance, then the list |

### 8.2 Identity

Users are identified by email address across both providers: sessions are
owned by it, and so will Accounts be. Consequences once anyone can sign
in:

- **An unverified email must not be an identity.** Dex's GitHub connector
  only yields a verified primary address. Dex's Google connector passes
  `email_verified` through without refusing. Google accounts with an
  unverified non-Gmail address exist. Before sign-up opens it must be
  established, by test, that such an account cannot sign in as that
  address; if it can, the fix is a check of the claim (in Dex's
  configuration if it has one, else in the backend from Pomerium's
  assertion; whether the assertion carries the claim is **UNVERIFIED**).
  This is prerequisite P4.
- The same address through Google and through GitHub is one account. That
  is the intent, and it rests on both addresses being verified.
- One person can have many Google and GitHub accounts. Nothing here stops
  that; the limits below make it not worth much.

### 8.3 Abuse

A free account gets a browser with unrestricted internet access, and soon
a terminal, on our address.

| Threat | Control | In |
|---|---|---|
| Farming free hours with many accounts | 3 hours and one session each; at most 3 new accounts per IP address and 50 per day in all (a breaker, not a defence); one free allowance per address per month even after deleting the account; `FREE_TIER=off` stops new free grants at once | stage 4 |
| Filling the cluster | the cluster-wide awake cap and the free ceiling (5.5): free users can never take a paying user's place | stage 3 |
| Crypto-mining | a session's two containers are limited to 2 CPUs together (`deploy/gke/blueprint.yaml`) and guaranteed a fifth of one; 3 free hours of that is worth cents, and a paid hour costs more than it could mine. What it does cost is the neighbours' performance on the node: a lower CPU limit for free accounts is worth considering with the terminal decision | exists |
| Spam | Google Cloud blocks outbound port 25 (**UNVERIFIED** here; to confirm); ports 465 and 587 to be blocked for all sessions in the session NetworkPolicy | stage 4 |
| Attacks on the cluster or the cloud metadata | already blocked: private ranges and the metadata address are excluded from session egress; gVisor | exists |
| Using sessions as a proxy, scraping or attack source from our address | no inbound ports exist; outbound is a browser. Acceptable-use policy; an abuse address; the record of which account's session was awake when (the ledger) to answer a complaint; blocking an account sleeps its sessions at once | stage 4 |
| A terminal | raises every row above: tools for scanning and tunnelling are one command away. Recommended: no terminal for free accounts until there is egress filtering per tier (open question 11.7) | decision |
| Card testing through Checkout | every checkout needs a signed-in account; Stripe's own controls on Checkout; rate limit on checkout creation per account (10 a day) | stage 2 |
| Chargebacks | account blocked on dispute; hours revoked | stage 2 |
| A forged webhook | signature over the raw body, 5 minute tolerance, mode check; the handler re-reads Stripe before granting, so even a forged event grants only what Stripe says was paid | stage 2 |
| Wake storms to dodge the meter (runs under a minute are free) | 30 starts per account per hour | stage 3 |

Not proposed for the start: requiring a card for the free tier (it is the
strongest control and the product owner chose no card; it is the first
thing to turn to if abuse appears), CAPTCHA, phone verification, GitHub
account age.

### 8.4 Pages and account deletion

- Terms, privacy, acceptable use, refunds and cancellation, pricing,
  contact: needed by Stripe to activate the account and by Google to
  verify the brand. Drafts of what each must say are in
  [`legal-pages.md`](../contracts/billing/legal-pages.md), for the product
  owner to review. They are not legal advice.
- A user can delete their account in the app: sessions, disks, snapshots
  and tokens go, the subscription is cancelled, and a marker stays 35 days
  so the same address gets no second free month.

### 8.5 Before sign-up opens, and what can follow

**Must exist first:**

| | Prerequisite |
|---|---|
| P1 | `BILLING=enforce` has run in production for at least a week with the allow-listed users, with no metering discrepancy |
| P2 | The awake cap, the free ceiling and the sign-up limits are on and tested |
| P3 | Terms, privacy and acceptable-use pages are published and the terms modal records acceptance |
| P4 | The email-verification question of 8.2 is answered by test, and fixed if needed |
| P5 | The Google OAuth app is "In production", and a Google account that is not a test user can sign in |
| P6 | Account deletion works |
| P7 | Blocking an account works end to end (sessions asleep, sign-in refused), and the abuse address is read |
| P8 | Ports 465 and 587 are blocked for sessions; port 25 confirmed blocked |
| P9 | The daily export of the ledger runs, and a restore was rehearsed |
| P10 | The terminal decision (11.7) is made |

**Can follow:** live payments (independent: sign-up can open on the free
tier with test-mode payments hidden, or after live payments); brand
verification's logo; a waiting queue; per-tier egress filtering; an admin
page; email notices.

---

## 9. Security review

- **Keys.** Three per mode: a restricted run-time key, the webhook secret,
  a restricted setup key used only by the setup workflow. GitHub secrets,
  entered by the product owner; the deploy workflow passes them to
  `kubectl` on stdin as it does Dex's. The backend never logs them and
  checks only a key's prefix. The operator has none.
- **The webhook** is the only new public, unauthenticated path. It is on
  the API host, matches one exact path, accepts 1 MiB, verifies before
  parsing, and acts only on what a fresh read of Stripe confirms.
- **Who can write the ledger.** The backend cannot write
  `accounts/status`; the operator cannot write an Account's `spec` and has
  no network path but the API server. A user cannot reach either kind:
  there is no API route that writes a Grant from user input, and session
  pods cannot reach the API server.
- **The backend gains the right to create Grants**, which are money. A
  compromised backend could mint hours. It can already delete every
  session; the exposure is accepted and bounded by the reconcile (which
  does not remove unknown Grants in v1; an audit job that flags Grants
  with no Stripe object behind them is a small later addition).
- **Checkout redirect URLs** are built from `PUBLIC_URL`, never from the
  request.
- **Fail directions** are stated: metering fails free, enforcement fails
  open for payers and closed for free users, payment fails with nothing
  charged.
- **Privacy.** The ledger holds email addresses and which session was
  awake on which day. Stripe holds card data; we hold its IDs. Sign-up IP
  addresses are kept only as salted hashes for 35 days.
- **Test mode in production.** With `STRIPE_MODE=test` the UI says so on
  every purchase surface, and a test payment grants real hours. While only
  the two allow-listed users exist that is the point; sign-up must not
  open in that state: before stage 4 either live payments are on or
  `STRIPE_MODE` is unset (nothing can be bought).

---

## 10. Not in v1

- Post-paid usage, automatic top-up, Stripe Billing Meters and credits.
- Metering or charging for storage, traffic, or the warm pool.
- Annual plans, trials, coupons, teams or shared accounts, invoices to
  companies beyond what the portal gives, currencies other than USD.
- Stripe Tax.
- Emails of any kind (low balance, receipts beyond Stripe's own, deletion
  warnings): the app has no mail sender. Stripe sends receipts and failed
  payment emails itself in live mode.
- An admin UI: `kubectl` and the Stripe Dashboard.
- A waiting queue when the cluster is full.
- Not billing idle minutes; per-second display.
- Per-tier egress rules.
- Proration of hours on plan change beyond "an upgrade starts a new
  period".
- Refunds from the app (the Dashboard does them; the webhook follows).

---

## 11. Open questions for the product owner

Each with the default this design assumes.

| # | Question | Recommended default |
|---|---|---|
| 11.1 | What do the three plans include? | Starter $5: 40 h, 3 sessions, 2 awake. Pro $20: 180 h, 10 sessions, 4 awake. Scale $100: 1000 h, 25 sessions, 8 awake, **not sold until the CPU quota is raised**. |
| 11.2 | Pack sizes and prices (pay as you go and top-up are the same packs)? | 20 h for $5, 100 h for $20, valid 12 months, same price for subscribers and non-subscribers. |
| 11.3 | Free allowance? | 3 h a month, 1 session, no card, does not roll over. |
| 11.4 | Is the idle time before sleep billed? | Yes; the UI says to stop a session to stop the clock. |
| 11.5 | Are unused sessions of non-subscribers deleted? | Yes: free after 14 unused days, pay as you go after 60, shown in the app from 7 days before. Off until explicitly turned on. Subscribers' sessions are never deleted. |
| 11.6 | Shorter idle time for free accounts (5 minutes)? | Not in v1 (it needs a per-session idle time in the backend); revisit if free usage is mostly idle tail. |
| 11.7 | Do free accounts get the terminal? | No, until there is per-tier egress filtering. Paid accounts do. |
| 11.8 | Collect sales tax or VAT at the start? | No (`automatic_tax` off); ask an accountant before live mode; consider US-only at first. |
| 11.9 | Grace when hours run out? | 5 minutes, free, then sleep with a snapshot. |
| 11.10 | Ledger stale: who may start sessions? | Paying accounts yes (unmetered), free accounts no. |
| 11.11 | Plan change behaviour? | Upgrade: charged at once, new period, new hours, old plan's unused time credited by Stripe. Downgrade: at the period's end. |
| 11.12 | What happens to a plan when a payment fails? | Stripe retries on its schedule; the Dashboard is set to cancel the subscription after the last retry; meanwhile no new hours. |
| 11.13 | Refund policy? | No refunds for part periods or unused hours except case by case; stated on the site. |
| 11.14 | A general Stripe sandbox, or the test-mode sandbox? | A general sandbox named `computeruse-dev` (Stripe's advice for new integrations). |
| 11.15 | Does the ledger stay in custom resources? | Yes, with the daily export; revisit at a few thousand paying accounts. |
| 11.16 | Are admins billed? | No: metered and exempt. |
| 11.17 | Is the legal entity, address and support email for Stripe and the site decided? | Needed before live mode and before the pages are published; the product owner's to supply. |
| 11.18 | Does sign-up open before or after live payments? | After: opening with only a free tier invites the users who will never pay first. |

---

## 12. What the product owner does by hand

In order. No key is pasted anywhere but a GitHub secret field.

**For test mode (stage 2):**

1. Create a Stripe account at https://dashboard.stripe.com/register. Do
   not activate payments yet.
2. In the Dashboard's sandbox menu, create a sandbox named
   `computeruse-dev` and switch to it.
3. Developers, API keys, "Create restricted key", named `setup`:
   write access to Products, Prices and Customer portal; read on the rest
   it asks for. Put it in the GitHub repository secret
   `STRIPE_TEST_SETUP_KEY`.
4. A second restricted key named `backend`: write on Checkout Sessions,
   Customers, Customer portal, Subscriptions; read on Prices, Products,
   Invoices, PaymentIntents, Charges, Refunds, Disputes. Secret
   `STRIPE_TEST_API_KEY`. (The backend track will say if a permission is
   missing; the key is then edited in the Dashboard, not replaced.)
5. Run the workflow "stripe setup" with `mode: test`, `apply: false`, read
   the plan, then again with `apply: true`.
6. Developers, Webhooks, "Add destination": URL
   `https://api.computeruse.site/stripe/webhook`, the events listed in
   `docs/contracts/billing/stripe.md`, the API version the backend track
   names. Reveal the signing secret and put it in the secret
   `STRIPE_TEST_WEBHOOK_SECRET`.
7. Set the repository **variable** `STRIPE_MODE` to `test`.
8. Merge the pull request that sets `BILLING=meter`, and run "deploy".
9. Buy a pack with the card `4242 4242 4242 4242` and see the hours
   arrive.

**For open sign-up (stage 4):**

10. Google Cloud console, Google Auth Platform, Audience: first try
    signing in to the app's Dex page with a Google account that is not a
    test user and tell the team what happened; then "Publish app".
11. Branding: app name "Computer Use", support email, homepage
    `https://computeruse.site`, privacy policy and terms links, authorised
    domain `computeruse.site`; verify the domain in Search Console; submit
    for brand verification.
12. Review, rewrite and approve the terms, privacy, acceptable-use and
    refund pages; supply the legal name, address and support and abuse
    email addresses.
13. Ask for a higher CPU quota in the Google Cloud project if more than
    eleven concurrent sessions are wanted.
14. Merge the stage 4 pull request and deploy.

**For live payments (stage 5):**

15. Activate the Stripe account: business details, bank account, the
    website `https://computeruse.site` (which by then has pricing,
    contact, refund and privacy pages).
16. In live mode: Settings, Billing, "Manage failed payments": cancel the
    subscription after the last retry. Settings, Customer emails: receipts
    and failed-payment emails on.
17. Repeat steps 3 to 6 in live mode with the secrets
    `STRIPE_LIVE_SETUP_KEY`, `STRIPE_LIVE_API_KEY`,
    `STRIPE_LIVE_WEBHOOK_SECRET` (the setup workflow with `mode: live`
    asks for a typed confirmation).
18. Set the variable `STRIPE_MODE` to `live` and deploy.
19. Buy the smallest pack with a real card, see the hours, refund it in
    the Dashboard, see them go.

---

## 13. Alternatives rejected

| Alternative | Why not |
|---|---|
| Stripe Billing Meters with a metered price for overage or pay as you go | bills in arrears, no stop at a limit, asynchronous, credits only at invoice time; the product owner chose no automatic overage (1.6, 4.2) |
| Stripe billing credits as the top-up balance | apply only to metered subscription items, reconciled only at invoice time (1.6) |
| A separate credit currency ("credits" worth some minutes each) | one product, one resource: hours are what the user already understands |
| Dollars as the stored balance | reads as stored value; hours of service do not, and the hourly price can differ by plan without a conversion |
| The backend as the meter | the component that restarts most would hold the money state (2.2) |
| Deriving intervals from watch events | a missed event is silent; a sample is self-correcting (2.2) |
| One ledger entry object per minute or per interval | thousands of objects a day for no gain over a counter in one object |
| A database for the ledger now | cost and operations out of proportion to eleven concurrent sessions (3.1) |
| Keeping processed event IDs | state, where deterministic names need none (4.3) |
| Creating the webhook endpoint from code | its secret would pass through a log (4.6) |
| Terraform for Stripe's objects | a state file and a 0.x provider for nine objects (4.6) |
| Failing open for everyone on a stale ledger | free compute for any new account during an operator outage (5.3) |
| Failing closed for everyone | locks out customers for our fault |
| Deleting data when hours run out | sleeping costs $0.55 a month and keeps the customer |
| The webhook on the app host | that host has no public path today and should not gain one |
| Extending the policy operator | different rights and a shared restart |
