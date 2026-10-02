# Metering and billing: tracks

Seven tracks in four stages. Design:
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
  behaves exactly as it does today, and a test in each backend track says
  so. No track changes a default.
- **No track handles a Stripe key.** Tests use fakes of Stripe's API
  (`httptest`) and signatures made with a made-up secret. Anything that
  needs a real sandbox is written as a documented script the product owner
  or a developer runs with their own key.
- One pull request per track (more if it helps review), tests included, no
  `Co-Authored-By` trailer, nothing merged or deployed by the agent.
- Go and Node as the repository already does them; the operator's Python
  through the Nix dev shell.

## Stages

| Stage | Flag | Tracks | Ends when |
|---|---|---|---|
| 1. Meter in shadow | `BILLING=meter`, no Stripe | A, B, D (reading side), E (usage page) | usage shown in production for the two users matches what they did |
| 2. Test payments | + `STRIPE_MODE=test` | C, E (buying), F (pricing page) | a test-card pack and a test-card subscription each grant hours; a test clock renews, fails and cancels correctly |
| 3. Enforce | `BILLING=enforce` | D (enforcing side), E (blocked states) | the two users are stopped at zero and resumed by a purchase; a week without a metering discrepancy |
| 4. Open sign-up | `OPEN_SIGNUP=true` + Pomerium policy | G, F (legal pages) | prerequisites P1 to P10 of the design's section 8.5 are each ticked in the pull request |
| 5. Live payments | `STRIPE_MODE=live` | none: configuration and the product owner's steps 15 to 19 | a real purchase and its refund |

A to F are built at the same time; the stage is which flag is on in
production. G is built last and switched on its own. Stages 4 and 5 do not
depend on each other; the recommended order is 5 then 4 (design 11.18).

## Shared files

| File | Who adds what |
|---|---|
| `backend/cmd/server/main.go`, `backend/internal/config/config.go` | D: `BILLING`, `BILLING_GRACE`, `BILLING_STALE_AFTER`, `BILLING_EXEMPT_EMAILS`, `MAX_AWAKE_SESSIONS`, `FREE_AWAKE_CEILING`, `WAKES_PER_HOUR`, `IDLE_DELETE`, and the wiring of `internal/accounts`. C: `STRIPE_MODE`, `STRIPE_API_KEY`, `STRIPE_WEBHOOK_SECRET`, and the wiring of `internal/stripebilling`. G: `OPEN_SIGNUP`, `SIGNUPS_PER_DAY`, `SIGNUPS_PER_IP_PER_DAY`, `TERMS_VERSION`. Separate blocks; no refactoring of what is there. |
| `backend/internal/api/api.go` | D only: the check before create and resume, `stoppedBy` and `deleteAfter` on the session view. C registers its routes from its own package. |
| `backend/internal/proxy/waker.go` | D only: one call before `Store.Wake`. |
| `backend/internal/sessions/` | D only: `StoppedByBilling`, `Wake` accepting it, the `last-awake` annotation. |
| `backend/go.mod` | C adds `stripe-go`. |
| `deploy/base/backend.yaml` | B: the Role's new rules and every env var of `deploy.md` (with `BILLING` absent). Nobody else. |
| every `pomerium-config.yaml` | B: the `api-stripe-webhook` route. G: the policy change. |
| `.github/workflows/deploy.yml` | B: the `stripe` Secret and the `billing-mode` ConfigMap. |
| `.github/workflows/images.yml`, `hack/pin-images.sh` | B: the operator's image, beside the others, not reordering. |
| `web/src/api.ts`, `App.tsx`, `shell.tsx` | E only. G hands E the terms modal's contract, it does not edit `web/`. |
| `docs/` | Each track adds its own page; nobody edits the design or the contracts. |

The interface between C and D, fixed here so that neither waits:

