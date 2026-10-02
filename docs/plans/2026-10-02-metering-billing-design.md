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

Settled by the product owner, in the order they were made:

1. **What is metered is awake session time.** Sleeping and stopped time
   uses no hours. $5 a month was sized at 40 awake hours.
2. **No automatic overage.** When what was paid for runs out, sessions
   stop; the user can buy more as a one-off purchase.
3. **Four paid options**: "$5, $20, $100 and PAYG tiers". Only the prices
   were given; what each includes is proposed here (section 7).
4. **A card before anything**: "I want the user to have to attach a payment
   method before they can create any resource, to make sure we capture
   intent." This replaces the earlier free allowance with no card.
5. **$5 of credit at sign-up**, given when the card is attached.
6. **Disks are billed**: a session's persistent disk is charged while it
   exists, awake or not, from the same balance.
7. **Calls in flight finish**: when credit runs out, a call that is already
   executing completes; then the session is snapshotted and put to sleep.
   No new calls are accepted.
8. **Removing the last payment method puts the account's sessions to
   sleep**, and create and wake are refused until a card is back.
9. **A required test**: users without a card cannot create an instance,
   and a scenario covering no card, card, card removed. The stateful
   components are interfaces so that this runs without Kubernetes or
   Stripe.
10. **Sign-up opens to anyone** with a Google or GitHub account. Admins
    stay a list.
11. **Stripe test mode first.** There is no Stripe account yet. The product
    owner creates it and enters every key as a GitHub Actions secret; no key
    is ever in the repository, a log or a chat, and no agent handles one.
12. The pattern of the session-policies feature: design, written contracts,
    concurrent tracks, everything behind a flag that is off by default.

## Summary of the proposal

- **One balance, in dollars.** Every account has one balance of credit. Two
  things use it: **$0.20 for each hour a session is awake**, and **$1.40 a
  month for each session kept** (its 5 GB disk), awake or asleep. Credit
  comes as **grants**: $5 once at sign-up, a plan's monthly credit, a pack
  that was bought. The grant that expires first is used first.
- **The four options are one mechanism.** Pay as you go buys credit at face
  value ($5, $20 or $50 packs). A subscription buys more credit than it
  costs, every month: $5 buys $10, $20 buys $44, $100 buys $240. A
  subscriber who runs out tops up with the same packs. Optional
  **auto-recharge** buys a pack with the saved card when the balance is low,
  up to a monthly cap the user sets: the feel of post-paid with none of its
  risk. Nothing is ever owed.
- **A card before anything.** A new user accepts the terms, then saves a
  card through Stripe Checkout in setup mode (nothing is charged). The
  verified webhook makes the account active and grants the $5, **once per
  card**: the grant is named after the card's fingerprint, so a card that
  has had it cannot earn it again on any account. Prepaid cards and
  wallet-tokenised cards are saved but earn no credit.
- **Stripe takes payments, saves cards and runs subscriptions; it does not
  meter and it does not stop anything.** Stripe's usage-based billing bills
  in arrears, cannot stop a customer at a limit, and its credits do not
  apply to flat plans (section 1.6). **Stopping is ours.**
- **The ledger is ours, in custom resources.** `Account`, `Grant`,
  `UsagePeriod`. A new **billing operator** (kopf, one replica) is the only
  thing that decrements a balance: every 60 seconds it looks at every
  session's Sandbox, charges what it saw, and debits grants, in one atomic
  update per account. The backend stays stateless: it reads the ledger to
  decide, and writes what Stripe tells it.
- **Never over-bill.** Only what was observed twice, at most 150 seconds
  apart, is charged. When the operator is down, time is free. The balance
  never goes below zero: there is no debt.
- **Exactly once without a database.** A Grant's name is the hash of what
  caused it (a card, a checkout session, a subscription period, an
  auto-recharge payment). Stripe may send an event twice, late or out of
  order: every handler re-reads Stripe's current state and creates the Grant
  if it is not there.
- **At zero**: a warning before; at zero a five minute grace (so a top-up in
  progress interrupts nothing); then each running session stops taking new
  calls, lets calls in flight finish (bounded by the proxy's own 10 minute
  limit), is snapshotted and put to sleep, marked "out of credit". Create
  and wake answer **402** with a message and a link, not an error a client
  would retry. Credit arriving makes sessions wakeable; it does not wake
  them. **No card**: the same sleep, at once, with its own reason.
- **Disks at zero** cost nothing to the user and something to us, so after
  14 days at a zero balance the sessions are deleted, with notices in the
  app. That deletion stays switched off until the product can send email.
- **Three switches.** `BILLING` = `off` (default), `meter` (count and show,
  refuse nothing), `enforce`. `STRIPE_MODE` = unset, `test`, `live`.
  `OPEN_SIGNUP`, the last stage, with its own prerequisites (section 8).
- **Testable by construction**: accounts, ledger, clock, Stripe and the
  session store are Go interfaces with in-memory fakes, and the card-gate
  scenario is written out step by step with its expected statuses and
  bodies ([`testing.md`](../contracts/billing/testing.md)).

