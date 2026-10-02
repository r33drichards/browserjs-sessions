# Metering and billing contracts

What the tracks of the metering and billing plan build against. Design:
[../../plans/2026-10-02-metering-billing-design.md](../../plans/2026-10-02-metering-billing-design.md).
Tracks: [../../plans/2026-10-02-metering-billing-tracks.md](../../plans/2026-10-02-metering-billing-tracks.md).

A change to anything here is a change to a contract: make it in its own pull
request, and say which tracks it affects.

**Status: proposed.** The numbers in `catalogue.yaml` and the open questions
of the design's section 11 wait for the product owner; the tracks do not
start before they are answered.

| File | What it fixes | Consumed by |
|---|---|---|
| [`crd-account.yaml`](crd-account.yaml) | the `Account` custom resource: who a user is to Stripe, whether they have a card, the sign-up credit's outcome, auto-recharge (spec, the backend's) and their ledger (status, the operator's) | A, B, C, D |
| [`crd-grant.yaml`](crd-grant.yaml) | the `Grant` custom resource: credit given to an account, named so that it cannot be made twice | A, B, C, D |
| [`crd-usageperiod.yaml`](crd-usageperiod.yaml) | the `UsagePeriod` custom resource: a closed period's record | A, B, D |
| [`catalogue.yaml`](catalogue.yaml) | the plan table as data: rates, the sign-up credit, pay as you go, the three plans, the credit packs, auto-recharge defaults | A, B, C, D, E, F |
| [`metering.md`](metering.md) | the two charges (awake time, disk), the tick, the step, periods, which way errors fall | A, D |
| [`metering-vectors.json`](metering-vectors.json) | 18 cases the step must answer the same way | A, D (the fake ledger) |
| [`spike/meter_ref.py`](spike/meter_ref.py) | a reference implementation of the step, and the runner of the vectors | A |
| [`enforcement.md`](enforcement.md) | account states, the decision table, where it is applied, the answers, the stop sequence, disks at zero, the sign-up limits | D, E, G |
| [`stripe.md`](stripe.md) | Stripe objects and lookup keys, calls, Grant keys, saving a card, the sign-up credit, auto-recharge, the webhook's events, reconcile | C |
| [`backend-api.yaml`](backend-api.yaml) | the backend's HTTP API additions | C, D, E |
| [`testing.md`](testing.md) | the Go interfaces and their fakes; the required card-gate scenario with expected statuses and bodies; the other required scenarios | C, D |
| [`ui-states.md`](ui-states.md) | every state the app shows, and its copy | E, F |
| [`deploy.md`](deploy.md) | flags and stages, env vars, secrets, workflows, the operator's workload, the catalogue ConfigMap, RBAC, the Pomerium routes, local development | A, B, C, D, G |
| [`legal-pages.md`](legal-pages.md) | drafts of what the public pages must say (not legal advice) | F, the product owner |

The three CRD files are written here and not yet in `deploy/base/`: track B
copies them there unchanged and lists them in `kustomization.yaml`. Their
CEL rules have not been run against an API server; that is track B's first
test, and a rule that does not behave as its message says is fixed here.

## Checking the vectors

```
python3 docs/contracts/billing/spike/meter_ref.py docs/contracts/billing/metering-vectors.json
18/18 vectors pass
```

The operator's tests run the same file. `--write` regenerates each
vector's `expect` from the reference; what it changes is a contract change.
