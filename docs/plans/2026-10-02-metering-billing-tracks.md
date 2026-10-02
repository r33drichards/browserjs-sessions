# Metering and billing: tracks

Seven tracks in five stages. Design:
[2026-10-02-metering-billing-design.md](2026-10-02-metering-billing-design.md).
Contracts: [../contracts/billing/](../contracts/billing/README.md).

**Nothing starts until the product owner has answered section 11 of the
design**; an answer that differs from a recommended default is first made
in the contracts, in a pull request of its own.

## Rules for every track

- **Build against the contracts, not against another track's branch.**
  Where a track needs something another track makes, it uses the fake named
  below until both are merged.
- **A contract is changed only in its own pull request**, which says which
  tracks it affects. If a contract is wrong or silent, stop and raise it; do
  not work around it in code.
- **Stay inside the files the track owns.** The few files two tracks must
  both edit are listed under "Shared files", with who adds what.
- **Everything is off by default.** With `BILLING` unset the product
  behaves exactly as it does today, and a test says so
  (`TestBillingOffIsToday`). No track changes a default.
- **Stateful dependencies are interfaces.** Billing and enforcement logic
  touches accounts, the ledger, the clock, Stripe and the session store
  only through the interfaces of
  [`testing.md`](../contracts/billing/testing.md), and its tests run on the
  in-memory fakes: no Kubernetes API, no Stripe.
- **No track handles a Stripe key.** Tests sign their webhook events with a
  made-up secret. Anything that needs a real sandbox is a documented script
  the product owner or a developer runs with their own key.
- One pull request per track (more if it helps review), tests included, no
  `Co-Authored-By` trailer, nothing merged or deployed by the agent.
- Go and Node as the repository already does them; the operator's Python
  through the Nix dev shell.

## Stages

| Stage | Flag | Tracks | Ends when |
|---|---|---|---|
| 1. Meter in shadow | `BILLING=meter`, no Stripe | A, B, D (interfaces, reading side), E (billing page, read-only) | the charges shown in production for the two users match what they did |
| 2. Test payments | + `STRIPE_MODE=test` | C, E (the gate, buying), F (pricing page) | a test card saved grants $5 once; a pack and a subscription each grant credit; a test clock renews, fails and cancels correctly |
| 3. Enforce | `BILLING=enforce` | D (enforcing side), E (blocked states) | the card-gate scenario passes in CI **and** is repeated by hand in production with test cards: no card, no session; card removed, sessions asleep; zero credit, drained and asleep |
| 4. Live payments | `STRIPE_MODE=live` | F (legal pages approved), the product owner's steps 11 to 16 | a real card saved, a real pack bought and refunded |
| 5. Open sign-up | `OPEN_SIGNUP=true` + Pomerium policy | G | prerequisites P1 to P10 of the design's section 8.5 are each ticked in the pull request |

A to F are built at the same time; the stage is which flag is on in
production. G is built last and switched on its own. Auto-recharge is part
of track C behind `AUTO_RECHARGE` and can be turned on at any stage from 2.

## Shared files

| File | Who adds what |
|---|---|
| `backend/cmd/server/main.go`, `backend/internal/config/config.go` | D: `BILLING`, `BILLING_GRACE`, `BILLING_DRAIN_TIMEOUT`, `BILLING_STALE_AFTER`, `BILLING_EXEMPT_EMAILS`, `BILLING_CATALOGUE`, `MAX_AWAKE_SESSIONS`, `WAKES_PER_HOUR`, `ZERO_BALANCE_DELETE`, `SIGNUP_CREDIT`, and the wiring of `internal/billing`. C: `STRIPE_MODE`, `STRIPE_API_KEY`, `STRIPE_WEBHOOK_SECRET`, `AUTO_RECHARGE`, and the wiring of `internal/billing/stripe`. G: `OPEN_SIGNUP`, `SIGNUPS_PER_DAY`, `SIGNUPS_PER_IP_PER_DAY`, `TERMS_VERSION`. Separate blocks; no refactoring of what is there. |
| `backend/internal/api/api.go` | D only: the store behind an interface, the check before create and resume, `stoppedBy`, `draining` and `deleteAfter` on the session view. C registers its routes from its own package. |
| `backend/internal/proxy/` | D only: `Waker.Store` behind an interface, one call before `Store.Wake`, the refusal of new requests to a draining session, the count of calls in flight (`InFlight`). |
| `backend/internal/idle/sweeper.go` | D only: the store behind an interface; `Sleep` called with the reason `idle`. |
| `backend/internal/sessions/` | D only: the sleep reason on `Store.Sleep`, `Wake` accepting `credit` and `payment-method`, the draining annotation. |
| `backend/internal/billing/` (the interfaces file) | D writes it from `testing.md`, **first**, as a pull request of its own that C then builds on. |
| `backend/go.mod` | C adds `stripe-go`. |
| `deploy/base/backend.yaml` | B: the Role's new rules, the catalogue's mount, and every env var of `deploy.md` (with `BILLING` absent). Nobody else. |
| every `pomerium-config.yaml` | B: the `api-stripe-webhook` route. G: the policy change. |
| `.github/workflows/deploy.yml` | B: the `stripe` Secret and the `billing-mode` ConfigMap. |
| `.github/workflows/images.yml`, `hack/pin-images.sh` | B: the operator's image, beside the others, not reordering. |
| `web/src/api.ts`, `App.tsx`, `shell.tsx` | E only. |
| `terraform-provider-browserjs/` | E's sibling task, small: the provider shows `error` and `billingUrl` of a 402 as its diagnostic. Owned by D (it is the API's contract), one file. |
| `docs/` | Each track adds its own page; nobody edits the design or the contracts. |