```
 user ── Pomerium ── backend ──────────────── Stripe (Checkout: save card,
                       │  reads Account.status      │  subscribe, buy credit;
                       │  writes Account.spec,      │  Portal; off-session charge)
                       │  Grants (from Stripe)      ▼ webhook, signed
                       │◄───────────── api.computeruse.site/stripe/webhook
                       │  sweep: drain, snapshot, sleep
                       ▼
             Account, Grant, UsagePeriod  (custom resources)
                       ▲
                       │ sole writer of Account.status: the only thing
                       │ that decrements a balance
              billing operator ── watches ── Sandboxes (exists? awake?)
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

Our shape is "prepaid credit, plans that grant credit, stop at zero".
Stripe's metering would give us none of the three: we would still need our own
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
  one-off Checkout payment or an off-session PaymentIntent is not stated
  outright (the pricing page says Billing volume "excludes one-off
  invoices"); the arithmetic assumes it does not. **UNVERIFIED**.
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

### 1.11 Cards on file

- Checkout has `mode: setup`: "Save payment details to charge your
  customers later." It "uses the Setup Intents API to create Payment
  Methods"; `currency` is required; with a `customer` "Stripe automatically
  attaches the resulting payment method to that Customer". The completed
  session carries `setup_intent`, whose `payment_method` is the saved card.
  (https://docs.stripe.com/api/checkout/sessions/create,
  https://docs.stripe.com/payments/checkout/save-and-reuse)
- Saving a card checks it: "Stripe may send a request to the issuing bank
  for either a $0, $1, or similar authorization to verify that the card is
  valid." It proves nothing about funds.
  (https://support.stripe.com/questions/unexpected-1-charge-on-customers-bank-statement).
  Whether Stripe charges a fee for a card save is **UNVERIFIED** (no fee is
  listed).
- A one-off Checkout payment does not save the card unless
  `payment_intent_data.setup_future_usage` is set; "if you use Checkout in
  `subscription` mode, Stripe automatically saves the payment method".
  (https://docs.stripe.com/payments/checkout/save-during-payment)
- `card.fingerprint`: "Uniquely identifies this particular card number. You
  can use this attribute to check whether two customers who've signed up
  with you are using the same card number ... For payment methods that
  tokenize card information (Apple Pay, Google Pay), the tokenized number
  might be provided instead of the underlying card number."
  (https://docs.stripe.com/api/payment_methods/object). It is "unique for a
  given Account" and unchanged by a new expiry date
  (https://support.stripe.com/questions/how-can-i-detect-duplicate-cards-or-bank-accounts).
  Whether a sandbox and live mode give the same fingerprint is
  **UNVERIFIED**. `card.funding` is `credit`, `debit`, `prepaid` or
  `unknown`.
- `payment_method.detached` "occurs whenever a payment method is detached
  from a customer"; the detached object has `customer: null`, and
  `previous_attributes` is "only included in events of type `*.updated`".
  So the event does not say whose card it was: we keep the list of each
  customer's payment method IDs. Events on sub-resources "don't trigger a
  corresponding event for the parent resource (`customer.updated`)".
  (https://docs.stripe.com/api/events/types,
  https://docs.stripe.com/api/payment_methods/detach)
- The portal lets customers manage payment methods; "if your customer has
  an active subscription the portal requires them to maintain at least one
  payment method" (https://support.stripe.com/questions/billing-customer-portal).
  A customer with no subscription can remove their last card there.
- Charging a saved card with nobody present: a PaymentIntent with
  `off_session: true` and `confirm: true`. "If a payment attempt fails, the
  request also fails with a 402 HTTP status code, and the PaymentIntent
  status is requires_payment_method"; with the decline code
  `authentication_required`, "bring your customer back online to complete
  the payment". Stripe asks that the terms cover "the customer's agreement
  to your initiating a payment ... on their behalf", "the anticipated timing
  and frequency of payments (... unscheduled top-ups)" and "how you
  determine the payment amount", and that a record of the agreement is
  kept. (https://docs.stripe.com/payments/save-and-reuse,
  https://docs.stripe.com/payments/setup-intents)
- Radar "evaluates risk and runs rules for three Stripe API objects:
  Charges, PaymentIntents and SetupIntents", but "when saving a customer's
  payment method without an initial payment, Radar doesn't act on the
  SetupIntent by default"; it is a setting, "Use Radar on payment methods
  saved for future use". Custom rules (for example on `:card_funding:`)
  need a paid Radar plan. Card testers prefer card saves "because card
  validation and authorizations during card setup don't typically show up
  on cardholder statements"; Stripe's advice includes "limit the number of
  cards that can be added to an account" and limiting new customers per IP
  address. (https://docs.stripe.com/radar/transaction-risk-prevention,
  https://docs.stripe.com/disputes/prevention/card-testing)

---

## 2. Metering

The exact rules, the algorithm and 18 test vectors are
[`metering.md`](../contracts/billing/metering.md). This section is why.

### 2.1 The unit: dollars

The sign-up credit is "$5", disks are charged, and there are four ways to
pay. Hours cannot express all three; dollars can.

| | Hours as the unit | **Dollars as the unit** (recommended) |
|---|---|---|
| The sign-up credit | "$5" has to be converted | is $5 |
| Disks | a second unit, or "disk-hours" turned into awake hours | a second rate on the same balance |
| Plans | a plan includes N hours | a plan gives more credit than it costs |
| A different rate later (a bigger machine, a GPU) | a new unit each time | a new rate |
| What the user sees | "12 hours left" | "$2.40 left, about 12 hours at your current use" |

The ledger is whole micro-dollars. Rates are per hour, and each tick's
charge carries its remainder, so an hour awake costs exactly $0.20 however
the ticks fall.

This is credit for the service, not money: it cannot be withdrawn or
transferred, it expires, and the terms say so. Whether it raises any
stored-value question is for the product owner's adviser (11.19).

### 2.2 What is charged

| | Awake charge | Disk charge |
|---|---|---|
| A warm pod before a user takes it | no | no |
| Starting: waiting for a node, restoring a snapshot | no | yes |
| Awake and used | yes | yes |
| Awake and idle, the 15 minutes before sleep | **yes** | yes |
| Asleep, stopped, failed | no | **yes** |
| Deleted | no | no |

The idle tail is charged because it costs what an active minute costs: the
session holds its place on the node. The UI says how to stop the clock.

The disk is charged per session, per GB, per hour, from the tick after the
session is created to its deletion: the same sampling as awake time, a
second rate. The real cost is about $0.10 per GB-month plus a snapshot,
$0.55 a month for a session; the proposed rate is $0.28 per GB-month,
$1.40 a month, because plan credit is discounted by up to 2.4 times and the
disk has to stay above cost on every plan (section 7).

### 2.3 Who decrements, and how often

The **operator**, every 60 seconds, and nothing else. Three ways to measure
were considered:

| | For | Against |
|---|---|---|
| The backend's sweeper decrements | it already sweeps | the backend restarts on every deploy; it would become the keeper of money state it was built not to keep; metering would stop when the app is down though sessions run |
| Kubernetes events | free | kept for an hour, dropped under load: not a record |
| **An operator samples the Sandboxes** | the Sandbox's status is the truth the UI shows; independent of the backend; one writer; trivially restartable | sampling loses partial minutes (in the user's favour) |

It charges a session the time between two looks only when they were at
most 150 seconds apart (and, for awake time, awake at both). The first look
at a newly awake session charges back to when it became Ready, if that was
within 150 seconds. Everything else is free: the tail before a sleep, a run
shorter than a tick, and any stretch when the operator was not looking.

The backend **acts** on the ledger (refuses, drains, sleeps); it never
writes it.

### 2.4 Atomic, idempotent

All of an account's mutable ledger (its sessions at the last look, how much
of each grant is used, the carried remainders, the period's totals) is in
one object's `status`, written by one writer in one update guarded by
`resourceVersion`. A tick either happened or did not.

### 2.5 Downtime

Operator down for under 150 seconds: nothing is lost. Longer: the gap is
free for everyone, and enforcement sees a stale ledger (section 5.6).
Backend down: metering carries on; nothing is stopped at zero until it is
back, and that time is overdraft, counted but owed by nobody.

### 2.6 Periods

Charging does not depend on a period. A period only decides when a plan's
credit arrives and expires (Stripe's billing period) and what the usage
page shows (that, or the calendar month for someone with no plan).

---

## 3. Where state lives

### 3.1 The options

| | For | Against |
|---|---|---|
| **Custom resources and an operator** (recommended) | the project's rule and the product owner's earlier choice; no new service, secret, bill or failure mode; `kubectl get accounts` is the admin tool; RBAC per kind; deterministic names give create-once for free | etcd is not a database: no transactions across objects, no queries, no history, lost with the cluster unless exported |
| Cloud SQL (Postgres) | a real ledger: transactions, constraints, queries, point-in-time recovery | about $10 a month and a thing to patch, back up and connect to; the backend becomes a database client; local development needs one too |
| Firestore | no server, backed up by Google, free at this size | a new API and IAM path, an emulator for local work, and the ledger leaves the cluster where everything else is |
| Stripe as the system of record, cached | no ledger of ours | Stripe cannot hold a balance that stops at zero (1.6); every decision would depend on a call to Stripe |

### 3.2 Being honest about etcd

- **Size.** An Account with 25 sessions, 100 grants and a month of days is
  a few tens of kilobytes at worst; the limit is 1.5 MiB. Lists inside it
  are capped in the schema. Ten thousand accounts with their grants and a
  year of closed periods are well under 1 GB of the 6 GB.
- **Write rate.** One status update a minute per account that has a
  session (disks are charged while asleep, so every account with a session
  is written every minute). A thousand accounts are 17 writes a second;
  that is where this design should be revisited (charging sleeping accounts
  every 15 minutes instead is the first step, and changes no result).
- **Atomicity.** There are no multi-object transactions, so the design
  does not need one: an account's ledger is one object, and Grants are
  immutable inputs to it.
- **Backup.** None by default. The plan adds a daily export of the three
  kinds to a versioned bucket (`deploy.md`).
- **What is lost if the cluster is recreated without an export.** Charges
  counted in the current period, admin grants, blocks, and **the record of
  which cards have had the sign-up credit** (so a card could earn it again;
  this is the one loss that costs us, and the reason the export is a
  prerequisite of open sign-up). Not lost: anything paid for, because the
  reconcile re-makes every plan, pack and recharge Grant from Stripe under
  the same names; and whether an account has a card, re-read from Stripe.
- **What it is not good for.** Reports across accounts: those come from
  Stripe's dashboard and from the export.

### 3.3 Recommendation

Custom resources, with Stripe as the record of money and cards, and the
export as the record of usage. Move to Postgres when there are a few
thousand accounts or a need for queries; the Grant and Account shapes map
directly onto tables, and the Go interfaces of
[`testing.md`](../contracts/billing/testing.md) are the seam.

### 3.4 The resources

| Kind | One per | Spec written by | Status written by | Kept |
|---|---|---|---|---|
| [`Account`](../contracts/billing/crd-account.yaml) | user (`acct-<owner hash>`) | backend: owner, Stripe customer, payment methods, the sign-up credit's outcome, the subscription as Stripe last gave it, auto-recharge, exempt, blocked, terms | operator: plan, level, balance by source, burn rate, the period's usage, the meter | for as long as the user exists; a stub after deletion |
| [`Grant`](../contracts/billing/crd-grant.yaml) | cause (`g-<hash of key>`) | backend (sign-up, plan, purchase, recharge), a person (admin); immutable but for `revoked` | operator, for display | 13 months after it expires; sign-up grants for ever |
| [`UsagePeriod`](../contracts/billing/crd-usageperiod.yaml) | account and closed period | operator, once | none | 13 months |

No ownerReferences between them and the Sandboxes: the ledger must outlive
a deleted session. All carry the owner label. Printer columns make
`kubectl get accounts` show owner, card, plan, level, balance and period
end; `kubectl get grants` account, source, amount, used, expiry, state.

Accounts are keyed by the email address, as sessions are. The risks of
that are section 8.2.

---

## 4. Stripe integration

The contract is [`stripe.md`](../contracts/billing/stripe.md).

### 4.1 What is used

| Need | Stripe feature |
|---|---|
| Save a card without charging (the gate) | Checkout, `mode: setup` |
| Subscribe to a plan | Checkout, `mode: subscription` |
| Buy credit (pay as you go, top-up) | Checkout, `mode: payment`, saving the card |
| Auto-recharge | a PaymentIntent, `off_session`, on the saved card |
| Card, invoices, change plan, cancel | Customer Portal |
| Learn what happened | webhooks, and a re-read of the object each time |
| Name what is sold | Products and Prices with lookup keys, made from the catalogue |

### 4.2 Pay as you go: prepaid, not post-paid

| | **Prepaid credit** (recommended) | Post-paid, Stripe Billing Meters, a monthly invoice |
|---|---|---|
| Risk of not being paid | none | all of it: the usage has happened when the card is charged, and a saved card proves no funds |
| Stops at a limit | by construction | only with our own ledger anyway, plus a spending cap to design |
| Surprise bills | impossible | the usual complaint |
| Mechanism | the same Grant as a top-up and as a plan's credit | a second billing path: meter events, a metered subscription, arrears invoices, dunning |
| Abuse with stolen or empty cards, now that sign-up is open | pays first | uses first |
| Friction | a purchase every so often; none with auto-recharge | none |

**Auto-recharge** removes the friction: with the user's recorded agreement,
when the balance falls below a threshold the saved card is charged for a
pack, never more than a monthly cap the user sets, off by default. If the
card is declined or the bank asks for authentication, auto-recharge turns
itself off and the app says so; the user tops up at a checkout, where they
can authenticate. It is behind its own switch (`AUTO_RECHARGE`) and can
ship after the rest.

**Do Billing Meters add anything?** Not in v1. They could later mirror our
usage into Stripe for reporting, but invoices for packs and plans already
exist in Stripe, and a second copy of usage is a second thing to reconcile.

### 4.3 Saving a card, and the sign-up credit

```
signed in ──accept terms──► no card ──Checkout (setup)──► card saved ──► active
                              ▲                                            │
                              └──────── last payment method removed ◄──────┘