```go
// Package accounts (track D). The only way the rest of the backend,
// track C included, touches an Account or a Grant.
type Accounts interface {
	// Ensure returns the caller's Account, making it if there is none.
	Ensure(ctx context.Context, owner string) (Account, error)
	ByName(ctx context.Context, name string) (Account, error)
	ByCustomer(ctx context.Context, stripeCustomerID string) (Account, error)
	// SetCustomer writes spec.stripeCustomerId once.
	SetCustomer(ctx context.Context, name, customerID string) error
	SetSubscription(ctx context.Context, name string, sub Subscription) error
	SetBlocked(ctx context.Context, name string, b *Blocked) error
	// EnsureGrant creates the Grant named from g.Key; an existing one is success.
	EnsureGrant(ctx context.Context, g Grant) error
	// Revoke sets spec.revoked on the Grants the selector finds.
	Revoke(ctx context.Context, sel GrantSelector, reason string) error
	// WithCustomer lists the Accounts that have a Stripe customer (reconcile).
	WithCustomer(ctx context.Context) ([]Account, error)
}
```

C builds against a fake of it; D against the dynamic client's fake.

---

## Track A: billing operator

**Owns**: `images/billing-operator/` (all of it: `Dockerfile`, the Python
package `billing_operator`, `tests/`, lock file, flake, in the layout of
`images/policy-operator/`), `docs/billing-operator.md`.

**Consumes**: `metering.md`, `metering-vectors.json`, `spike/meter_ref.py`,
the three CRDs, `catalogue.yaml`, the operator rows of `deploy.md`.

**Builds**

- `step(meter, grants, observed, now)`: the step of `metering.md`, a pure
  function.
- The pass: one list of Sandboxes, grouping by owner label, the billable
  rule, one status update per account with `resourceVersion`, the retry
  and skip rule.
- Free grants (lazily, by month, honouring `FREE_TIER`), `status.plan`,
  period open and close (`UsagePeriod`), pruning of `meter.consumed`, the
  daily retention pass, `Grant.status` for display, the `Metered` and
  `PaymentFailed` conditions.
- Liveness, and a gauge or log line per pass (accounts touched, seconds
  credited, duration) so that a stalled meter is visible.

**Tests**

- Every vector of `metering-vectors.json` through `step`.
- Property tests on `step`: credit never exceeds `now - lastSeen`; never
  negative; the sum taken from grants plus overdraft equals the credit; a
  repeated tick with the same `now` credits nothing; any gap over
  `MAX_GAP` credits at most `MAX_GAP`.
- The pass against a fake API (objects in memory): a conflict is retried
  with the same `now`; three conflicts skip; an owner with no Account is
  logged and skipped; a warm-pool Sandbox is never billed.
- Period close at a month boundary and at a Stripe period boundary; the
  free grant made once per month; no free grant for a subscriber, a blocked
  or a deleted account, or with `FREE_TIER=off`.

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
`crd-usageperiod.yaml` (copied from the contracts), `deploy/base/billing-operator.yaml`
(new), the edits to `deploy/base/kustomization.yaml`, `networkpolicy.yaml`,
`secrets.example.yaml`, `backend.yaml`; the `deploy/gke` and `deploy/local`
overlays; the `api-stripe-webhook` route in every `pomerium-config.yaml`;
the Stripe part of `.github/workflows/deploy.yml`; the operator in
`images.yml` and `hack/pin-images.sh`; the `billing-export` CronJob and,
in `infra/main`, its bucket and Workload Identity binding;
`docs/billing-deployment.md`.

**Consumes**: `deploy.md`, the three CRDs.

**Builds**: everything in `deploy.md` except the code. `BILLING` is absent
from the base and from `deploy/gke`; `deploy/local` sets `meter`.

**Tests**

1. On kind: the three CRDs are accepted; each CEL rule refuses what its
   message says (an Account whose name is not its hash, a changed owner, a
   changed `stripeCustomerId`, a changed Grant, an un-revoked Grant, a
   non-admin Grant with no expiry, a changed UsagePeriod). A rule that does
   not behave is fixed in a contract pull request.
2. RBAC: the backend's ServiceAccount cannot patch `accounts/status`; the
   operator's cannot patch an Account's `spec`, and cannot read Secrets.
3. NetworkPolicy: the operator reaches the API server and nothing else;
   nothing reaches it.
