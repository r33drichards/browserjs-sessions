# Image build pipeline

`.github/workflows/images.yml` builds the four container images and pushes
them to Artifact Registry. No key is stored anywhere: the push job exchanges
GitHub's OIDC token for the `images-push` service account (Workload Identity
Federation), which can write to the one image repository and nothing else, and
which Google hands only to workflows running on `refs/heads/main`.

## Images

| Image | Build context | Dockerfile | Rebuilt when these change |
|---|---|---|---|
| `backend` | repository root | `Dockerfile` | `Dockerfile`, `.dockerignore`, `backend/**`, `web/**` |
| `mcp-js` | `images/mcp-js` | `images/mcp-js/Dockerfile` | `images/mcp-js/**` |
| `browser` | `images/browser` | `images/browser/Dockerfile` | `images/browser/**` |
| `site` | `site` | `site/Dockerfile` | `site/**` |

All are built for `linux/amd64` only, which is what the cluster's nodes are.

A change to `images.yml` itself rebuilds all four.

`site` is the public site (landing page, docs, blog): VitePress builds static
files and nginx serves them. It is not a session image and is in no blueprint.

The root `Dockerfile` does not exist yet. Until it does, `backend` is skipped
with a notice in the run, not failed.

## What triggers what

| Event | What happens |
|---|---|
| Pull request touching the paths above | The affected images are built. No sign-in, no push. |
| Push to `main` touching the paths above | The affected images are built and pushed. |
| Run by hand (Actions, **images**, Run workflow) on `main` | The chosen image, or all, is built and pushed. |
| Run by hand on any other branch | Build only. |

Pull requests from forks build too: the build job needs no credentials.

Pushes to `main` queue; they do not cancel each other, so every commit that
changes an image gets its own image.

## Where images go

```
us-west1-docker.pkg.dev/browserjs-sessions/browserjs/<name>:<full commit sha>
us-west1-docker.pkg.dev/browserjs-sessions/browserjs/<name>:main
```

The prefix is the repository variable `IMAGE_REGISTRY`. `:main` moves with
every push; the commit tag does not. An image is only pushed for commits that
changed it, so not every commit has a tag for every image.

## One-time setup

1. Merge and apply the `infra/main` change that creates the `images-push`
   service account (pull request, read the plan, merge; **infra apply** then runs by itself).
2. From the apply run's outputs, set two repository variables:

   ```sh
   gh variable set IMAGE_PUSH_SA  --body "<images_push_service_account_email>"
   gh variable set IMAGE_REGISTRY --body "<registry_url>"
   ```

   `GCP_WIF_PROVIDER` is already set (the bootstrap).
3. Run **images** by hand on `main` to push the first set.

Until the variables are set, the publish job stops at its first step and says
which one is missing.

## Finding a digest

The Kubernetes manifests pin images by digest, not by tag.

- Each publish job writes the full reference to its run summary:
  `…/browserjs/<name>@sha256:…`.
- From a commit:

  ```sh
  gcloud artifacts docker images describe \
    us-west1-docker.pkg.dev/browserjs-sessions/browserjs/<name>:<commit sha> \
    --format='value(image_summary.digest)'
  ```

- Or the console: Artifact Registry, `browserjs`, the image, its tags.

## Rolling one out

1. Take the digest reference from the run summary.
2. Pin it in the deploy overlay, for example with kustomize:

   ```yaml
   images:
     - name: backend            # the name the base manifests use
       newName: us-west1-docker.pkg.dev/browserjs-sessions/browserjs/backend
       digest: sha256:…
   ```

   The session images (`browser`, `mcp-js`) are named in the Sandbox template;
   pin them there the same way. `hack/pin-images.sh` does all of this:
   `hack/pin-images.sh site=sha256:…` pins one image.
3. Commit, review, apply the overlay to the cluster.

Running sessions keep the image they started with. A session picks up a new
`browser` or `mcp-js` image the next time its pod is created, and a Pod
Snapshot taken with the old image will not restore onto the new one (the pod
spec no longer matches), so that session starts cold once.

Nodes pull with their own service account, which can read this repository; no
`imagePullSecrets` are needed.

## Things to know

- **The registry deletes old images.** `infra/main/registry.tf` removes a
  version once it is older than 30 days and no longer among the 10 most recent
  of that image. A digest pinned in the cluster is not exempt. Roll forward
  before that, or raise `registry_keep_versions` /
  `registry_delete_older_than_days`. A node that has the image cached keeps
  working; a new node cannot pull it.
- **The browser image is slow and large**: a Nix build producing about 3.6 GB
  in one layer. Its job frees disk on the runner first, has a two-hour limit,
  and uses no layer cache (the one layer changes whenever its inputs do, so a
  cache would cost minutes and gigabytes per run and never be hit). Every
  browser build is a full build.
- **`backend`, `mcp-js` and `site` use the GitHub Actions layer cache**, one scope per
  image. Pull requests read `main`'s cache.
- **The image is built before signing in** to Google, then pushed in a second
  step that reuses the build. The access token lasts one hour and the browser
  build may take longer.

## Not covered

- Image signing and provenance attestations (provenance is switched off so
  that each tag is one plain image manifest with one digest).
- Vulnerability scanning.
- `arm64` or multi-architecture images.
- Running tests. The workflow only builds.
- Deploying. Nothing here touches the cluster.
- Tags for releases; only commit and `main` tags exist.

## Not verified

The workflow has not run. Unknown until it does:

- How long the browser build takes on a hosted runner and whether the freed
  disk is enough.
- That `mcp-js` pulls its Docker Hub base image without hitting the anonymous
  pull limit that hosted runners share.
- The sign-in, which depends on the `images-push` service account that the
  next apply creates.