```

The verified `checkout.session.completed` (and every payment-method event
after it) leads to one function that lists the customer's payment methods
at Stripe and writes whether there is one. The first time there is, the
sign-up credit is decided, once:

| The card | Outcome |
|---|---|
| a card not seen before | $5 credit, valid 90 days |
| a card whose fingerprint already earned a credit, on any account | saved, no credit ("card already used") |
| prepaid | saved, no credit |
| saved through Apple Pay or Google Pay | saved, no credit (its fingerprint is the device's, so it would defeat the rule above) |

"One per person" is therefore one per card number and one per account
(and so per Google or GitHub identity). Someone with several cards and
several identities can collect several credits; each costs them a real
card and us about $1.40 to $1.90 (section 7), and `SIGNUP_CREDIT=off` stops
it at once.

Refusing prepaid cards **as payment methods** is not proposed: Stripe's
rule for it needs a paid Radar plan, and a prepaid card that pays is a
customer. Turning on "Use Radar on payment methods saved for future use" is
on the product owner's list.

### 4.4 Removing the last card

The account goes back to `no_card`: its sessions are put to sleep at the
next sweep (within 30 seconds; the same snapshot-then-suspend as running
out, with the reason "no payment method"), and create and wake answer 402
until a card is back. Its credit is **kept**. It cannot be used to run
anything meanwhile; the disks that still exist go on drawing on it, because
they go on costing (11.9). Adding any card makes sessions wakeable again
and gives no second sign-up credit.

The trigger is the `payment_method.detached` webhook, found to its account
through the list of payment method IDs we keep, and re-read at Stripe; and,
in case the webhook is missed, a reconcile of every account's payment
methods every 15 minutes. A subscriber cannot remove their last card in the
portal (Stripe's rule), so in practice this is the pay-as-you-go case.

An expired card is still a card (11.10).

### 4.5 Exactly once, in any order

Every effect of a Stripe event is "make sure this Grant exists" or "write
down what Stripe says now". The Grant's name is a hash of its cause, and
the object is re-read from Stripe, not taken from the event. A duplicate, a
late event, or two events in the wrong order all end in the same state.
This replaces the list of processed event IDs Stripe suggests, which would
be state to keep.

The same functions run from the webhook, from the page the user returns to
after Checkout, and from the timed reconcile.

### 4.6 Plan changes, cancelling, failed payments

| Event | Money (Stripe) | Credit (ours) |
|---|---|---|
| Subscribe | charged at once | the plan's credit for the period, when the invoice is paid |
| Renewal | charged on the period's date | a new grant; **what was left of the old one expires: no rollover** |
| Upgrade (portal) | charged at once for the new plan, less the unused time of the old; a new period starts now | the new plan's full credit; the old plan's remaining credit is revoked (it was refunded as time) |
| Downgrade (portal) | takes effect at the period's end | nothing until then |
| Cancel (portal) | no more charges; the plan runs to the period's end | the current grant runs to its expiry; afterwards the account is pay as you go: its sign-up and purchased credit remain, its limits become pay as you go's |
| Renewal payment fails | Stripe retries on its schedule; status `past_due` | no new grant; the plan's limits are kept while Stripe retries (the grace); the user sees "payment failed"; they run on whatever credit they have |
| Stripe gives up (Dashboard setting: cancel after the last retry) | subscription `canceled` | pay as you go, as after cancelling. Sessions beyond pay as you go's limit are kept; no new one can be made until some are deleted. |
| Refund (by the product owner, in the Dashboard) | returned | the grant is revoked; what was used is not clawed back |
| Dispute | withdrawn | the grant is revoked, the account blocked until the dispute is won or an admin unblocks |

**Order of use**: earliest expiry first. In practice: the plan's credit
(ends with the month), then the sign-up credit (90 days), then purchased
credit (12 months), so that what a user paid for separately is the last to
go.

That the portal's upgrade behaves as described is the first thing to
confirm in the sandbox with a test clock; the fallback is in the contract.

### 4.7 Objects as code

Recommendation: **a small idempotent command in the repository**
(`backend/cmd/stripe-setup`), run by a manual workflow, test mode first.

| | Setup command | Terraform (`stripe/stripe`) |
|---|---|---|
| Fit | six prices, four products, one portal configuration, found by fixed product IDs and lookup keys | general |
| State | none: Stripe is the state | a state file to keep, which would hold whatever secret attributes the provider exposes |
| Maturity | uses `stripe-go`, which the backend needs anyway | 0.3.0, generated, portal configuration resource not in Stripe's own list |
| Review | `--apply=false` prints the plan | `plan` |

The webhook endpoint is **not** made by code: its signing secret is shown
in the Dashboard and has to go from there into a GitHub secret by the
product owner's hand; a script that created it would have the secret in
its output.

**The plan table is data.** What a plan gives, the two rates, the limits
and the sign-up credit are in one file, a ConfigMap read by the backend and
the operator and re-read when it changes: no deploy of code. Only a change
of a **price** touches Stripe (a new Price with a new lookup key).

### 4.8 Tax

Not collected at the start: `automatic_tax` is off. Stripe's documents do
not require it from the first sale, and Stripe Tax only collects where the
business is registered. This is a business decision with legal weight;
open question 11.17.

### 4.9 Test and live

One switch, `STRIPE_MODE`, chooses which pair of secrets the deploy
workflow puts in the cluster; the backend refuses to start with a live key
in test mode or the reverse, and refuses webhook events of the other mode.
Prices are found by lookup key, so nothing else differs.

---

## 5. Enforcement

The states, the decision table, the answers and the stop sequence are
[`enforcement.md`](../contracts/billing/enforcement.md).

### 5.1 Where

| Door | Today | Check |
|---|---|---|
| Create a session (UI, API token, Terraform) | `api.create`, before anything is created or claimed from the warm pool | terms, card, credit, sessions limit, awake limit, capacity |
| Resume a stopped session | `api.patch` | terms, card, credit, awake limit, capacity |
| Wake on use: an MCP call, the screen, an upload to a sleeping session | `proxy.Waker.EnsureAwake` | the same as resume |
| A new call to a session that is being stopped | the proxy | refused with the reason |
| Already running | nothing per request | the sweep stops it |

The account charged is the session owner's. A token acts as its owner, so
token-driven and Terraform-driven use is the owner's use and needs the
owner's card. Session policies are not involved. Reading, stopping and
deleting are never refused. API tokens and policies cost nothing and are
not gated.

### 5.1a How the backend reads balance and card state

Create and wake must answer in milliseconds, and the ledger's writer is a
separate process. The backend keeps an **informer** (a watch with a local
cache) on Accounts: a decision reads `spec.paymentMethod` and `status` from
memory, with no call to the API server, to the operator or to Stripe. The
cache is at most as old as the operator's last tick (60 s) for the balance,
and a watch event behind (well under a second) for the card, which the
backend itself wrote. A balance that is a minute old can only err by a
minute of use, which the grace already allows for.

That read is the `Accounts` and `Ledger` interfaces of
[`testing.md`](../contracts/billing/testing.md): the informer-backed
implementation in production, an in-memory fake in tests. The decision
function and the sweep see nothing else, so the card-gate scenario runs
with no Kubernetes and no Stripe.

### 5.2 The gate

A session is the only billable resource. Without a saved card it cannot be
created (402 `payment_method_required`, with a link) or woken. In the UI a
new user is taken to a first-run page that asks for the card and says what
they get. Terraform and scripts get the same 402; the provider prints the
message and the link.

### 5.3 Running out, precisely

| When | What happens |
|---|---|
| Balance at 20 % of the plan's monthly credit, or $1 | a warning banner with "Add credit"; nothing else. With auto-recharge on, a pack is bought instead. |
| Balance reaches zero (`exhaustedAt`) | banner: sessions sleep in 5 minutes. Create and wake are refused, `out_of_credit`. Running sessions carry on. |
| + 5 minutes (the grace) | each running session is marked draining: **new** calls are refused, viewers and event streams are closed, **calls in flight finish**. |
| when its calls have finished, or after 10 minutes at most | the session is **snapshotted, then suspended**, marked `stopped-by: credit`. |
| Credit arrives at any point before the sleep | the stop is called off; nothing was interrupted. |
| Credit arrives after the sleep | sessions become wakeable. They are not woken: the next use, or Resume, wakes them. |

The short grace is kept and does not contradict "calls in flight
finish": the grace comes first and exists only so that a top-up already in
progress interrupts nothing; the finish-then-sleep rule is what happens
when it ends. Setting `BILLING_GRACE` to 0 starts the drain at zero.

The 10 minute bound is the proxy's own limit on an MCP call
(`mcpResponseTimeout`); a call cannot outlive it today either. The grace
and the drain are free (overdraft): about 15 minutes at most per
exhaustion.

Nothing is deleted when credit runs out.

### 5.4 What a client sees

HTTP **402** with a JSON body naming the reason and the billing URL; for
MCP, the same status with a JSON-RPC error whose message says the session
is asleep because its owner is out of credit (or has no payment method),
that it is kept, and where to fix it. No `Retry-After`, and not a 5xx or a
429: it must not look transient to a client that retries. What Claude's
clients show for it is **UNVERIFIED** and is the first thing the backend
track records; the fallback (HTTP 200 with the JSON-RPC error) is in the
contract.

### 5.5 Disks at zero

At zero the disk charge has nothing to draw on. The rule: **the balance
never goes negative and nothing is owed**; the charge is counted as
overdraft. So that this is not free storage for ever, after **14 days
continuously at zero** the account's sessions are deleted, with notices in
the app at zero (the date), at 7 days and the day before. Any credit
arriving clears the clock.

The deletion is behind its own switch, **off**, and should stay off until
the product can send email (11.8): an in-app notice reaches only someone
who opens the app, and deleting a user's data unannounced is the kind of
mistake that cannot be taken back. Until then a zero-balance disk costs us
$0.55 a month.

### 5.6 When something is down

- **Stripe unreachable**: nobody can save a card, buy or open the portal
  ("nothing was charged, try again"). Enforcement never calls Stripe, so
  nothing else changes. A card removed during the outage is noticed when
  Stripe is back.
- **Ledger stale** (the operator has not written for 10 minutes): an
  account that had credit when last counted starts and runs as usual and is
  not charged for the time; an account that had none stays refused; nothing
  is stopped. The card requirement still holds (it is the backend's own
  knowledge).
- **Kubernetes API unreachable**: nothing in the product works; no special
  case.

### 5.7 Admins and exempt accounts

Addresses in `BILLING_EXEMPT_EMAILS` (by default the admins) need no card,
are metered so their usage is visible, and are never refused or stopped. An
admin can also give credit (`source: admin` Grant) or block an account with
`kubectl`. There is no admin UI in v1.

### 5.8 Capacity

The project's quota (12 CPUs, 250 GB SSD) allows two session nodes: about
eleven awake sessions beside the seven warm pods. The table ends with
admission control: at most `MAX_AWAKE_SESSIONS` (10) awake in the cluster;
beyond that the answer is "every desktop is in use right now, try again in
a few minutes". A queue is not in v1.

---

## 6. UI

States and copy: [`ui-states.md`](../contracts/billing/ui-states.md).
Cloudscape components as the app already uses them.

### 6.1 First run: the gate

```
┌ Computer Use ───────────────────────────────────────────── me@x.com ┐
│  Welcome                                                             │
│ ┌─ 1. Terms ──────────────────────────────────────────── ✓ accepted ┐│
│ └───────────────────────────────────────────────────────────────────┘│
│ ┌─ 2. Add a payment method ─────────────────────────────────────────┐│
│ │ A card is required before you create a desktop. Nothing is        ││
│ │ charged now; your bank may show a temporary authorisation.        ││
│ │                                                                   ││
│ │ When your card is saved you get $5 of credit: about 25 hours      ││
│ │ awake, valid for 90 days. One per person and per card.            ││
│ │                                                                   ││
│ │ $0.20 for each hour a session is awake · $1.40 a month for each   ││
│ │ session you keep                          [See plans]             ││
│ │                                                    [Add a card]   ││
│ └───────────────────────────────────────────────────────────────────┘│
└──────────────────────────────────────────────────────────────────────┘
```

### 6.2 Billing (`/billing`)

```
┌ Computer Use ───────────────────────────────── $7.40 ▾ ── me@x.com ┐
│ Sessions         Billing                    [Add credit] [Manage billing] │
│ Billing      ┌─ Credit ───────────────────────────────────────────────┐ │
│ API tokens   │  $7.40                                                 │ │
│              │  Using now  $0.20 an hour · about 37 hours left        │ │
│              │  Plan credit      $6.10   until 5 Nov                  │ │
│              │  Sign-up credit   $1.30   until 30 Dec                 │ │
│              │  Purchased credit $0.00                                │ │
│              │  Plan credit used this period                          │ │
│              │  ███████████░░░░░░░░░░░░░░░░░  $3.90 of $10.00         │ │
│              │  $0.20 for each hour awake, $1.40 a month for each     │ │
│              │  session you keep. Stop a session to stop the hourly   │ │
│              │  charge; delete it to stop the disk charge.            │ │
│              └────────────────────────────────────────────────────────┘ │
│              ┌─ Plan ─────────────────────────────────────────────────┐ │
│              │ ┌ Pay as you go ┐ ┌ Starter ● ───┐ ┌ Pro ───────────┐  │ │
│              │ │ no monthly fee│ │ $5 a month   │ │ $20 a month    │  │ │
│              │ │ credit at     │ │ $10 credit   │ │ $44 credit     │  │ │
│              │ │ face value    │ │ ~40 h awake  │ │ ~200 h awake   │  │ │
│              │ │ 3 sessions    │ │ 3 sessions   │ │ 10 sessions    │  │ │
│              │ │ 2 at once     │ │ 2 at once    │ │ 4 at once      │  │ │
│              │ │               │ │ Current plan │ │ [Change plan]  │  │ │
│              │ └───────────────┘ └──────────────┘ └────────────────┘  │ │
│              │ Renews 5 November 2026                                 │ │
│              └────────────────────────────────────────────────────────┘ │
│              ┌─ Payment method ───────────────────────────────────────┐ │
│              │ Visa ···· 4242, expires 08/28          [Manage cards]  │ │
│              │ Auto-recharge  ( off )                                 │ │
│              │   when on: buy [$20 ▾] when my balance falls below     │ │
│              │   [$2], at most [$50] a month                          │ │
│              └────────────────────────────────────────────────────────┘ │
│              ┌─ Usage ──────────────────── Period [5 Oct – 5 Nov ▾] ──┐ │
│              │ $ ▁▃▂▅▇▂▁▁▄▆▃▂ ...        (by day: awake, disk)        │ │
│              │ Session        Awake      Awake $   Disk $   Total     │ │
│              │ brave-otter    12 h 05    $2.41     $0.47    $2.88     │ │
│              │ calm-heron      2 h 45    $0.55     $0.47    $1.02     │ │
│              └────────────────────────────────────────────────────────┘ │
└─────────────────────────────────────────────────────────────────────────┘
```

### 6.3 Adding credit

```
┌ Add credit ───────────────────────────────────────────┐
│ ( ) $5      (•) $20      ( ) $50                      │
│ Credit is used after your plan's credit and is valid  │
│ for 12 months.                                        │
│ ⓘ Test mode: no real money is taken.                  │
│                      [Cancel] [Continue to payment]   │
└───────────────────────────────────────────────────────┘
```

The browser goes to Stripe's page and returns to `/billing?checkout=...`,
which shows "Confirming" and then "$20 of credit added".

### 6.4 Banners

```
✓  Card saved. $5 of credit added, valid until 30 December. [Create your first desktop]
⚠  $0.80 of credit left, about 3 hours at your current use.   [Add credit] [See plans] ✕
⛔ You are out of credit. Running sessions go to sleep at 14:32; work in progress
   finishes first and nothing is lost.                                   [Add credit]
