#!/usr/bin/env bash
# Pin the three images deploy/gke runs, by digest.
#
#   hack/pin-images.sh backend=sha256:… browser=sha256:… mcp-js=sha256:…
#       writes the given digests (any subset) into the images: block of
#       deploy/gke/kustomization.yaml, then copies the two session images
#       into deploy/gke/blueprint.yaml and deploy/gke/warmpool.yaml
#   hack/pin-images.sh
#       only copies: use it after editing kustomization.yaml by hand
#   hack/pin-images.sh --registry [tag]
#       asks Artifact Registry (gcloud) for the digest each image's tag
#       (default: main) points at now, and pins those
#   hack/pin-images.sh --check
#       changes nothing; fails if a placeholder is left or the files
#       disagree. The deploy workflow runs this first.
#
# The digests are in the summary of the "images" workflow run for the commit.
set -euo pipefail
cd "$(dirname "$0")/.."

kustomization=deploy/gke/kustomization.yaml
# Where the session images are named: a cold session's pod and a warm one's.
blueprints=(deploy/gke/blueprint.yaml deploy/gke/warmpool.yaml)
images=(backend browser mcp-js)

die() {
  echo "pin-images: $*" >&2
  exit 1
}

# The image's entry in the images: block, as "newName digest".
pinned() { # image
  awk -v want="browserjs/$1" '
    $1 == "-" && $2 == "name:" { current = $3 }
    current == want && $1 == "newName:" { name = $2 }
    current == want && $1 == "digest:" { digest = $2 }
    END { if (name == "" || digest == "") exit 1; print name, digest }
  ' "$kustomization" || die "no images: entry for browserjs/$1 in $kustomization"
}

set_digest() { # image, digest
  [[ "$2" =~ ^sha256:[0-9a-f]{64}$ ]] || die "$1: not a digest (sha256: and 64 hex digits): $2"
  pinned "$1" >/dev/null
  awk -v want="browserjs/$1" -v digest="$2" '
    $1 == "-" && $2 == "name:" { current = $3 }
    current == want && $1 == "digest:" { sub(/digest:.*/, "digest: " digest) }
    { print }
  ' "$kustomization" >"$kustomization.tmp"
  mv "$kustomization.tmp" "$kustomization"
}

# The reference a session image has in a blueprint.
in_blueprint() { # image, file
  awk -v suffix="/$1@" '$1 == "image:" && index($2, suffix) { print $2 }' "$2"
}

check=""
case "${1:-}" in
  --check)
    check=1
    ;;
  --registry)
    tag="${2:-main}"
    for image in "${images[@]}"; do
      read -r name _ <<<"$(pinned "$image")"
      digest="$(gcloud artifacts docker images describe "$name:$tag" --format='value(image_summary.digest)')"
      echo "$image:$tag is $digest"
      set_digest "$image" "$digest"
    done
    ;;
  *)
    for arg in "$@"; do
      [[ "$arg" == *=* ]] || die "expected image=digest, got: $arg"
      case "${arg%%=*}" in
        backend | browser | mcp-js) set_digest "${arg%%=*}" "${arg#*=}" ;;
        *) die "unknown image: ${arg%%=*} (one of: ${images[*]})" ;;
      esac
    done
    ;;
esac

failed=""
for image in "${images[@]}"; do
  read -r name digest <<<"$(pinned "$image")"
  if [[ ! "$digest" =~ ^sha256:[0-9a-f]{64}$ ]]; then
    echo "pin-images: $image is not pinned in $kustomization (digest: $digest)" >&2
    failed=1
  fi
  [ "$image" != backend ] || continue
  want="$name@$digest"
  for blueprint in "${blueprints[@]}"; do
    have="$(in_blueprint "$image" "$blueprint")"
    [ "$(wc -l <<<"$have")" -eq 1 ] && [ -n "$have" ] || die "expected one $image image line in $blueprint"
    [ "$have" != "$want" ] || continue
    if [ -n "$check" ]; then
      echo "pin-images: $blueprint has $have, $kustomization says $want: run hack/pin-images.sh" >&2
      failed=1
    else
      awk -v have="$have" -v want="$want" '
        $1 == "image:" && $2 == have { sub(/image:.*/, "image: " want) }
        { print }
      ' "$blueprint" >"$blueprint.tmp"
      mv "$blueprint.tmp" "$blueprint"
      echo "$blueprint: $image is $want"
    fi
  done
done
[ -z "$failed" ] || exit 1
echo "pin-images: ok"
