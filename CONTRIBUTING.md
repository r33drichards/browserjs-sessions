# Contributing

Issues and pull requests are welcome. For anything larger than a fix, open an
issue first and say what you want to change: the design notes in `docs/plans/`
show how much is usually settled before code is written.

Security problems are not reported here: see [`SECURITY.md`](SECURITY.md).

## Setting up

Everything runs from the Nix dev shell (`nix develop`); nothing else needs
installing. The whole system on a local kind cluster:
[`docs/local-development.md`](docs/local-development.md).

| Part | Check before you push |
|---|---|
| `backend/` | `go vet ./... && go test ./...` |
| `web/` | `npm ci && npm test && npm run build` |
| `terraform-provider-browserjs/` | `go test ./...` (see its README for the acceptance tests) |
| `images/policy-operator/`, `images/billing-operator/` | `pytest` (each directory's `flake.nix` has the environment) |
| `infra/` | `tofu fmt -check -recursive && (cd main && tofu init -backend=false && tofu validate && tofu test)` |
| `site/` | `npm ci && npm run build` |
| `deploy/` | `kubectl kustomize deploy/local >/dev/null && kubectl kustomize deploy/gke >/dev/null` |

## Pull requests

- One subject per pull request. Say what changes for a user or an operator,
  and how you checked it.
- Behaviour changes come with a test, and with the documentation that
  describes the behaviour (`docs/` for how it is built, `site/` for how it is
  used).
- Write for a reader who has not seen the code: plain sentences, the reason
  in a comment where the code cannot say it.
- `docs/contracts/` is what the components agree on. Change the contract in
  the same pull request as the code on both sides of it.

## What the checks on your pull request do

A pull request from a fork runs the tests and builds the images, with no
credentials: it cannot push an image, read the infrastructure plan or reach
the cluster, and a maintainer has to approve its workflows before they start.
The "infra plan" check therefore fails on a fork at its sign-in step; a
maintainer runs the plan from a branch in this repository when a change to
`infra/` is ready.

Pull requests that change `.github/workflows/` get a slower, closer review.
Third-party actions are pinned to a commit, with the version in a comment.

## Licence

By contributing you agree that your contribution is licensed under the
[Apache License 2.0](LICENSE), as section 5 of the licence says. There is no
contributor agreement to sign.