⛔ You are out of credit. Your sessions are asleep and kept. They will be deleted
   on 16 October unless you add credit.                      [Add credit] [See plans]
⛔ You have no payment method. Your sessions are asleep and kept. Add a card to
   wake them or create new ones.                                         [Add a card]
⛔ Your last payment failed. Update your card to keep your plan.        [Update card]
⛔ Your bank asked for confirmation, so auto-recharge is off. Add credit now to
   confirm with your bank.                                               [Add credit]
```

### 6.5 Blocked create and blocked wake

```
Create session
┌ ⛔ Add a payment method to create a session.            [Add a card] ┐
Name [ brave-otter        ]
This session will use $0.20 an hour while awake and $1.40 a month while it exists.
                                         [Cancel] [Create session ░░]

brave-otter                                            ● Asleep: out of credit
┌──────────────────────────────────────────────────────────────────┐
│        This session is kept as it was.                           │
│        It can wake once you have credit.                         │
│                  [Add credit]  [See plans]                       │
└──────────────────────────────────────────────────────────────────┘
```

### 6.6 The public site

A pricing page: the four options as a table, the two rates in words, the
packs, the sign-up credit and the card requirement, and links to the terms,
privacy, acceptable-use and refund pages
([`legal-pages.md`](../contracts/billing/legal-pages.md)).

---

## 7. Pricing arithmetic

### 7.1 What things cost us

From `docs/warm-pool.md` and `docs/infrastructure.md`: a Spot session node
is about $0.13 an hour all in (machine, boot disk, NAT; the product owner's
figure is $0.125), an on-demand one $0.21. Nine session pods fit a node
today (memory decides). XFCE and a terminal will make a session heavier,
and nodes are never perfectly full: one lingers about ten minutes after its
last session sleeps, and the first session on a node pays for all of it.

Node cost per awake session-hour, Spot:

| Sessions that fit a node | Node full | Node half full on average | + traffic ($0.01, a guess to be measured) |
|---|---|---|---|
| 9 (today) | $0.015 | $0.029 | $0.039 |
| **6 (with a desktop, assumed)** | $0.022 | $0.044 | **$0.054** |
| 4 (pessimistic) | $0.033 | $0.066 | $0.076 |

- **Planning cost: $0.055 per awake hour.** Pessimistic: **$0.075**.
- A kept session: 5 GB at about $0.10 per GB-month and a snapshot,
  **$0.55 a month**.
- Fixed: about **$178 a month** (system node, load balancer, the warm
  pool's node and disks).
- Stripe: 2.9 % + $0.30 a charge, plus 0.7 % on subscriptions.

### 7.2 The rates

| | List rate | Our cost | Cost per dollar of credit spent on it |
|---|---|---|---|
| Awake | **$0.20 an hour** | $0.055 (pessimistic $0.075) | $0.28 ($0.38) |
| Disk | **$0.28 per GB-month** ($1.40 a month a session) | $0.55 a month a session | $0.39 |

So a dollar of credit, however it is spent, costs us at most about $0.39.
Every option below brings in at least $0.40 per dollar of credit it gives.
That is the whole constraint, and it is why the disk rate is 2.5 times its
cost: on the largest plan a dollar of credit is bought for 42 cents.

### 7.3 The four options

| | Pay as you go | Starter | Pro | Scale |
|---|---|---|---|---|
| Price | none; packs of $5, $20, $50 | $5 a month | $20 a month | $100 a month |
| Credit | face value, valid 12 months | $10 a month | $44 a month | $240 a month |
| Credit per dollar paid | 1.0 | 2.0 | 2.2 | 2.4 |
| **What an awake hour costs the customer** | **$0.20** | **$0.100** | **$0.091** | **$0.083** |
| A kept session costs the customer, a month | $1.40 | $0.70 | $0.64 | $0.58 |
| Awake hours a month, with one / a typical number of sessions kept | 25 per $5 | 43 / about 40 | 213 / about 200 (3 kept) | 1193 / about 1130 (10 kept) |
| Sessions kept | 3 | 3 | 10 | 25 |
| Awake at once | 2 | 2 | 4 | 8 |
| Unused credit | 12 months | does not roll over | does not roll over | does not roll over |

The value per hour improves with every tier, pay as you go is the worst
rate (twice Starter's), and the $5 plan gives the 40 hours it was sized
at, now with a session's disk paid from the same credit.

What could differ by tier and does **not** in v1, so that the first
version has one kind of session: the idle time before sleep (15 minutes),
the disk size (5 GB), CPU and memory limits, API tokens, Terraform and
policies (all tiers), support (best effort by email; Scale first in the
queue). Each is a candidate for a later tier difference; a longer idle time
and a bigger disk are the natural first two, and both need per-session
settings the backend does not have today.

### 7.4 Margins

"Worst customer" spends every cent of credit and keeps the most sessions
allowed. Planning cost / pessimistic cost.

| | Paid | Stripe | Net | Credit | Worst customer costs us | Margin |
|---|---|---|---|---|---|---|
| Starter | $5 | $0.48 | $4.52 | $10 | 3 kept ($1.65) + 29 h ($1.60 / $2.18) = $3.25 / $3.83 | **+$1.27 / +$0.69** |
| Pro | $20 | $1.02 | $18.98 | $44 | 10 kept ($5.50) + 150 h ($8.25 / $11.25) = $13.75 / $16.75 | **+$5.23 / +$2.23** |
| Scale | $100 | $3.90 | $96.10 | $240 | 25 kept ($13.75) + 1025 h ($56.38 / $76.88) = $70.13 / $90.63 | **+$25.97 / +$5.47** |
| Pack $5 | $5 | $0.45 | $4.55 | $5 | 25 h = $1.38 / $1.88 | +$3.17 / +$2.67 |
| Pack $20 | $20 | $0.88 | $19.12 | $20 | 100 h = $5.50 / $7.50 | +$13.62 / +$11.62 |
| Pack $50 | $50 | $1.75 | $48.25 | $50 | 250 h = $13.75 / $18.75 | +$34.50 / +$29.50 |
| Sign-up credit | $0 | | $0 | $5 | 25 h = $1.38 / $1.88 | **-$1.38 / -$1.88** |

Sensitivity to how many sessions fit a node, for the awake hour (half-full
nodes, traffic included), against what each tier pays per hour after
Stripe's fees:

| Fit per node | Cost per awake hour | Starter ($0.090 net) | Pro ($0.086) | Scale ($0.080) | Pay as you go ($0.18) |
|---|---|---|---|---|---|
| 9 (today) | $0.039 | above | above | above | above |
| 6 (assumed with XFCE) | $0.054 | above | above | above | above |
| 4 | $0.076 | above | above | above, by 5 % | above |
| 3 | $0.098 | **below** | **below** | **below** | above |

Every tier stays above cost down to four sessions a node. At three, the
subscriptions lose money on awake time and the rate has to rise; because
the rates are data, that is a change to one number.

A typical customer uses about half their credit (an assumption):

| Typical | Uses | Costs us | Leaves |
|---|---|---|---|
| Starter | 18 h, 1 session kept ($5 of credit) | $1.54 | +$2.98 |
| Pro | 80 h, 4 kept ($21.60) | $6.60 | +$12.38 |
| Scale | 500 h, 10 kept ($114) | $33.00 | +$63.10 |
| Pay as you go | a $20 pack every two months: 40 h and 1 kept a month | $2.75 | +$6.81 a month |

### 7.5 Break-even on the fixed $178 a month

| Mix of typical customers | Contribution |
|---|---|
| 60 Starter | $179 |
| 30 Starter, 5 Pro, 5 pay as you go | $185 |
| 10 Starter, 10 Pro, 1 Scale | $217 |
| 15 Pro | $186 |
| 3 Scale | $189 |

**The sign-up credit is the largest variable.** Each new user who spends
all of it costs $1.40 to $1.90. A hundred such sign-ups a month cost as
much as the whole fixed bill. It is bounded by its 90 day expiry, by one
per card, and by `SIGNUP_CREDIT=off`. The amount is the product owner's
decision; the arithmetic says to watch it (11.3).

**Scale cannot be sold yet.** $240 of credit is about 1.5 sessions awake
around the clock and up to 8 at once, in a cluster with eleven places. It
is in the catalogue with `enabled: false` until the CPU quota is raised.

---

## 8. Open sign-up

Its own stage, last, switched separately (`OPEN_SIGNUP` and one Pomerium
policy change in one pull request), with the prerequisites of 8.5.

### 8.1 What changes

| | Today | Open |
|---|---|---|
| Pomerium policy on `app` and the session MCP routes | two email addresses | `authenticated_user: true` |
| API tokens (`ALLOWED_EMAILS`) | the same two addresses | any account that exists and is not blocked |
| Admins | `ADMIN_EMAILS` | unchanged |
| Google OAuth app | "Testing" | "In production", brand verified |
| GitHub OAuth app | no restriction | unchanged |
| First visit | sessions list | terms, then the card, then the list |

### 8.2 Identity

Users are identified by email address across both providers: sessions are
owned by it, and so are Accounts. Consequences once anyone can sign in:

- **An unverified email must not be an identity.** Dex's GitHub connector
  only yields a verified primary address. Dex's Google connector passes
  `email_verified` through without refusing. Before sign-up opens it must
  be established, by test, that a Google account with an unverified address
  cannot sign in as that address; if it can, the fix is a check of the
  claim (in Dex's configuration if it has one, else in the backend from
  Pomerium's assertion; whether the assertion carries the claim is
  **UNVERIFIED**). With cards and credit attached to accounts this matters
  more than before. Prerequisite P4.
- The same address through Google and through GitHub is one account. That
  is the intent, and it rests on both addresses being verified.

### 8.3 Abuse, with a card on file

**A saved card is now the main control.** Nobody gets a browser, a
terminal or our outbound address without one, and each card earns the
credit once. What a card does not stop, and what still needs a limit:

| Threat | Control | In |
|---|---|---|
| Collecting the sign-up credit repeatedly | one per card fingerprint, for ever; none for prepaid or wallet cards; one per account; expiry 90 days; `SIGNUP_CREDIT=off`; the export, so the record survives the cluster | stage 3 |
| Stolen cards used to pass the gate and burn $5 | costs us at most about $1.90 per card; turn on Radar for saved payment methods; no charge is made, so no dispute follows from the save itself | stage 3, the product owner's Radar setting |
| **Card testing through the setup Checkout** (validating stolen card numbers for free) | every setup Checkout needs a signed-in account; 5 setup Checkouts per account a day; at most 5 cards per account; sign-ups limited per IP address and per day; Stripe's own controls on Checkout. This is the new risk the gate introduces, and Stripe names card saves as testers' preferred method. | stage 3 and 5 |
| Paying with a stolen card, using the credit, chargeback | prepaid means the loss is the credit used plus the $15 dispute fee; the account is blocked and the grant revoked on dispute; Radar on payments | stage 2 |
| Auto-recharge on a stolen card | off by default, needs the account's owner to turn it on, capped monthly; a failure turns it off | with auto-recharge |
| Filling the cluster | the cluster-wide awake cap; per-plan awake limits | stage 3 |
| Crypto-mining | a session's two containers are limited to 2 CPUs together and guaranteed a fifth of one; at $0.20 an hour it costs more than it could mine | exists |
| Spam | Google Cloud blocks outbound port 25 (**UNVERIFIED** here; to confirm); ports 465 and 587 to be blocked for all sessions in the session NetworkPolicy | stage 5 |
| Attacks on the cluster or the cloud metadata | already blocked: private ranges and the metadata address are excluded from session egress; gVisor | exists |
| Sessions as a proxy, scraping or attack source from our address | no inbound ports; acceptable-use policy; an abuse address; the ledger says whose session was awake when, and there is a card behind it; blocking an account sleeps its sessions at once | stage 5 |
| A terminal | every row above is easier with one, but every user of it now has a card on file and pays by the hour: the argument for withholding it from anyone is gone (11.16) | decision |
| A forged webhook | signature over the raw body, 5 minute tolerance, mode check; every handler re-reads Stripe before acting | stage 2 |
| Wake storms to dodge the meter (runs under a minute are free of the awake charge) | 30 starts per account per hour | stage 3 |

Not proposed: CAPTCHA, phone verification, GitHub account age, refusing
prepaid cards outright.

### 8.4 Pages and account deletion

- Terms, privacy, acceptable use, refunds and cancellation, pricing,
  contact: needed by Stripe to activate the account and by Google to
  verify the brand, and now also because users are asked for a card on
  their first visit. Drafts of what each must say are in
  [`legal-pages.md`](../contracts/billing/legal-pages.md), for the product
  owner to review. They are not legal advice.
- A user can delete their account in the app: sessions, disks, snapshots
  and tokens go, the subscription is cancelled, the cards are detached, and
  a stub stays so that neither the address nor the card earns a second
  sign-up credit.

### 8.5 Before sign-up opens, and what can follow

**Must exist first:**

| | Prerequisite |
|---|---|
| P1 | `BILLING=enforce` has run in production for at least a week with the allow-listed users, with no metering discrepancy, and the card-gate scenario of `testing.md` passes in CI |
| P2 | **Live payments are on** (a card saved in test mode is not a card), with Radar on saved payment methods |
| P3 | Terms, privacy, acceptable-use and refund pages are published and the gate records acceptance |
| P4 | The email-verification question of 8.2 is answered by test, and fixed if needed |
| P5 | The Google OAuth app is "In production", and a Google account that is not a test user can sign in |
| P6 | Account deletion works |
| P7 | Blocking an account works end to end (sessions asleep, sign-in refused), and the abuse address is read |
| P8 | Ports 465 and 587 are blocked for sessions; port 25 confirmed blocked |
| P9 | The daily export of the ledger runs, and a restore was rehearsed (the sign-up credit's record depends on it) |
| P10 | The awake cap, the sign-up limits and the setup-checkout limits are on and tested |

**Can follow:** auto-recharge; brand verification's logo; a waiting queue;
email; deletion of disks at zero; an admin page.

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
- **Cards.** No card number ever reaches us: Checkout and the portal are
  Stripe's pages. We keep payment method IDs, the fingerprint (in a Grant's
  key), brand, last four digits and expiry.
- **Off-session charges** happen only for an account that turned
  auto-recharge on and agreed to its text, are capped monthly, are
  idempotent by a key written before the call, and stop at the first
  failure.
- **Who can write the ledger.** The backend cannot write
  `accounts/status`; the operator cannot write an Account's `spec`, creates
  no Grants, and has no network path but the API server. No API route
  writes a Grant from user input, and session pods cannot reach the API
  server.
- **The backend gains the right to create Grants**, which are money. A
  compromised backend could mint credit. It can already delete every
  session; the exposure is accepted, and an audit job that flags Grants
  with no Stripe object behind them is a small later addition.
- **Checkout redirect URLs** are built from `PUBLIC_URL`, never from the
  request.
- **Fail directions** are stated: metering fails free, enforcement fails
  open for accounts that had credit and closed for the rest, payment fails
  with nothing charged, a balance cannot go negative.
- **Privacy.** The ledger holds email addresses and which session was
  awake on which day. Sign-up IP addresses are kept only as salted hashes
  for 35 days.
- **Test mode in production.** With `STRIPE_MODE=test` the UI says so on
  every purchase surface, a test card passes the gate and a test payment
  grants real credit. While only the two allow-listed users exist that is
  the point; it is why live payments are a prerequisite of open sign-up.

---

## 10. Not in v1

- Post-paid usage, Stripe Billing Meters and Stripe's billing credits.
- A free tier with no card.
- Charging for traffic or the warm pool; per-session rates (bigger
  machines, bigger disks, longer idle times).
- Annual plans, trials, coupons, teams or shared accounts, currencies other
  than USD.
- Stripe Tax.
- Emails of any kind from us (low balance, deletion warnings): the app has
  no mail sender. Stripe sends receipts and failed-payment emails itself in
  live mode.
- Deleting disks at zero (designed, switched off until there is email).
- An admin UI: `kubectl` and the Stripe Dashboard.
- A waiting queue when the cluster is full.
- Proration of credit on plan change beyond "an upgrade starts a new
  period".
- Refunds from the app (the Dashboard does them; the webhook follows).
- Checking that a saved card has not expired.

Auto-recharge is designed and contracted, behind its own switch, and may
ship with v1 or just after (11.7).

---

## 11. Open questions for the product owner

Each with the default this design assumes. 11.1 to 11.4 are the tier
definitions, to confirm or edit; they are data
([`catalogue.yaml`](../contracts/billing/catalogue.yaml)).

| # | Question | Recommended default |
|---|---|---|
| 11.1 | The rates? | $0.20 for each awake hour; $0.28 per GB-month of disk ($1.40 a month for a 5 GB session). |
| 11.2 | What do the three subscriptions give? | Starter $5: $10 of credit (about 40 awake hours), 3 sessions, 2 awake. Pro $20: $44 (about 200 hours), 10 sessions, 4 awake. Scale $100: $240 (about 1100 hours), 25 sessions, 8 awake, **not sold until the CPU quota is raised**. Credit does not roll over. |
| 11.3 | Pay as you go and the sign-up credit? | Packs of $5, $20, $50 at face value, valid 12 months, minimum $5; limits 3 sessions, 2 awake. Sign-up credit $5, valid 90 days, one per card and per account, none for prepaid or wallet cards. |
| 11.4 | Anything else different by tier (idle time, disk size, CPU, tokens and policies, support)? | No, in v1: one kind of session on every tier. |
| 11.5 | Is the idle time before sleep charged? | Yes; the UI says to stop a session to stop the clock. |
| 11.6 | Grace and drain when credit runs out? | 5 minutes of grace, then calls in flight finish within at most 10 minutes (the proxy's limit), then snapshot and sleep. Free. |
| 11.7 | Auto-recharge in v1? | Yes, as the last piece: off by default per account, default pack $20 below $2, monthly cap $50 (the user's to change, at most $500). |
| 11.8 | Disks at zero: delete, and when? | Delete after 14 days at zero, with in-app notices at 0, 7 and 13 days; **switched off until the product can send email**. Building a mail sender is the next thing to decide. |
| 11.9 | With no card, do kept disks go on drawing on the remaining credit? | Yes: the disks still exist and still cost. The credit is otherwise untouched and usable again when a card is back. |
| 11.10 | Does an expired card count as a card? | Yes in v1 (it is attached at Stripe; a charge on it fails and is handled as a failed payment). |
| 11.11 | Ledger stale: who may start sessions? | Accounts that had credit when last counted (uncharged meanwhile); nobody else. |
| 11.12 | Plan changes? | Upgrade: charged at once, new period, the new plan's full credit, the old plan's remaining credit dropped. Downgrade and cancel: at the period's end. |
| 11.13 | Failed renewal? | Stripe retries on its schedule; plan limits kept meanwhile, no new credit; the Dashboard is set to cancel after the last retry, and the account becomes pay as you go. |
| 11.14 | Refund policy? | No refunds for part periods or unused credit except case by case; stated on the site. |
| 11.15 | Does the ledger stay in custom resources? | Yes, with the daily export; revisit at a few thousand accounts. |
| 11.16 | Does everyone get the terminal? | Yes: every user has a card on file and pays by the hour. |
| 11.17 | Collect sales tax or VAT at the start? | No (`automatic_tax` off); ask an accountant before live mode; consider US-only at first. |
| 11.18 | A general Stripe sandbox, or the test-mode sandbox? | A general sandbox named `computeruse-dev` (Stripe's advice for new integrations). |
| 11.19 | Is prepaid credit with an expiry acceptable where the business operates, and is the legal entity, address and support email decided? | For the product owner's adviser; needed before live mode. |
| 11.20 | Are admins billed? | No: metered, exempt, no card needed. |
| 11.21 | Does sign-up open before live payments? | No: a card saved in test mode is not a card. |

---

## 12. What the product owner does by hand

In order. No key is pasted anywhere but a GitHub secret field.

**For test mode (stages 2 and 3):**

1. Create a Stripe account at https://dashboard.stripe.com/register. Do
   not activate payments yet.
2. In the Dashboard's sandbox menu, create a sandbox named
   `computeruse-dev` and switch to it.
3. Developers, API keys, "Create restricted key", named `setup`:
   write access to Products, Prices and Customer portal; read on the rest
   it asks for. Put it in the GitHub repository secret
   `STRIPE_TEST_SETUP_KEY`.
4. A second restricted key named `backend`: write on Checkout Sessions,
   Customers, Customer portal, PaymentIntents, PaymentMethods,
   Subscriptions; read on Prices, Products, SetupIntents, Invoices,
   Charges, Refunds, Disputes. Secret `STRIPE_TEST_API_KEY`. (The backend
   track will say if a permission is missing; the key is then edited in the
   Dashboard, not replaced.)
5. Run the workflow "stripe setup" with `mode: test`, `apply: false`, read
   the plan, then again with `apply: true`.
6. Developers, Webhooks, "Add destination": URL
   `https://api.computeruse.site/stripe/webhook`, the events listed in
   `docs/contracts/billing/stripe.md`, the API version the backend track
   names. Reveal the signing secret and put it in the secret
   `STRIPE_TEST_WEBHOOK_SECRET`.