---

## Track A: billing operator

**Owns**: `images/billing-operator/` (all of it: `Dockerfile`, the Python
package `billing_operator`, `tests/`, lock file, flake, in the layout of
`images/policy-operator/`), `docs/billing-operator.md`.

**Consumes**: `metering.md`, `metering-vectors.json`, `spike/meter_ref.py`,
the three CRDs, `catalogue.yaml`, the operator rows of `deploy.md`.

**Builds**

- `step(meter, grants, observed, now, rates)`: the step of `metering.md`, a
  pure function, integer arithmetic.
- The pass: one list of Sandboxes, grouping by owner label, the observation
  (exists, awake, readySince, diskGB), one status update per account with
  `resourceVersion`, the retry and skip rule.
- `status.plan`, `balances`, `burnMicrosPerHour`, period open and close
  (`UsagePeriod`), pruning of `meter.consumed`, the daily retention pass
  (never deleting `signup` Grants), `Grant.status` for display, the
  `Metered` and `PaymentFailed` conditions.
- The catalogue read from the mounted file and re-read when it changes; a
  file that does not parse keeps the last good one.
- Liveness, and a log line per pass (accounts touched, micro-dollars
  charged, duration) so that a stalled meter is visible.

**Tests**

- Every vector of `metering-vectors.json` through `step`.
- Property tests on `step`: awake seconds never exceed `now - lastSeen`;
  nothing is negative; what is taken from grants plus the overdraft equals
  what was charged; the balance is never below zero; a repeated tick with
  the same `now` charges nothing; any gap over `MAX_GAP` charges at most
  `MAX_GAP`; 3600 seconds awake in any pattern of ticks charge exactly the
  hourly rate.
- The pass against a fake API (objects in memory): a conflict is retried
  with the same `now`; three conflicts skip; an owner with no Account is
  logged and skipped; a warm-pool Sandbox is never charged, awake or disk;
  a sleeping session is charged disk only; a rate changed in the catalogue
  applies from the next tick.
- Period close at a month boundary and at a Stripe period boundary.

**Done when**: `nix develop -c pytest` passes in the directory; the image
builds; a documented command, given a directory of Sandbox, Account and
Grant YAML and a list of times, prints each account's status after each
tick.

**Must not touch**: `deploy/`, `backend/`, `web/`, `.github/workflows/`,
`images/policy-operator/`, the contracts.

**Fakes it needs**: none.

---

## Track B: deployment

**Owns**: `deploy/base/crd-account.yaml`, `crd-grant.yaml`,
`crd-usageperiod.yaml`, `catalogue.yaml` (copied from the contracts),
`deploy/base/billing-operator.yaml` (new), the edits to
`deploy/base/kustomization.yaml`, `networkpolicy.yaml`,
`secrets.example.yaml`, `backend.yaml`; the `deploy/gke` and `deploy/local`
overlays; the `api-stripe-webhook` route in every `pomerium-config.yaml`;
the Stripe part of `.github/workflows/deploy.yml`; the operator in
`images.yml` and `hack/pin-images.sh`; the `billing-export` CronJob and,
in `infra/main`, its bucket and Workload Identity binding;
`docs/billing-deployment.md`.

**Consumes**: `deploy.md`, the three CRDs, `catalogue.yaml`.

**Builds**: everything in `deploy.md` except the code. `BILLING` is absent
from the base and from `deploy/gke`; `deploy/local` sets `meter`.

**Tests**

