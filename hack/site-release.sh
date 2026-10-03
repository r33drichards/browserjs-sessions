#!/usr/bin/env bash
# Release only the public VitePress site; docs/releases.md.
#   hack/site-release.sh deploy <repository>@sha256:…
#   hack/site-release.sh preserve [tree]   keep its running digest in an overlay
# Both run under the Actions concurrency group "deploy", as the full deploy does.
set -euo pipefail
cd "$(dirname "$0")/.."
NS="${NS:-browserjs-sessions}"
k() { kubectl -n "$NS" "$@"; }
die() { echo "site-release: $*" >&2; exit 1; }

case "${1:-}" in
  preserve)
    tree="${2:-.}"
    # --ignore-not-found permits the first cluster deployment. Other API errors fail.
    current="$(k get deployment site --ignore-not-found -o json)"
    [ -n "$current" ] || exit 0
    image="$(jq -r '.spec.template.spec.containers[] | select(.name == "site") | .image' <<<"$current")"
    [[ "$image" =~ ^[^[:space:]]+/site@sha256:[0-9a-f]{64}$ ]] || die "the running site is not pinned: $image"
    file="$tree/deploy/gke/kustomization.yaml"
    awk -v repository="${image%@*}" -v digest="${image##*@}" '
      $1 == "-" && $2 == "name:" { site = ($3 == "browserjs/site") }
      site && $1 == "newName:" { sub(/newName:.*/, "newName: " repository) }
      site && $1 == "digest:" { sub(/digest:.*/, "digest: " digest); found = 1 }
      { print }
      END { if (!found) exit 1 }
    ' "$file" >"$file.tmp" || { rm -f "$file.tmp"; die "no site image in $file"; }
    mv "$file.tmp" "$file"
    echo "Preserving the independently released site: $image"
    ;;
  deploy)
    image="${2:-}"
    repository="$(hack/release.sh pinned | awk '$1 == "site" { sub(/@.*/, "", $2); print $2 }')"
    [ -n "$repository" ] || die "the overlay has no site repository"
    [ "${image%@*}" = "$repository" ] || die "unexpected site repository: $image"
    [[ "${image##*@}" =~ ^sha256:[0-9a-f]{64}$ ]] || die "deploy requires an immutable image digest"
    # A full deploy installs these. Never publish unchecked on an uninitialized cluster.
    k get rollouts.argoproj.io site >/dev/null
    k get analysistemplates.argoproj.io site-answers >/dev/null
    hack/release.sh rollout-status site 600
    previous="$(k get deployment site -o json | jq -r '.spec.template.spec.containers[] | select(.name == "site") | .image')"
    [[ "$previous" =~ ^[^[:space:]]+/site@sha256:[0-9a-f]{64}$ ]] || die "the previous site has no immutable digest"

    promote() {
      k set image deployment/site "site=$image" &&
        hack/release.sh rollout-status site 600 &&
        k get pods -l app=site -o json | jq -e --arg digest "${image##*@}" '
          [.items[] | select(.metadata.deletionTimestamp == null and .status.phase == "Running")] |
          length > 0 and all(.[]; any(.status.containerStatuses[]; .name == "site" and (.imageID | contains($digest))))
        ' >/dev/null &&
        k create configmap site-release --dry-run=client -o yaml \
          --from-literal="image=$image" --from-literal="commit=${GITHUB_SHA:-$(git rev-parse HEAD)}" \
          --from-literal="run=${GITHUB_SERVER_URL:-}/${GITHUB_REPOSITORY:-}/actions/runs/${GITHUB_RUN_ID:-}" |
          kubectl apply -f - >/dev/null
    }
    if promote; then
      echo "Released site: $image"
      [ -z "${GITHUB_STEP_SUMMARY:-}" ] || printf '### Site deployed\n\n`%s`\n' "$image" >>"$GITHUB_STEP_SUMMARY"
    else
      echo "::error::Site release failed; restoring $previous"
      k set image deployment/site "site=$previous"
      hack/release.sh rollout-status site 600 || die "site rollback failed; inspect rollout/site"
      die "the site release failed and the previous image was restored"
    fi
    ;;
  *) die "usage: $0 deploy <image>@sha256:… | preserve [tree]" ;;
esac