7. Set the repository **variable** `STRIPE_MODE` to `test`.
8. Merge the pull request that sets `BILLING=meter`, and run "deploy".
9. In the app: add the card `4242 4242 4242 4242`, see $5 of credit
   arrive; buy a pack; subscribe; remove the card in "Manage billing" and
   see nothing change yet (shadow mode).
10. Merge the pull request that sets `BILLING=enforce`, deploy, and repeat:
    without a card nothing can be created; removing the card puts sessions
    to sleep.

**For live payments (stage 4):**

11. Review, rewrite and approve the terms, privacy, acceptable-use and
    refund pages; supply the legal name, address and support and abuse
    email addresses.
12. Activate the Stripe account: business details, bank account, the
    website `https://computeruse.site` (which by then has pricing, contact,
    refund and privacy pages).
13. In live mode: Settings, Billing, "Manage failed payments": cancel the
    subscription after the last retry. Settings, Customer emails: receipts
    and failed-payment emails on. Radar settings: "Use Radar on payment
    methods saved for future use" on.
14. Repeat steps 3 to 6 in live mode with the secrets
    `STRIPE_LIVE_SETUP_KEY`, `STRIPE_LIVE_API_KEY`,
    `STRIPE_LIVE_WEBHOOK_SECRET` (the setup workflow with `mode: live`
    asks for a typed confirmation).
