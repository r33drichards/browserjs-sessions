# Metronome integration: the meter and the credit ledger

Decision of the product owner, 2026-10-02: **"let's just use Metronome and
we can drop Bigtable."** This plan replaces the ledger parts of
[2026-10-02-metering-billing-design.md](2026-10-02-metering-billing-design.md)
(the billing operator's ledger, the `Grant` and `UsagePeriod` resources,
the backend's in-memory copy). Everything else in that design stands: the
prices, the card gate, the decision table, the stop sequence, Stripe for
payments, the UI.

The contract is
[`docs/contracts/billing/metronome.md`](../contracts/billing/metronome.md);
the other contracts are updated in the same pull request and say at their
top what changed. No product code, no Metronome account and no cloud
resource was created for this.

Facts about Metronome and Stripe were read from their documentation on
2026-10-02 (sources in section 9). **UNVERIFIED** marks what the
documentation does not say; each such point is a numbered sandbox check
(M1 to M11 in the contract) to run before code depends on it.

---

## 1. Go or no-go

**Go.** Nothing in the documentation rules Metronome out at our size, and
it has every piece of the model. Three things are not confirmed and should
be the product owner's first ten minutes after signing up (section 8).

| Question | Answer | Source |
|---|---|---|
| Can a new self-serve account get sandbox API access now? | **Yes, by the documentation**: "Access your sandbox API token via Developer, API tokens" after signing up at signup.metronome.com; "the environment is determined entirely by your API token". The sign-up page itself could not be read by a tool (it renders in the browser), and whether **production** access is equally self-serve is UNVERIFIED. | api-quickstart |
| What does it cost? | Startup plan: **0.8% of billing volume + $0.04 per 1,000 ingested events**, "Start free". No monthly minimum is stated; that there is none is UNVERIFIED. | metronome.com/pricing |
| At two users | about 17,000 events a month: **under $1**. | arithmetic below |
| At 1,000 accounts | about 2.9M events: **$116 a month**, plus 0.8% of billing volume (at most about $160 on $20,000 of monthly sales). Whether the 0.8% applies to credit we grant ourselves, with Metronome invoicing nothing, is UNVERIFIED; assume it does. So **$116 to $280 a month**, against revenue of about $20,000. | arithmetic below |
| Prepaid credits with expiry and priority | **Yes.** Credits have an access schedule (`starting_at`, `ending_before`) and a `priority` (lower is used first); order is priority, then free before paid, then earliest end. | prioritization-rules, create-a-credit |
| Recurring credits for the $5/$20/$100 tiers | **Yes** (`recurring_credits`: `MONTHLY`, `commit_duration` of one period, `rollover_fraction`). **Not used** in this design: see decision 2. | create-a-contract, hybrid-business-models |
| Balance never negative, nothing ever owed | **Yes, by a documented pattern**: a list rate of 0 and the real price as the commit rate; "usage burns down the commit at the real prices while balance exists, and falls back to 0 USD after the commit is exhausted". The net balance treats a negative segment as zero. Check M2. | guarantee-zero-overages, getNetBalance |
| A balance read API | **Yes**: `POST /v1/contracts/customerBalances/getNetBalance` ("real-time") and the balances list with each credit's ledger. Latency and rate limits are not published (M8). | getNetBalance, get-remaining-balance |
| Alerts by webhook at a threshold or at zero | **Yes**: `low_remaining_contract_credit_and_commit_balance_reached`, for every customer when no `customer_id` is given, signed with HMAC-SHA256, retried for about two days. Sent "within minutes of that condition being met". Metronome's own prepaid guide shows it firing at `remaining_balance: 0`; that a threshold of 0 is accepted, and whether it fires on expiry, is M4. | threshold-notifications, setup-webhooks, prepaid-credits guide |
| Auto-recharge of prepaid credit through Stripe | **Yes, natively** (`prepaid_balance_threshold_configuration` with a Stripe payment gate), but with limits that do not fit ours: the threshold is at least $5 and the recharge at least $10 above it; it needs Metronome connected to Stripe, a default payment method, a stored address and a tax provider; a failed payment switches it off; no monthly cap is documented. **Not used**: decision 3. | prepaid-balance-thresholds |

**Events, the arithmetic.** One `session.awake` event per awake session per
minute; one `session.kept` event per existing session per hour.

- Two users, two sessions awake 4 hours a day, four sessions kept: 2 x 240
  x 30 = 14,400, plus 4 x 730 = 2,920: 17,320 events, $0.69.
- 1,000 accounts, 200 sessions awake on average 4 hours a day, 2,000 kept:
  200 x 240 x 30 = 1.44M, plus 2,000 x 730 = 1.46M: 2.9M events, $116.
  Sending disk once a day instead of once an hour would make it $60.

**This is dearer than building it** (the storage alone was about $5 a
month on Firestore), and it is the right trade: no ledger of ours to get
wrong, no store to run or back up, and the money logic is a vendor's
tested product owned by the company that already takes our payments.

**The fallback**, if the sandbox turns out to be sales-gated or a minimum
makes it unusable: the small usage service on Firestore of
[pull request 83](https://github.com/r33drichards/browserjs-sessions/pull/83).
The interfaces below are the same for both (the `Ledger` hides which), so
falling back costs one implementation, not a redesign.

---

## 2. The design on one page

```
                 every 60 s                       usage events (HTTPS, idempotent)
  Sandboxes  ----------------->  observer  ------------------------------------->  METRONOME
  (the sessions)                 (billing operator, reduced:                       customers, contracts
                                  counts seconds, sends events,                    rate card  (list 0 / commit rate)
                                  writes nothing to the cluster)                   credits    (key, priority, expiry)
                                                                                   burn-down, balance
                                                                                        |        ^
                                                    alert at zero (signed webhook)      |        |  create credit, read balance
                                                                                        v        |  and usage
  user, API token,   create / resume / wake                                  +----------------------------+
  MCP client  ------------------------------------------------------------>  |          BACKEND           |
                     reads the owner's Account (one GET, no cache):          |  decision table, sweep,    |
                     card present? blocked? credit.exhausted?                |  balance pass (5 min),     |
                                                                             |  Stripe + Metronome hooks  |
  STRIPE  <----  Checkout (card, plan, pack), portal, auto-recharge  ------  |                            |
          ---->  webhooks: card saved, invoice paid, pack paid, refund ----> +----------------------------+
                                                                                        |
                                                                                        v
                                                                              Account (custom resource, "for now"):
                                                                              Stripe customer, card, flags,
                                                                              Metronome customer, credit.exhausted
```

- **Metronome is the meter and the ledger, and nothing else.** It is not
  connected to Stripe, sends no invoice and charges nobody. This is the
  second of the two patterns Stripe documents ("Metronome with Stripe
  Subscriptions": the subscription flows, "including Checkout", "continue
  to work as-is"), taken one step further: usage is never invoiced at all,
  because the list rate is zero and credit pays for everything.
- **Stripe is exactly as designed**: Checkout in setup mode for the card
  (the gate), Checkout for a plan or a pack, the portal, our off-session
  charge for auto-recharge, our webhook. A payment becomes credit when the
  backend creates a Metronome credit with the same deterministic key the
  `Grant` had (`purchase/<checkout session>`, `plan/<subscription>/<period>`,
  `signup/<card fingerprint>`). Metronome refuses a key it has seen (409),
  which is the whole idempotency, as the Grant's name was.
- **The observer sends what it saw.** It keeps the seconds half of the
  metering step (the gap rule: time not observed is never charged) and
  sends one event per awake session per minute with those seconds, and one
  per kept session per hour with its GB-seconds. Metronome turns seconds
  into money and draws the credit down: plan credit first, then sign-up,
  then purchased.
- **No copy of the ledger in the backend's memory, and no local ledger.**
  The decision at create, resume and wake reads the owner's `Account` from
  the API server at that moment and calls nobody else. Whether there is
  credit is one boolean on the Account, `spec.credit.exhausted`, set when
  Metronome's alert arrives and cleared when we add credit (which only we
  do). This is what Metronome's own prepaid guide recommends: "maintain a
  boolean entitlement flag in your database", flipped by the zero-balance
  webhook.
- **At zero**: the alert arrives, the backend re-reads the balance from
  Metronome (it believes Metronome, not the event), sets `exhausted` and
  `exhaustedAt`; five minutes later the sweep drains and sleeps the
  account's sessions, calls in flight finishing first, exactly as
  `enforcement.md` already says.
- **A balance pass every 5 minutes** reads Metronome for accounts with an
  awake session (and those with auto-recharge on, or with a credit due to
  expire). It repairs a missed or late alert and notices credit that
  expired.
- **When Metronome cannot be reached**, nobody is refused and nobody is
  stopped: decisions do not call it. An account with credit carries on, one
  without stays refused, usage that cannot be delivered within an hour is
  free, and a purchase's credit is created when Metronome is back (Stripe
  retries the webhook; the hourly reconcile repeats it).
- **The UI** reads the balance and usage through our API, which reads
  Metronome at the request. `backend-api.yaml` does not change.

---

## 3. Metronome objects for our model

Made by a setup command, as Stripe's products are (contract: "Objects made
by the setup command").

| Ours | In Metronome |
|---|---|
| An account | a customer whose ingest alias is the Account's name (`acct-<owner hash>`; no email address is sent), with one contract on the rate card `cu-standard-v1` |
| Awake time, $0.20 an hour | metric: sum of `seconds` on `session.awake` events; product "Awake time" (seconds divided by 3600); list rate 0, commit rate 20 cents an hour |
| A kept session, $1.40 a month (5 GB at $0.28 per GB-month) | metric: sum of `gb_seconds` on `session.kept` events; product "Disk" (divided by 2,628,000); list rate 0, commit rate 28 cents per GB-month |
| The sign-up credit, $5, 90 days, once per card | a credit, key `signup/<card fingerprint>`, priority 20 |
| A plan's monthly credit ($10, $44, $240 for $5, $20, $100), no rollover | a credit per **paid** period, key `plan/<subscription>/<period start>`, ending at the period's end, priority 10 |
| A pack ($5, $20, $50), 12 months | a credit, key `purchase/<checkout session>` or `recharge/<payment intent>`, priority 30 |
| The order: plan, sign-up, purchased | the priorities 10, 20, 30 |
| "Nothing is ever owed" | list rate 0: usage with no credit left costs nothing |
| Out of credit | the alert `cu-zero-balance` (threshold 0, every customer) |
| A closed period's record | Metronome's monthly usage statement; the period shown is the calendar month for everyone |

Who is the source of truth for what:

| Fact | Truth |
|---|---|
| A payment, a card, a subscription and its plan | Stripe (mirrored on the Account by the Stripe webhook, as designed) |
| Usage, credit granted, credit left | Metronome |
| Card present, blocked, exempt, terms, **credit exhausted** | the `Account` resource (what a decision reads) |
| Refunds, tax, invoices for plans and packs | Stripe; a refund archives the Metronome credit |

---

## 4. Usage ingestion

Contract: "Usage events". In short:

- `POST /v1/ingest`, up to 100 events a request, 34-day deduplication on
  `transaction_id`, events may be backdated 34 days.
- `transaction_id` is `awake/<session>/<tick>` or `kept/<session>/<hour>`:
  a retry or a replay is the same key and is ignored by Metronome.
- The seconds in an event are computed by the **unchanged** gap rule of
  `metering.md`; the 18 vectors still pin them. The observer does no money
  arithmetic.
- 429 and 5xx: retry with backoff, same keys, for up to an hour from
  memory; then, or on a restart, drop. 4xx: log and drop. Every loss is
  free time for the user. Metronome's guide suggests a durable queue in
  front of ingest; we do not add one, because an hour of lost usage costs
  us cents and a queue is another thing to run.
- The observer's state is in its own memory (each session's last sight,
  the hour's disk seconds). That is not a copy of the ledger: losing it
  loses charges and nothing else.

---

## 5. Enforcement

Contract: `enforcement.md` (updated) and "What the Account records".

**What is checked, and where.** Per start, not per call: `api.create`,
`api.patch` (resume), and `proxy.Waker.await` before `Store.Wake` (one
check per wake; concurrent callers share it). A call to a running session
makes no billing read. The check is one GET of the owner's Account and the
decision table: blocked, exempt, terms, **no card**, **credit exhausted**,
then the limits.

**Where "out of credit" lives: on the Account, not read from Metronome
per decision.** Recommended, for three reasons:

1. An outage of Metronome then cannot refuse a paying customer or let the
   decision hang; a per-decision read would put a third party on the wake
   path of every MCP client.
2. It is what the vendor recommends for this model.
3. It is not an in-memory copy: it is one stored boolean with one writer
   (`ensureCredit`), on the resource the owner agreed to keep.

What it costs: the flag can be late. Credit gone, to the alert or the
balance pass (minutes), plus the 5-minute grace and the drain: about 20
minutes of use at most, rated at zero and owed by nobody. The other way
round cannot happen: credit only arrives through our own `EnsureGrant`,
which clears the flag in the same call.

**Failure behaviour.**

| What fails | Effect |
|---|---|
| Metronome's alert is late or lost | the balance pass sets the flag within 5 minutes |
| Metronome unreachable, reads | the flag keeps its value; nobody new is refused or stopped; the billing page shows the last stored balance, marked stale |
| Metronome unreachable, ingest | the observer retries for an hour, then that usage is free |
| Metronome unreachable at a purchase | the Stripe webhook answers 500 and Stripe retries; the hourly reconcile repeats it. The card was charged and the credit arrives late, not never. |
| The observer is down | no usage, so no alert: use is free until it is back. Its Lease going stale is logged and shown. |
| Our Metronome webhook is down | Metronome retries for about two days; the balance pass does not depend on it |

**Reconcile for missed webhooks**: the balance pass (Metronome), and the
existing 15-minute and hourly Stripe reconciles, which now also re-make
any Metronome credit that is missing (409 for the ones that exist).

---

## 6. What happens to the build

State on `main` today: track E (UI) is merged. Open: track B deployment
([#79](https://github.com/r33drichards/browserjs-sessions/pull/79)), track
D's interfaces and fakes
([#80](https://github.com/r33drichards/browserjs-sessions/pull/80)).
Tracks A and C have no pull request yet.

### Contract files

| File | Change |
|---|---|
| `metronome.md` | **new**; wins where another file disagrees |
| `crd-account.yaml` | `status` and its subresource removed; `spec.metronomeCustomerId` and `spec.credit` added; printer columns |
| `crd-grant.yaml`, `crd-usageperiod.yaml` | marked superseded; not deployed |
| `metering.md` | note at the top: what is observed and the seconds rule stay; money, debit, totals and periods are Metronome's |
| `metering-vectors.json`, `spike/meter_ref.py` | unchanged (seconds: the observer; money: the fake Metronome) |
| `enforcement.md` | the decision reads the Account with no informer; `balance` and `stale` inputs replaced by `exhausted`; rows 6 and 7 removed; a table of what happens when each part is down; auto-recharge moves to the balance pass |
| `testing.md` | `Ledger` changed, `Metronome` added (below); fakes; scenario 5 added; the observer's tests |
| `stripe.md` | note at the top: "create the Grant" is `Ledger.EnsureGrant` onto Metronome with the same keys; nothing else changes |
| `deploy.md` | the Secret `metronome`, its four GitHub secrets, `metronome-setup.yml`, the public route `api-metronome-webhook`, the operator's egress and rights, `BILLING_BALANCE_PASS` in place of `BILLING_STALE_AFTER`, labels |
| `backend-api.yaml` | `POST /metronome/webhook` added; nothing else |
| `catalogue.yaml`, `ui-states.md`, `legal-pages.md` | unchanged |

### The interfaces (`testing.md` has them in full)

```go
// Ledger: the credit, kept in Metronome. No decision calls it.
type Ledger interface {
	EnsureCustomer(ctx context.Context, account string) (customerID string, err error)
	Balance(ctx context.Context, account string) (Balance, error)
	Usage(ctx context.Context, account string, from, to time.Time) (Usage, error)
	EnsureGrant(ctx context.Context, g Grant) (created bool, existing Grant, err error) // signature unchanged
	Revoke(ctx context.Context, sel GrantSelector, reason string) error                // unchanged
	EnsureCredit(ctx context.Context, account string) (AccountCredit, error)            // writes Account.spec.credit
}

// Metronome: every call the backend makes to Metronome; one method per endpoint.
type Metronome interface {
	CreateCustomer, CustomerByAlias, CreateContract, CreateCredit, Credits, ArchiveCredit, Usage
}
```

`Ledger.View` is gone (there is no status to view). `Accounts`, `Stripe`,
`Sessions`, `InFlight` and `Clock` keep their signatures. The fake is of
`Metronome`; `Ledger` is the real code over it. `TestCardGateScenario`
keeps every step, status and body; where it said `Ledger.Tick` it calls
the fake's `Tick` and posts the alert event it returns.

### Per track

| Track | Now | Change | Size |
|---|---|---|---|
| **A, operator** | not yet a pull request | Becomes the **observer**: the pass, the observation, the **seconds** function (vectors: `awakeSeconds`, `diskGBSeconds`), the hourly disk sum, the sender to Metronome with keys, batching, retry and give-up, the Lease. **Drops**: money, debit, grants, `Account.status`, periods, `UsagePeriod`, retention, conditions, and reading Accounts at all. Needs the Metronome token and egress on 443. | Smaller |
| **B, deployment** (#79) | open | **Drops** the `Grant` and `UsagePeriod` CRDs and their rights and CEL tests. **Adds** the Secret `metronome` from the GitHub secrets (sandbox or production by `STRIPE_MODE`), the route `api-metronome-webhook` in every `pomerium-config.yaml`, the operator's new egress and its smaller Role with the Lease, `metronome-setup.yml`, the backend's Role without `watch`. The export CronJob exports Accounts only. The Account CRD is re-copied. | About the same |
| **C, Stripe** | not yet a pull request | **Nothing in what it calls**: it creates and revokes credit only through `Ledger.EnsureGrant` and `Revoke`. Three small things: `decideSignupCredit` reads "another account's" as an `existing` with an empty Account; the checkout return no longer waits for a tick; `ensureRecharge`'s trigger is called from D's balance pass, not the sweep. | Very small |
| **D, enforcement** (#80 open: interfaces and fakes) | in review | #80: replace `Ledger` as above, add `Metronome`, replace the fake ledger with the fake Metronome (the Go step moves inside it). Then, new: `backend/internal/billing/metronome/` (the HTTP client; `Ledger` over it; the webhook handler; the balance pass), `backend/cmd/metronome-setup`, `Accounts` on direct reads with label selectors and **no informer**, the decision reading `spec.credit.exhausted`, `GET /api/billing` and `/usage` from `Ledger.Balance` and `Usage`, scenario 5 and `TestNoLedgerCopy`. The decision table, the sweep, the drain and the answers are as planned. | The most change: one new package, the informer removed |
| **E, UI** (merged) | done | None: `backend-api.yaml` is unchanged. Optional later: the checkout return can stop polling for the tick, since credit is there at once. | None |
| **F, site**; **G, sign-up** | | None. The privacy page gains Metronome as a processor (usage figures and an account identifier; no email address, no card data). | One line |

Suggested order: D amends #80 first (C and the scenarios build on the
fakes); the sandbox checks M1 to M6 in parallel, since they can change the
contract; then A, B and D's new package together.

---

## 7. Decisions, each with a default

| # | Decision | Default | Why |
|---|---|---|---|
| 1 | Where "out of credit" lives | **On the Account** (`spec.credit.exhausted`), set from Metronome's alert and the balance pass; not read from Metronome per decision | Section 5 |
| 2 | Who owns subscriptions: Stripe Billing, or Metronome contracts with recurring credits | **Stripe stays**; plan credit is granted by us per paid invoice | Credit must follow a **payment**; a Metronome recurring credit is granted on schedule whether or not Stripe collected. It also keeps Checkout and the portal, which Stripe lists as "requires custom integration" with Metronome's own invoicing, and leaves track C and the merged UI untouched. |
| 3 | Auto-recharge: Metronome's, or ours | **Ours** (already designed, off by default), triggered from the balance pass | Metronome's needs a $5 threshold and $10 steps, a tax provider and stored addresses, has no monthly cap, and switches itself off on a failed payment |
| 4 | Connect Metronome to Stripe at all | **No**, for now | Nothing is invoiced by Metronome. Revisit if we ever want post-paid or enterprise contracts: that is what the connection is for. |
| 5 | How often disk usage is sent | **Hourly** | $58 a month at 1,000 accounts; daily would be $2 and delays the disk charge by up to a day. Free to change later. |
| 6 | Usage period shown to a subscriber | **The calendar month**, like everyone | It is Metronome's statement period; the plan's credit still follows Stripe's billing period. One sentence changes on the billing page. |
| 7 | Do exempt accounts (admins) get Metronome customers | **Yes** | "Metered for the record"; their usage rates at zero and counts toward the event fee (cents) |
| 8 | Send the email address to Metronome | **No**: the customer's name is the owner hash | Less personal data with a processor; support looks an account up by hash |
| 9 | Pull request 83 (the storage options) | **Close it**, keeping its link here as the fallback | Superseded by this decision |

**What would change the recommendation back to our own service**: sandbox
or production access turns out to need a sales call or a minimum fee; M2
fails (credit cannot be drawn at a commit rate with a zero list rate, so
"nothing owed" is not expressible); or the alert's delay in M4 is hours,
not minutes, **and** the balance read is rate-limited too tightly for the
pass to stand in for it.

---

## 8. The product owner's to-do list

No agent creates the account, sees a token, or types one anywhere. Tokens
and secrets go from Metronome's dashboard into GitHub's secret form and
nowhere else: not a chat, not a file, not a log.

1. **Sign up** at https://signup.metronome.com/ with the company address.
   Note whether it asks for a card or a sales call, and whether the
   pricing shown is still 0.8% + $0.04 per 1,000 events with no minimum.
   If it is sales-gated or has a minimum that matters, stop and say so:
   the fallback is section 1's.
2. In the **sandbox**: Developer, API tokens, create a token. Put it in
   the GitHub Actions secret **`METRONOME_SANDBOX_API_TOKEN`**.
3. Run the workflow **`metronome-setup`** with `environment: sandbox`,
   `apply: false`; read the plan; run it again with `apply: true`. (Exists
   once track D's setup command is merged.)
4. In the sandbox: Developer, Notifications, Webhooks, Add:
   `https://api.computeruse.site/metronome/webhook`. Put its secret in
   **`METRONOME_SANDBOX_WEBHOOK_SECRET`**. Send the test notification.
5. Run, or ask a developer to run with their own sandbox token, the
   checks **M1 to M11**; the results go into `docs/billing-metronome.md`.
6. Stage 1 (shadow): merge the change that sets `BILLING=meter`. Compare
   Metronome's usage for the two users with what they did, for a week.
7. Before live payments: ask Metronome how production access is granted
   and what, if anything, is signed. Create the production token and
   webhook the same way into **`METRONOME_PRODUCTION_API_TOKEN`** and
   **`METRONOME_PRODUCTION_WEBHOOK_SECRET`**; run `metronome-setup` with
   `environment: production`.
8. Add Metronome to the privacy page's list of processors (track F drafts
   the line) and check its data processing terms.
9. **Do not** enable Metronome's Stripe integration (decision 4).

The Stripe steps of the first design's to-do list are unchanged.

---

## 9. Sources

Read 2026-10-02.

- Stripe: https://docs.stripe.com/billing/how-metronome-works-with-stripe.md
- Pricing: https://metronome.com/pricing (0.8%, $0.04 per 1,000 events,
  "Start free"; read through a summariser, so check the page). That the
  acquisition by Stripe closed on 2026-01-14 is from a secondary source.
- Getting started and tokens:
  https://docs.metronome.com/guides/get-started/api-quickstart.md
- Ingest: https://docs.metronome.com/api-reference/usage/ingest-events.md ;
  https://docs.metronome.com/guides/events/send-usage-events.md
- Credits and order of use:
  https://docs.metronome.com/api-reference/credits-and-commits/create-a-credit.md ;
  https://docs.metronome.com/guides/pricing-packaging/apply-credits-and-commits/prioritization-rules.md
- Nothing owed:
  https://docs.metronome.com/guides/pricing-packaging/apply-credits-and-commits/guarantee-zero-overages.md
- Balance:
  https://docs.metronome.com/api-reference/credits-and-commits/get-the-net-balance-of-a-customer.md ;
  https://docs.metronome.com/guides/customers-billing/optimize-customer-experience/get-remaining-balance.md
- Alerts and webhooks:
  https://docs.metronome.com/guides/customers-billing/set-up-notifications/threshold-notifications.md ;
  https://docs.metronome.com/api-reference/alerts/create-a-threshold-notification.md ;
  https://docs.metronome.com/guides/platform-configuration/setup-webhooks.md
- The prepaid model, and the entitlement flag:
  https://docs.metronome.com/guides/pricing-packaging/billing-model-guides/prepaid-credits.md
- Subscriptions and recurring credits:
  https://docs.metronome.com/guides/pricing-packaging/billing-model-guides/hybrid-business-models.md ;
  https://docs.metronome.com/api-reference/contracts/create-a-contract.md
- Auto-recharge and payment gating:
  https://docs.metronome.com/guides/customers-billing/optimize-customer-experience/prepaid-balance-thresholds.md ;
  https://docs.metronome.com/guides/pricing-packaging/apply-credits-and-commits/manual-payment-gated-commits.md
- Stripe connection: https://docs.metronome.com/integrations/invoice-integrations/stripe.md
- Rates and products:
  https://docs.metronome.com/api-reference/rate-cards/add-a-rate.md ;
  https://docs.metronome.com/guides/implement-metronome/core-concepts/create-products-contracts.md
- Export (set up by contacting Metronome; plan not stated):
  https://docs.metronome.com/guides/reporting-insights/data-export/overview.md

**Not said by the documentation** (each is a sandbox check or a question
for Metronome): any latency or rate limit; whether production access is
self-serve; whether there is a minimum fee and what counts as billing
volume; whether a threshold of 0 is accepted and whether the alert fires
on expiry or fires again; what happens to an event for an unknown
customer; whether schedule times must be on the hour; whether a uniqueness
key survives archiving; where data is stored (residency).