4. `kubectl kustomize deploy/local` and `deploy/gke` render; the deploy
   workflow's Secret step, run with fake values in a kind job, makes the
   Secret and ConfigMap and removes them when `STRIPE_MODE` is empty,
   printing no value.
5. The export CronJob writes a file a second cluster can apply.

**Done when**: the above pass in CI or as documented scripts; with the
operator's image pinned, `deploy/gke` applies with billing still off and
nothing about the running product changes.

**Must not touch**: `backend/` and `web/` code, `images/`, Pomerium's
policy (G), the contracts.

**Fakes it needs**: a placeholder image for the operator until A's exists.

---

## Track C: backend, Stripe

**Owns**: `backend/internal/stripebilling/` (new: the Stripe client, the
checkout, portal and webhook handlers, `ensureSubscription`,
`ensurePurchase`, the reconcile), `backend/cmd/stripe-setup/` (new),
`.github/workflows/stripe-setup.yml` (new), the edits to `main.go`,
`config.go` and `go.mod` named under "Shared files",
`docs/billing-stripe.md`, `docs/billing-development.md`.

**Consumes**: `stripe.md`, `catalogue.yaml`, `backend-api.yaml` (checkout,
portal, webhook), `deploy.md` (configuration, the key's mode check, local
development), the `Accounts` interface above.

**Builds**

- The setup command: products, prices, the portal configuration from
  `catalogue.yaml`; `--apply=false` prints what it would change; a second
  run changes nothing.
- `POST /api/billing/checkout`, `GET /api/billing/checkout/{id}`,
  `POST /api/billing/portal`; cookie only; a limit of 10 checkouts per
  account per day.
- `POST /stripe/webhook` on the API host only: raw body, signature, mode,
  the event table.
- The two ensure functions and the hourly reconcile.
- The Stripe side of account deletion (cancel the subscription), called by
  D's handler.

**First, three checks in a sandbox** (scripts in the pull request, run by
the product owner or a developer with their own key; results written into
`docs/billing-stripe.md`):

1. Through Pomerium locally: `stripe trigger checkout.session.completed`
   reaches the handler with a signature that verifies (the body and header
   are untouched).