15. Set the variable `STRIPE_MODE` to `live` and deploy. Every account's
    test-mode card and credit are gone with the mode: the two existing
    users add a real card.
16. Buy the smallest pack with a real card, see the credit, refund it in
    the Dashboard, see it go.

**For open sign-up (stage 5):**

17. Google Cloud console, Google Auth Platform, Audience: first try
    signing in to the app with a Google account that is not a test user
    and tell the team what happened; then "Publish app".
18. Branding: app name "Computer Use", support email, homepage
    `https://computeruse.site`, privacy policy and terms links, authorised
    domain `computeruse.site`; verify the domain in Search Console; submit
    for brand verification.
19. Ask for a higher CPU quota in the Google Cloud project if more than
    eleven concurrent sessions are wanted (and before Scale is enabled).
20. Merge the open sign-up pull request and deploy.

---

## 13. Alternatives rejected

| Alternative | Why not |
|---|---|
| Stripe Billing Meters with a metered price for pay as you go | bills in arrears, no stop at a limit, asynchronous; non-payment and abuse risk with open sign-up (1.6, 4.2) |
| Stripe billing credits as the balance | apply only to metered subscription items, reconciled only at invoice time (1.6) |
| Hours as the unit | cannot express a $5 credit or a disk charge without a second unit (2.1) |
| A different hourly rate per plan, with credit at face value | the same credit would be worth different amounts on different days; "more credit for your money" is one rule |
| Plans that include hours, plus a separate dollar balance | two balances and an order between them to explain |
| A negative balance (debt) for disks at zero | post-paid by another name, and uncollectable |
| The backend as the meter | the component that restarts most would hold the money state (2.3) |
| Deriving intervals from watch events | a missed event is silent; a sample is self-correcting |
| One ledger object per minute or per interval | thousands of objects a day for no gain over a counter in one object |
| A database for the ledger now | cost and operations out of proportion to eleven concurrent sessions (3.1) |
| Keeping processed event IDs | state, where deterministic names need none (4.5) |
| A SetupIntent with Stripe Elements in our page | card fields in our origin; Checkout keeps them on Stripe's and brings its own card-testing controls |
| Keying the sign-up credit on the account alone | a new Google account is free; a new card number is not |
| Refusing prepaid cards as payment methods | needs a paid Radar plan, and turns away payers; they are only refused the bonus |
| Cutting calls in flight at zero | the product owner's decision: they finish |
| Waking sessions automatically when credit returns | a top-up should not start spending by itself |
| Deleting data when credit runs out | sleeping costs $0.55 a month and keeps the customer; deletion comes only after 14 days at zero, and only once there is email |
| Creating the webhook endpoint from code | its secret would pass through a log (4.7) |
| Terraform for Stripe's objects | a state file and a 0.x provider for eleven objects (4.7) |
| Failing open for everyone on a stale ledger | free compute for accounts with no credit during an operator outage |
| Failing closed for everyone | locks out customers for our fault |
| The webhook on the app host | that host has no public path today and should not gain one |
| Extending the policy operator | different rights and a shared restart |
