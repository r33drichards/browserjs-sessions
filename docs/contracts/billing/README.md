# Metering and billing contracts

What the tracks of the metering and billing plan build against. Design:
[../../plans/2026-10-02-metering-billing-design.md](../../plans/2026-10-02-metering-billing-design.md).
Tracks: [../../plans/2026-10-02-metering-billing-tracks.md](../../plans/2026-10-02-metering-billing-tracks.md).

A change to anything here is a change to a contract: make it in its own pull
request, and say which tracks it affects.

**Status: proposed.** The numbers in `catalogue.yaml` and the open questions
of the design's section 11 wait for the product owner; the tracks do not
start before they are answered.

**Changed 2026-10-02: the meter and the credit ledger are Metronome.** The
product owner decided to use Metronome (a Stripe product) in place of the
operator's ledger and its custom resources. Plan:
[../../plans/2026-10-02-metronome-integration.md](../../plans/2026-10-02-metronome-integration.md).
[`metronome.md`](metronome.md) is the contract for it and **wins wherever
another file here disagrees**; each file it changes says so at its top.
In short: usage goes to Metronome as events, credit is a Metronome credit,
the backend keeps no copy of either in memory, and the only custom
resource left is `Account`.

| File | What it fixes | Consumed by |
|---|---|---|
| [`metronome.md`](metronome.md) | **new**: Metronome's objects and who makes them, the usage events, credit and its keys, what the Account records, the webhook, the balance pass, configuration, the sandbox checks | A, B, C, D |
| [`crd-account.yaml`](crd-account.yaml) | the `Account` custom resource, the backend's alone: who a user is to Stripe and to Metronome, whether they have a card, whether they have credit left, the sign-up credit's outcome, auto-recharge. **Changed**: no `status`; `metronomeCustomerId` and `credit` added. | B, C, D |
| [`crd-grant.yaml`](crd-grant.yaml) | **superseded, not deployed**: credit is a Metronome credit | nobody |
| [`crd-usageperiod.yaml`](crd-usageperiod.yaml) | **superseded, not deployed**: usage history is Metronome's | nobody |
| [`catalogue.yaml`](catalogue.yaml) | the plan table as data: rates, the sign-up credit, pay as you go, the three plans, the credit packs, auto-recharge defaults | A, B, C, D, E, F |
| [`metering.md`](metering.md) | the two charges (awake time, disk), what is observed, how seconds are counted, which way errors fall. **Changed**: the money half of the step and the periods are Metronome's. | A, D |
| [`metering-vectors.json`](metering-vectors.json) | 18 cases: the seconds are the observer's contract, the money the fake Metronome's. Unchanged. | A, D (the fake Metronome) |
| [`spike/meter_ref.py`](spike/meter_ref.py) | a reference implementation of the step, and the runner of the vectors | A |
| [`enforcement.md`](enforcement.md) | account states, the decision table, where it is applied, the answers, the stop sequence, disks at zero, the sign-up limits | D, E, G |
| [`stripe.md`](stripe.md) | Stripe objects and lookup keys, calls, Grant keys, saving a card, the sign-up credit, auto-recharge, the webhook's events, reconcile | C |
| [`backend-api.yaml`](backend-api.yaml) | the backend's HTTP API additions | C, D, E |
| [`testing.md`](testing.md) | the Go interfaces and their fakes; the required card-gate scenario with expected statuses and bodies; the other required scenarios | C, D |
| [`ui-states.md`](ui-states.md) | every state the app shows, and its copy | E, F |
| [`deploy.md`](deploy.md) | flags and stages, env vars, secrets, workflows, the operator's workload, the catalogue ConfigMap, RBAC, the Pomerium routes, local development | A, B, C, D, G |
| [`legal-pages.md`](legal-pages.md) | drafts of what the public pages must say (not legal advice) | F, the product owner |

`crd-account.yaml` is written here: track B copies it to `deploy/base/`
unchanged and lists it in `kustomization.yaml`; the other two CRD files
are not copied. Its CEL rules have not been run against an API server;
that is track B's first test, and a rule that does not behave as its
message says is fixed here.

## Checking the vectors

```
python3 docs/contracts/billing/spike/meter_ref.py docs/contracts/billing/metering-vectors.json
18/18 vectors pass
```

The operator's tests run the same file. `--write` regenerates each
vector's `expect` from the reference; what it changes is a contract change.