2. With a test clock: subscribe, advance a month (a new plan Grant),
   upgrade in the portal (new period, old Grant superseded, charged at
   once), downgrade (waits), fail a renewal with the declining test card
   (`past_due`, no Grant), cancel (free at the period's end).
3. The restricted key: the smallest set of permissions with which
   everything above works, by name, into `deploy.md`.

**Tests**: with an `httptest` Stripe and the fake `Accounts`: every row of
the event table; each event twice; events in reversed order; a bad
signature, an old timestamp, the wrong mode; a checkout session that
belongs to another account; subscribe while subscribed; the reconcile
re-making Grants after they are deleted; the setup command against the
fake, twice. A live key with `STRIPE_MODE=test` refuses to start, and the
error does not contain the key.

**Done when**: `go test ./...` passes; the handlers match
`backend-api.yaml`; the three sandbox checks are recorded.

**Must not touch**: `backend/internal/accounts` (D), `api.go`, `proxy/`,
`sessions/`, `web/`, `deploy/`.

**Fakes it needs**: Stripe (`httptest`), `Accounts`.

---

## Track D: backend, accounts and enforcement

**Owns**: `backend/internal/accounts/` (new: the Account and Grant client
over an informer, the `Accounts` interface, the decision function, the
billing sweep, the idle-delete pass, `GET /api/billing`,
`/api/billing/usage`, `/api/billing/catalogue`, `DELETE /api/account`), the
edits to `api.go`, `proxy/waker.go`, `sessions/`, `main.go` and `config.go`
named under "Shared files", `docs/billing-enforcement.md`.

**Consumes**: `enforcement.md`, `backend-api.yaml`, the three CRDs,
`catalogue.yaml`, `metering.md` (to read `status`, never to compute it),
`deploy.md`.

**Builds**

- Stage 1: Accounts made on first sight; the three read routes; the
  decision function computed and logged as `would_refuse`.
- Stage 3: the decision table applied at create, resume and wake; the
  answers, including the MCP form; the sweep (`stopped-by: billing`, grace
  from `exhaustedAt`); the wake rate limit; the cluster caps.
- Account deletion; the idle-delete pass (behind `IDLE_DELETE`).

**First**: what Claude's MCP clients show for a 402 and a 503 from the
session endpoint, recorded; the answer form in `enforcement.md` is
confirmed or changed by a contract pull request.

**Tests**: the decision function as a table test, one case per row of the
decision table and per column, plus the order of the rows; `BILLING` off
gives today's responses byte for byte; `meter` refuses nothing; an admin
waking a user's session is judged on the owner's account; a token is
judged as its owner; the sweep sleeps with a snapshot, retries a failure,
does nothing when stale, and never touches an exempt account; a purchase
inside the grace prevents the sleep; reading, stopping and deleting work
at zero.

**Done when**: `go test ./...` passes; the read routes match
`backend-api.yaml` field for field.

**Must not touch**: `backend/internal/stripebilling` (C),
`backend/internal/auth`, `backend/internal/tokens` (G), `web/`, `deploy/`.

**Fakes it needs**: the dynamic client's fake for the three kinds, with
`status` written by the test in place of the operator.

---

## Track E: UI

**Owns**: `web/` (the `/billing` page, the utility in the top navigation,
the banners, the buy modal, the blocked states on create, list and session
pages, the checkout return, the delete-account modal, the terms modal;
`billingApi.ts`).

**Consumes**: `backend-api.yaml`, `ui-states.md`, the design's section 6.

**Builds**: every row of the states table of `ui-states.md`. The page and
the navigation item do not exist when `GET /api/billing` answers 404.

**Tests**: vitest for the client and for the formatting (used rounded
down, left rounded up, to the minute); a component test per state against
a mock of `backend-api.yaml`; the create form disabled with the right
alert for each reason; the checkout return's polling with a fake timer;
the existing tests keep passing.

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

**Consumes**: `catalogue.yaml`, `ui-states.md` (the pricing section),
`legal-pages.md`.

**Builds**: the pricing page from `catalogue.yaml` at build time (enabled
items only); the four legal pages as **drafts marked as drafts**, from the
points of `legal-pages.md`, with placeholders for the legal name, address
and email addresses the product owner supplies.

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
that is not blocked"), the sign-up limits and the terms routes
(`POST /api/billing/terms`) in a new file of `backend/internal/accounts`
agreed with D, the policy change in every `pomerium-config.yaml`, the two
blocked ports in `deploy/base/networkpolicy.yaml`, `docs/open-signup.md`
(the runbook: how to open, how to close again, how to block an account,
how to turn the free tier off).

**Consumes**: `enforcement.md` ("Rate of new accounts"), `deploy.md`
(stage 4, the Pomerium policy), `backend-api.yaml` (terms), the design's
section 8.

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
pass untouched); with it on, a new address gets an Account and a token; a
blocked account's token is refused on the next request; the 51st sign-up
of a day and the 4th from one address are refused with `signups_paused`
and make no Account; a deleted account signing up again in the same month
gets no free Grant.

**Done when**: the tests pass, the runbook is written, and the pull
request's description ticks P1 to P10 with a link to the evidence for
each. The product owner merges and deploys it; no agent does.

**Must not touch**: the metering, Stripe and UI code of A, C and E.

**Fakes it needs**: none beyond D's.

---

## After the tracks: integration

- **Stage 1** (A, B, D reading, E usage page merged): on kind, a session
  awake for ten minutes shows nine to ten minutes used; the operator
  killed for five minutes loses those minutes and nothing else; a warm pod
  is never counted. Then production in `meter` for a week, compared by
  hand with what the two users did.
- **Stage 2** (C merged, the product owner's steps 1 to 9 done): the
  sandbox checks of track C against production in test mode.
- **Stage 3**: an admin Grant of ten minutes on a test account: the low
  banner, the out-of-hours banner, the sleep with a snapshot five minutes
  after zero, refused create, resume and MCP wake, then a test-card pack
  and everything wakes.
- **Before stage 4**: the restore rehearsal (delete the three kinds on a
  staging cluster, apply the export, run the reconcile, compare).