1. On kind: the three CRDs are accepted; each CEL rule refuses what its
   message says (an Account whose name is not its hash, a changed owner, a
   changed `stripeCustomerId`, a `signupCredit` changed once set, a changed
   Grant, an un-revoked Grant, a non-admin Grant with no expiry, a changed
   UsagePeriod). A rule that does not behave is fixed in a contract pull
   request.
2. RBAC: the backend's ServiceAccount cannot patch `accounts/status`; the
   operator's cannot patch an Account's `spec`, cannot create a Grant, and
   cannot read Secrets.
3. NetworkPolicy: the operator reaches the API server and nothing else;
   nothing reaches it.
4. `kubectl kustomize deploy/local` and `deploy/gke` render;
   `deploy/base/catalogue.yaml` is byte for byte the contract's; a changed
   catalogue reaches the two pods without a restart. The deploy workflow's
   Secret step, run with fake values in a kind job, makes the Secret and
   ConfigMap and removes them when `STRIPE_MODE` is empty, printing no
   value.
5. The export CronJob writes a file a second cluster can apply.

**Done when**: the above pass in CI or as documented scripts; with the
operator's image pinned, `deploy/gke` applies with billing still off and
nothing about the running product changes.

**Must not touch**: `backend/` and `web/` code, `images/`, Pomerium's
policy (G), the contracts.

**Fakes it needs**: a placeholder image for the operator until A's exists.

---

## Track C: backend, Stripe

**Owns**: `backend/internal/billing/stripe/` (new: the implementation of
the `Stripe` interface over `stripe-go`; the checkout, portal, auto-recharge
and webhook handlers; `ensurePaymentMethods`, `decideSignupCredit`,
`ensureSubscription`, `ensurePurchase`, `ensureRecharge`; the reconcile),
`backend/cmd/stripe-setup/` (new), `.github/workflows/stripe-setup.yml`
(new), the edits to `main.go`, `config.go` and `go.mod` named under "Shared
files", `docs/billing-stripe.md`, `docs/billing-development.md`.

**Consumes**: `stripe.md`, `catalogue.yaml`, `backend-api.yaml` (checkout,
portal, auto-recharge, webhook), `testing.md` (the interfaces, scenario 4),
`deploy.md` (configuration, the key's mode check, local development).

**Builds**

- The setup command: products, prices, the portal configuration from the
  catalogue; `--apply=false` prints what it would change; a second run
  changes nothing.
- `POST /api/billing/checkout` (setup, subscription, payment),
  `GET /api/billing/checkout/{id}`, `POST /api/billing/portal`,
  `PUT /api/billing/auto-recharge`; cookie only; the limits on setup
  checkouts and saved cards.
- `POST /stripe/webhook` on the API host only: raw body, signature, mode,
  the event table.
- The ensure functions, the sign-up credit decision, the two reconciles.
- Auto-recharge: the attempt (called by D's sweep through an interface),
  `seq` written before the call, the failure handling. Behind
  `AUTO_RECHARGE`.
- The Stripe side of account deletion, called by D's handler.

**First, four checks in a sandbox** (scripts in the pull request, run by
the product owner or a developer with their own key; results written into
`docs/billing-stripe.md`):

1. Through Pomerium locally: `stripe trigger checkout.session.completed`
   reaches the handler with a signature that verifies (the body and header
   are untouched).
2. A setup-mode Checkout: which events arrive and in what order; that the
   card is attached to the customer; what `payment_method.detached` carries
   when the card is removed in the portal and through the API; whether the
   portal lets a customer with no subscription remove their last card; the
   fingerprint of the same test card saved twice, and through a wallet.
3. With a test clock: subscribe, advance a month (a new plan Grant, the old
   credit gone), upgrade in the portal (new period, old Grant superseded,
   charged at once), downgrade (waits), fail a renewal with the declining
   test card (`past_due`, no Grant), cancel (pay as you go at the period's
   end).
4. The restricted key: the smallest set of permissions with which
   everything above works, by name, into `deploy.md`. And an off-session
   charge on the test card that requires authentication: the error and the
   events, as `ensureRecharge` expects them.

**Tests**: on the `billingtest` fakes: scenario 4 of `testing.md`
(`TestWebhookIdempotency`) in full; every row of the event table; a
checkout session that belongs to another account; subscribe while
subscribed; the reconcile re-making Grants after they are deleted and
noticing a card removed with no webhook; the setup command against a fake,
twice; auto-recharge at the threshold, at the cap, on failure, and with
the crash between `seq` and the call. A live key with `STRIPE_MODE=test`
refuses to start, and the error does not contain the key.

**Done when**: `go test ./...` passes; the handlers match
`backend-api.yaml`; the four sandbox checks are recorded; scenario 1 of
`testing.md` passes with this track's real webhook handler in it.

**Must not touch**: the interfaces file and the rest of
`backend/internal/billing` (D), `api.go`, `proxy/`, `sessions/`, `web/`,
`deploy/`.

**Fakes it needs**: `billingtest` (D's first pull request).

---

## Track D: backend, accounts and enforcement

**Owns**: `backend/internal/billing/` (new: the interfaces of `testing.md`;
their Kubernetes implementations over an informer in `billing/kube`; the
fakes in `billing/billingtest`, including the Go port of the metering step;
the account state and decision functions; the sweep with drain, snapshot
and sleep; the deletion pass; `GET /api/billing`, `/api/billing/usage`,
`/api/billing/catalogue`, `DELETE /api/account`), the scenario tests in
`backend/internal/billing/scenarios/`, the edits to `api.go`, `proxy/`,
`idle/`, `sessions/`, `main.go` and `config.go` named under "Shared files",
the Terraform provider's diagnostic, `docs/billing-enforcement.md`.

**Consumes**: `testing.md`, `enforcement.md`, `backend-api.yaml`, the three
CRDs, `catalogue.yaml`, `metering.md` and `metering-vectors.json` (for the
fake ledger; the backend never computes a real balance), `deploy.md`.

**Builds, in this order**

1. **The interfaces and the fakes**, as a first pull request: the file of
   `testing.md`, `billingtest`, and the existing concrete types put behind
   interfaces where `testing.md` says (`api`, `proxy.Waker`, `idle`), with
   no change of behaviour. C starts from this.
2. Stage 1: Accounts made on first sight; the three read routes; the
   decision function computed and logged as `would_refuse`.
3. Stage 3: the decision table at create, resume and wake; the answers,
   including the MCP form; the sweep (grace, drain with `InFlight`, snapshot
   then sleep with the reason, the three triggers); the wake rate limit;
   the cluster cap.
4. Account deletion; the deletion of sessions at zero (behind
   `ZERO_BALANCE_DELETE`).

**First**: what Claude's MCP clients show for a 402 from the session
endpoint, and whether any retries it, recorded; the answer form in
`enforcement.md` is confirmed or changed by a contract pull request.

**Tests** (names and steps are `testing.md`'s, verbatim):

- **`TestCardGateScenario`**: steps a1 to e4 with the statuses and bodies
  written there, through the real API mux, the real webhook handler and the
  real sweep, on the fakes.
- **`TestNoCardCannotCreate`**, **`TestNoCardCannotCreateWithToken`**,
  **`TestNoCardCannotCreateWarm`** (no `SandboxClaim` is created),
  `TestExemptNeedsNoCard`, `TestBillingOffIsToday`,
  `TestMeterModeRefusesNothing`.
- **`TestExhaustionDrain`** (a call in flight finishes; the timeout
  variant; credit arriving during the drain calls the stop off).
- **`TestDiskAccruesAsleep`** (and the deletion clock, on and off).
- The decision function as a table test: one case per row of the decision
  table and per column, plus the order of the rows.
- The Go port of the step runs `metering-vectors.json`.
- An admin waking a user's session is judged on the owner's account; a
  token is judged as its owner; reading, stopping and deleting work with no
  card and at zero; a session stopped for credit is not woken by credit
  arriving.

**Done when**: `go test ./...` passes with every test above present under
its name; the read routes match `backend-api.yaml` field for field; no
test in `backend/internal/billing` imports `k8s.io/client-go` or reaches
the network.

**Must not touch**: `backend/internal/billing/stripe` (C),
`backend/internal/auth`, `backend/internal/tokens` (G), `web/`, `deploy/`.

**Fakes it needs**: its own. Until C's handler exists, scenario 1 posts its
events to a stub that calls the same ensure functions' signatures; the stub
is deleted when C merges, and the scenario must pass unchanged.

---

## Track E: UI

**Owns**: `web/` (the first-run gate, the `/billing` page with the plan
picker, the payment-method and auto-recharge container, the balance in the
top navigation, the banners, the add-credit modal, the blocked states on
create, list and session pages, the checkout return, the delete-account
modal; `billingApi.ts`).

**Consumes**: `backend-api.yaml`, `ui-states.md`, the design's section 6.

**Builds**: every row of the states table of `ui-states.md`. Nothing of it
exists when `GET /api/billing` answers 404.

**Tests**: vitest for the client and for the formatting (dollars rounded
down to the cent; the hours estimate); a component test per state against
a mock of `backend-api.yaml`; a new user with no card sees the gate and
cannot reach the create form; the create form disabled with the right
alert for each reason; each `stoppedBy` reason on the session page; the
checkout return's polling with a fake timer, for each kind and each
sign-up outcome; the existing tests keep passing.

**Done when**: `npm test` and `npm run build` pass; every state can be
reached against the mock; nothing about the app changes when the mock
answers 404.

**Must not touch**: `backend/`, `deploy/`, `site/`, the contracts.

**Fakes it needs**: a mock of `backend-api.yaml`.

---

## Track F: public site

**Owns**: `site/pricing.md`, `site/legal/` (terms, privacy, acceptable
use, refunds), `site/contact.md`, the navigation and footer entries in
`site/.vitepress/config.ts`, the "limits" reference page's new rows.

**Consumes**: `catalogue.yaml`, `ui-states.md` (the public pricing
section), `legal-pages.md`.

**Builds**: the pricing page from the catalogue at build time (enabled
items only): the four options as a table, the two rates, the packs, the
sign-up credit and the card requirement; the four legal pages as **drafts
marked as drafts**, from the points of `legal-pages.md`, with placeholders
for the legal name, address and email addresses the product owner supplies.

**Tests**: the site builds; a test that the pricing page's numbers are the
catalogue's.

**Done when**: the site builds with the pages. The legal pages are
published only when the product owner has approved their text in the pull
request; until then they are excluded from the build by a flag in the
site's configuration.

**Must not touch**: anything outside `site/`.

**Fakes it needs**: none.

---

## Track G: open sign-up (last, separately switched)

**Owns**: `OPEN_SIGNUP` in `backend/internal/auth` and
`backend/internal/tokens` (the allow-list check becomes "has an Account
that is not blocked"), the sign-up limits and the terms route
(`POST /api/billing/terms`) in a new file of `backend/internal/billing`
agreed with D, the policy change in every `pomerium-config.yaml`, the two
blocked ports in `deploy/base/networkpolicy.yaml`, `docs/open-signup.md`
(the runbook: how to open, how to close again, how to block an account,
how to turn the sign-up credit off).

**Consumes**: `enforcement.md` ("Sign-up limits"), `deploy.md` (stage 5,
the Pomerium policy), `backend-api.yaml` (terms), the design's section 8.

**Starts when**: stage 3 is in production.

**First, two tests whose results decide the rest**:

1. P4: can a Google account whose email is not verified by Google sign in
   through Dex and Pomerium and be taken for that address? If yes, add the
   check and say where.
2. P5: can a Google account that is not a listed test user sign in while
   the OAuth app is in "Testing"?

**Builds**: the flag; the limits; terms acceptance; the policy change, in
the same pull request as the flag so that the two cannot be deployed
apart; closing sign-up again is reverting that pull request.

**Tests**: with the flag off nothing changes (today's allow-list tests
pass untouched); with it on, a new address gets an Account in state
`terms`, then `no_card`, and can create nothing (the card-gate scenario
run for a brand-new address); a blocked account's token is refused on the
next request; the sign-up beyond the daily limit and beyond the per-address
limit are refused with `signups_paused` and make no Account; a deleted
account signing up again, with the same card, gets no second sign-up
credit.

**Done when**: the tests pass, the runbook is written, and the pull
request's description ticks P1 to P10 with a link to the evidence for
each. The product owner merges and deploys it; no agent does.

**Must not touch**: the metering, Stripe and UI code of A, C and E.

**Fakes it needs**: `billingtest`.

---

## After the tracks: integration

- **Stage 1** (A, B, D reading, E read-only page merged): on kind, a
  session awake for ten minutes shows nine to ten minutes charged at the
  awake rate and ten of disk; asleep, disk only; the operator killed for
  five minutes loses those minutes and nothing else; a warm pod is never
  charged. Then production in `meter` for a week, compared by hand with
  what the two users did.
- **Stage 2** (C merged, the product owner's steps 1 to 9 done): the
  sandbox checks of track C against production in test mode.
- **Stage 3**: the card-gate scenario by hand in production with test
  cards (steps a to e of `testing.md`); then an admin Grant of ten cents
  on a test account: the low banner, the out-of-credit banner, a long
  `run_js` call started just before the grace ends finishes, the session
  sleeps with a snapshot, create, resume and MCP wake are refused with 402,
  then a test-card pack and the session is wakeable and not awake.
- **Before stage 5**: the restore rehearsal (delete the three kinds on a
  staging cluster, apply the export, run the reconcile, compare; a card
  that had the sign-up credit still cannot earn it).
