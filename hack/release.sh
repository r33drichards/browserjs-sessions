#!/usr/bin/env bash
# A release, and its parts. The flow, what the canary catches and what it
# does not: docs/releases.md.
#
# The whole thing, from a checkout with gh signed in (and gcloud, for the
# digests):
#
#   hack/release.sh                 pins the digests the registry's "main" tags
#                                   point at, opens the pull request, waits for
#                                   its checks, merges it, starts the deploy
#                                   workflow and follows it to its verdict
#   hack/release.sh backend=sha256:… site=sha256:…
#                                   the same, with the digests given
#   hack/release.sh --no-pin        releases main as it is (a change to the
#                                   manifests with no new image)
#   DRY_RUN=1 hack/release.sh …     pins and shows the change, and stops
#
# The parts, which .github/workflows/deploy.yml runs against the cluster of
# the current kubectl context (read-only, but for record and rollback):
#
#   hack/release.sh pinned [tree]           the digests a tree pins
#   hack/release.sh changes                 running against pinned, as a table;
#                                           also previous=, session_images= and
#                                           changed= lines for $GITHUB_OUTPUT
#   hack/release.sh rollout-status <name> [seconds]
#                                           waits for the Rollout of the backend
#                                           or the site: promoted, or given up on
#                                           (then it says why, and fails)
#   hack/release.sh verify-deployments [tree]
#                                           every workload runs what is pinned
#   hack/release.sh verify-session          the pod of $SESSION_ID runs the
#                                           session images in $EXPECT_IMAGES
#                                           (default: the pinned ones)
#   hack/release.sh wait-warm-pool          until the warm pool's pods are the
#                                           pinned images, and one is ready
#   hack/release.sh mint-token <file>       an API token for this run's canary:
#                                           written to <file> (mode 600), its
#                                           record to the cluster, its name to
#                                           stdout. An hour, the five scopes of
#                                           the canary, owned by the first admin
#                                           of the running backend who may use
#                                           tokens. Nobody enters it,
#                                           and nothing prints it or the owner
#   hack/release.sh revoke-token <name>     deletes that record: the token and
#                                           every access token made from it are
#                                           refused from the next request
#   hack/release.sh record                  this commit and its digests become
#                                           the last good release
#   hack/release.sh rollback <commit>       applies that commit's deploy/gke
#                                           and waits for it
set -euo pipefail
cd "$(dirname "$0")/.."

NS="${NS:-browserjs-sessions}"
# Where the last good release is written down: in the cluster it describes.
RECORD=release
deployments=(backend site policy-operator billing-operator)
session_images=(browser mcp-js)

die() {
  echo "release: $*" >&2
  exit 1
}
k() { kubectl -n "$NS" "$@"; }

# "<image> <reference>" for every image a tree pins (deploy/gke's images:).
pinned() { # [tree]
  awk '
    $1 == "-" && $2 == "name:" { name = $3; sub(/^browserjs\//, "", name) }
    $1 == "newName:" { repository = $2 }
    $1 == "digest:" { print name, repository "@" $2 }
  ' "${1:-.}/deploy/gke/kustomization.yaml"
}
reference() { # image, [tree]
  pinned "${2:-.}" | awk -v want="$1" '$1 == want { print $2 }'
}

# What runs now, as far as the objects say: a Deployment's image, or the
# warm pool's template's.
running() { # image
  case "$1" in
    browser | mcp-js)
      k get sandboxtemplates.extensions.agents.x-k8s.io session -o json 2>/dev/null |
        jq -r --arg c "$1" '.spec.podTemplate.spec.containers[] | select(.name == $c) | .image'
      ;;
    *) k get deployment "$1" -o json 2>/dev/null | jq -r '.spec.template.spec.containers[0].image' ;;
  esac
}

previous() { k get configmap "$RECORD" -o jsonpath='{.data.commit}' 2>/dev/null || true; }

case "${1:-}" in
  pinned)
    pinned "${2:-.}"
    ;;

  changes)
    previous="$(previous)"
    changed="" sessions_changed=""
    echo "| Image | Running | Pinned | |"
    echo "|---|---|---|---|"
    while read -r image want; do
      have="$(running "$image")"
      if [ "$have" = "$want" ]; then verdict="same"; else
        verdict="**changes**"
        changed="$changed $image"
        [[ " ${session_images[*]} " != *" $image "* ]] || sessions_changed="$sessions_changed $image"
      fi
      echo "| \`$image\` | \`${have##*@}\` | \`${want##*@}\` | $verdict |"
    done < <(pinned)
    echo
    if [ -n "$previous" ]; then
      echo "Last good release: \`$previous\` ($(k get configmap "$RECORD" -o jsonpath='{.data.time}'))."
    else
      echo "No release is on record in this cluster: there is nothing to roll back to, and the session images are not tried on a canary session first (the running backend may predate it)."
    fi
    [ -n "$changed" ] || echo "**No image changes.** If one was meant to, the pins in \`deploy/gke/kustomization.yaml\` are not the new digests."
    if [ -n "${GITHUB_OUTPUT:-}" ]; then
      {
        echo "previous=$previous"
        echo "changed=${changed# }"
        echo "session_images=${sessions_changed# }"
        # For the canary session: container=digest of what is pinned.
        echo "canary_digests=$(for image in "${session_images[@]}"; do printf '%s=%s,' "$image" "$(reference "$image" | sed 's/.*@//')"; done | sed 's/,$//')"
      } >>"$GITHUB_OUTPUT"
    fi
    ;;

  rollout-status)
    name="${2:-}"
    [ -n "$name" ] || die "rollout-status takes the Rollout's name"
    if ! k get rollouts.argoproj.io "$name" >/dev/null 2>&1; then
      # Not released by a Rollout here (a cluster without them): the Deployment.
      k rollout status "deployment/$name" --timeout="${3:-600}s"
      exit
    fi
    deadline=$((SECONDS + ${3:-600}))
    last=""
    while :; do
      json="$(k get rollouts.argoproj.io "$name" -o json)"
      # The Rollout has seen the Deployment's template as it now is.
      seen="$(jq -r '.status.workloadObservedGeneration // ""' <<<"$json")"
      want="$(k get deployment "$name" -o jsonpath='{.metadata.generation}')"
      phase="$(jq -r '.status.phase // "Unknown"' <<<"$json")"
      line="rollout/$name: $phase$(jq -r 'if .status.message then " (" + .status.message + ")" else "" end' <<<"$json")"
      [ "$line" = "$last" ] || echo "$line"
      last="$line"
      if [ "$seen" = "$want" ]; then
        case "$phase" in
          Healthy)
            # Promoted: the pods users reach are the new template's.
            if [ "$(jq -r '.status.stableRS == .status.currentPodHash' <<<"$json")" = true ]; then exit 0; fi
            ;;
          Degraded)
            echo "rollout/$name was given up on: the new version was not promoted, and what served before still serves"
            k get analysisruns.argoproj.io -l "rollouts-pod-template-hash=$(jq -r '.status.currentPodHash' <<<"$json")" \
              -o 'custom-columns=ANALYSIS:.metadata.name,PHASE:.status.phase,MESSAGE:.status.message' 2>/dev/null || true
            # What the check said. Its output has no token and no address
            # in it (test/canary.py).
            for job in $(k get jobs -l app=release-canary --sort-by=.metadata.creationTimestamp -o name 2>/dev/null | tail -1); do
              k logs "$job" --tail=80 2>/dev/null || true
            done
            exit 1
            ;;
        esac
      fi
      [ "$SECONDS" -lt "$deadline" ] || die "rollout/$name is still $phase after ${3:-600}s"
      sleep 5
    done
    ;;

  verify-deployments)
    tree="${2:-.}"
    failed=""
    for deployment in "${deployments[@]}"; do
      want="$(reference "$deployment" "$tree")"
      [ -n "$want" ] || die "$tree pins no image for $deployment"
      json="$(k get deployment "$deployment" -o json)" || die "no deployment/$deployment"
      have="$(jq -r '.spec.template.spec.containers[0].image' <<<"$json")"
      replicas="$(jq -r '.spec.replicas' <<<"$json")"
      if rollout="$(k get rollouts.argoproj.io "$deployment" -o json 2>/dev/null)"; then
        # Its pods are the Rollout's, made from the Deployment's template.
        if [ "$have" != "$want" ]; then
          echo "FAIL  $deployment is to run ${have##*@}, not the pinned ${want##*@}"
          failed=1
          continue
        fi
        if [ "$(jq -r '.status.phase == "Healthy" and .status.stableRS == .status.currentPodHash' <<<"$rollout")" != true ]; then
          echo "FAIL  $deployment's rollout is $(jq -r '.status.phase' <<<"$rollout"): the pinned version is not what serves ($(jq -r '.status.message // "no message"' <<<"$rollout"))"
          failed=1
          continue
        fi
        digest="${want##*@}"
        pods="$(k get pods -l "app=$deployment" -o json | jq -r --arg d "$digest" '
          [.items[] | select(.metadata.deletionTimestamp == null) | select(.status.phase == "Running")] |
          "\(length) \([.[] | .status.containerStatuses[0].imageID | select(contains($d) | not)] | unique | join(" "))"')"
        if [ "${pods%% *}" = 0 ] || [ -n "${pods#* }" ]; then
          echo "FAIL  $deployment has ${pods%% *} pods running, some not the pinned $digest: ${pods#* }"
          failed=1
          continue
        fi
        echo "ok    $deployment runs the pinned $digest (${pods%% *} pods, released by its Rollout)"
        continue
      fi
      if [ "$replicas" = 0 ]; then
        echo "ok    $deployment has no pods (its feature is off)"
        continue
      fi
      if [ "$have" != "$want" ]; then
        echo "FAIL  $deployment is to run ${have##*@}, not the pinned ${want##*@}"
        failed=1
        continue
      fi
      # Rolled out: the pods there are, are the new ones, and they are ready.
      if [ "$(jq -r '[.status.observedGeneration == .metadata.generation, .status.updatedReplicas == .spec.replicas,
          .status.readyReplicas == .spec.replicas, (.status.replicas // 0) == .spec.replicas] | all' <<<"$json")" != true ]; then
        echo "FAIL  $deployment has not finished rolling out: $(jq -c '.status | {replicas, updatedReplicas, readyReplicas}' <<<"$json")"
        failed=1
        continue
      fi
      # What the kubelet says it pulled, for the pods that serve.
      digest="${want##*@}"
      others="$(k get pods -l "app=$deployment" -o json | jq -r --arg d "$digest" '
        [.items[] | select(.metadata.deletionTimestamp == null) | select(.status.phase == "Running") |
         .status.containerStatuses[0].imageID | select(contains($d) | not)] | unique | join(" ")')"
      if [ -n "$others" ]; then
        echo "FAIL  $deployment has a pod running $others, not the pinned $digest"
        failed=1
        continue
      fi
      echo "ok    $deployment runs the pinned $digest"
    done
    [ -z "$failed" ]
    ;;

  verify-session)
    [ -n "${SESSION_ID:-}" ] || die "SESSION_ID is not set"
    expected="${EXPECT_IMAGES:-}"
    if [ -z "$expected" ]; then
      for image in "${session_images[@]}"; do expected="$expected,$image=$(reference "$image")"; done
    fi
    pod="$(k get pod "$SESSION_ID" -o json)" || die "session $SESSION_ID has no pod"
    failed=""
    IFS=, read -ra pairs <<<"${expected#,}"
    for pair in "${pairs[@]}"; do
      container="${pair%%=*}" want="${pair#*=}"
      have="$(jq -r --arg c "$container" '.spec.containers[] | select(.name == $c) | .image' <<<"$pod")"
      pulled="$(jq -r --arg c "$container" '.status.containerStatuses[] | select(.name == $c) | .imageID' <<<"$pod")"
      if [ "$have" != "$want" ]; then
        echo "FAIL  $SESSION_ID's $container is ${have##*/}, not ${want##*/}"
        failed=1
      elif [[ "$want" == *@sha256:* ]] && [[ "$pulled" != *"${want##*@}"* ]]; then
        echo "FAIL  $SESSION_ID's $container was pulled as ${pulled##*@}, not ${want##*@}"
        failed=1
      else
        echo "ok    $SESSION_ID's $container is ${want##*/}"
      fi
    done
    echo "      on node $(jq -r '.spec.nodeName' <<<"$pod"), from $(k get sandboxes.agents.x-k8s.io "$SESSION_ID" -o json |
      jq -r 'if .metadata.annotations["browserjs.dev/canary"] then "the blueprint, as a canary" elif ((.metadata.ownerReferences // [])[0].kind == "SandboxClaim") then "the warm pool" else "the blueprint (cold)" end')"
    [ -z "$failed" ]
    ;;

  wait-warm-pool)
    if ! k get sandboxwarmpools.extensions.agents.x-k8s.io s >/dev/null 2>&1; then
      echo "no warm pool here: nothing to wait for"
      exit 0
    fi
    browser="$(reference browser)" mcpjs="$(reference mcp-js)"
    deadline=$((SECONDS + ${WARM_POOL_TIMEOUT:-900}))
    while :; do
      # The pool's own Sandboxes: the ones nobody has claimed yet.
      state="$(k get sandboxes.agents.x-k8s.io -o json | jq -r --arg b "$browser" --arg m "$mcpjs" '
        [.items[] | select((.metadata.ownerReferences // [])[0].kind == "SandboxWarmPool")] as $warm |
        [$warm[] | select(([.spec.podTemplate.spec.containers[] | select(.name == "browser" or .name == "mcp-js") | .image] | sort) != ([$b, $m] | sort))] as $old |
        "\($warm | length) \($old | length) \([$warm[] | select(([.spec.podTemplate.spec.containers[] | .image] | index($b)) != null) | .metadata.name] | join(" "))"')"
      read -r total old names <<<"$state"
      ready=0
      for name in $names; do
        [ "$(k get pod "$name" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null)" != True ] || ready=$((ready + 1))
      done
      echo "warm pool: $total waiting, $old still on other images, $ready ready on the pinned ones"
      if [ "$old" = 0 ] && [ "$ready" -ge 1 ]; then exit 0; fi
      [ "$SECONDS" -lt "$deadline" ] || die "the warm pool did not come up on the pinned images in ${WARM_POOL_TIMEOUT:-900}s"
      sleep 15
    done
    ;;

  mint-token)
    file="${2:-}"
    [ -n "$file" ] || die "mint-token takes the file to write the token to"
    # The backend's own lists, as it runs them.
    deployment="$(k get deployment backend -o json)" || die "no deployment/backend to take the admins from"
    printf '%s' "$deployment" | ( umask 077 && python3 -c '
import datetime, hashlib, json, secrets, string, sys
deployment = json.loads(sys.stdin.read() or "{}")
env = {e["name"]: e.get("value", "") for c in deployment.get("spec", {}).get("template", {}).get("spec", {}).get("containers", []) for e in c.get("env", [])}
split = lambda v: [a.strip().lower() for a in (v or "").split(",") if a.strip()]
admins, allowed = split(env.get("ADMIN_EMAILS")), split(env.get("ALLOWED_EMAILS"))
owners = [a for a in admins if a in allowed]
if not owners:
    sys.exit("release: no admin of the backend (ADMIN_EMAILS) may use API tokens (ALLOWED_EMAILS): the canary has nobody to act as")
owner = owners[0]
# As backend/internal/tokens makes one: bjs_<12 of base32, lower case>_<32 random bytes, base64url>.
token_id = "".join(secrets.choice("abcdefghijklmnopqrstuvwxyz234567") for _ in range(12))
secret = "".join(secrets.choice(string.ascii_letters + string.digits + "-_") for _ in range(43))
token = "bjs_%s_%s" % (token_id, secret)
expires = (datetime.datetime.now(datetime.timezone.utc) + datetime.timedelta(hours=1)).strftime("%Y-%m-%dT%H:%M:%SZ")
with open(sys.argv[1], "w") as f:
    f.write(token)
print(json.dumps({
    "apiVersion": "browserjs.dev/v1alpha1", "kind": "APIToken",
    "metadata": {"name": "tok-" + token_id, "labels": {"browserjs.dev/owner": hashlib.sha256(owner.encode()).hexdigest()[:32],
                                                        "app.kubernetes.io/managed-by": "release-canary"}},
    "spec": {"id": token_id, "owner": owner, "name": "release-canary",
             "scopes": ["sessions:read", "sessions:write", "sessions:connect", "policies:read", "policies:write"],
             "expiresAt": expires, "sha256": hashlib.sha256(token.encode()).hexdigest()},
}))' "$file" ) >"$file.record" || { rm -f "$file" "$file.record"; exit 1; }
    k apply -f "$file.record" >/dev/null || { rm -f "$file" "$file.record"; die "the token's record was refused"; }
    jq -r '.metadata.name' "$file.record"
    rm -f "$file.record"
    ;;

  revoke-token)
    [ -n "${2:-}" ] || die "revoke-token takes the record's name (tok-...)"
    k delete apitokens.browserjs.dev "$2" --ignore-not-found >/dev/null
    # And any a run that was killed left behind: an hour old at most anyway.
    k delete apitokens.browserjs.dev -l app.kubernetes.io/managed-by=release-canary --ignore-not-found >/dev/null
    echo "revoked $2"
    ;;

  record)
    commit="${GITHUB_SHA:-$(git rev-parse HEAD)}"
    k create configmap "$RECORD" --dry-run=client -o yaml \
      --from-literal=commit="$commit" \
      --from-literal=time="$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
      --from-literal=run="${GITHUB_SERVER_URL:-}/${GITHUB_REPOSITORY:-}/actions/runs/${GITHUB_RUN_ID:-}" \
      --from-literal=images="$(pinned)" | kubectl apply -f - >/dev/null
    echo "configmap/$RECORD: the last good release is $commit"
    ;;

  rollback)
    commit="${2:-}"
    [[ "$commit" =~ ^[0-9a-f]{40}$ ]] || die "rollback takes the commit to go back to (40 hex digits), as configmap/$RECORD has it"
    git cat-file -e "$commit^{commit}" 2>/dev/null || git fetch --quiet --depth=1 origin "$commit" ||
      die "commit $commit cannot be fetched"
    tree="$(mktemp -d)"
    # That commit's manifests, the contract files they are made from, and
    # its own checks: nothing of the commit that failed.
    git archive "$commit" deploy docs/contracts hack | tar -x -C "$tree"
    echo "rolling back to $commit:"
    pinned "$tree" | sed 's/^/  /'
    (cd "$tree" && hack/pin-images.sh --check)
    kubectl apply -k "$tree/deploy/gke"
    failed=""
    for deployment in policy-operator opa billing-operator; do
      k rollout status "deployment/$deployment" --timeout=300s || failed=1
    done
    for rollout in backend site; do
      "$0" rollout-status "$rollout" 900 || failed=1
    done
    "$0" verify-deployments "$tree" || failed=1
    rm -rf "$tree"
    [ -z "$failed" ] || die "the rollback to $commit did not come up: the cluster needs a person (docs/releases.md)"
    echo "rolled back to $commit"
    ;;

  "" | --no-pin | *=sha256:*)
    # The whole release, from a checkout.
    command -v gh >/dev/null || die "gh is needed"
    [ -z "$(git status --porcelain)" ] || die "the working tree has changes: commit or stash them first"
    git fetch --quiet origin main
    pin=1
    [ "${1:-}" != --no-pin ] || pin=""
    if [ -n "$pin" ]; then
      branch="release-$(date -u +%Y%m%d-%H%M%S)"
      git checkout --quiet -b "$branch" origin/main
      before="$(pinned)"
      if [ $# -gt 0 ]; then hack/pin-images.sh "$@"; else hack/pin-images.sh --registry; fi
      after="$(pinned)"
      if [ "$before" = "$after" ]; then
        git checkout --quiet - && git branch --quiet -D "$branch"
        die "nothing to pin: deploy/gke already has these digests. To release main as it is: hack/release.sh --no-pin"
      fi
      # The lines of "after" that "before" has not: what was pinned.
      news="$(comm -13 <(sort <<<"$before") <(sort <<<"$after"))"
      echo "pinned:"
      sed 's/^/  /' <<<"$news"
      git commit --quiet -am "deploy: pin images ($(awk '{ print $1 }' <<<"$news" | paste -sd, - | sed 's/,/, /g'))"
      if [ -n "${DRY_RUN:-}" ]; then
        echo "DRY_RUN: the commit is on the local branch $branch; nothing was pushed"
        exit 0
      fi
      git push --quiet -u origin "$branch"
      url="$(gh pr create --base main --head "$branch" --fill)"
      echo "pull request: $url"
      # The pre-merge gate: every check of the pull request, the canary on
      # kind among them.
      sleep 20
      gh pr checks "$url" --watch --interval 30 || die "the pull request's checks failed: nothing was merged or deployed ($url)"
      gh pr merge "$url" --squash --delete-branch
      git checkout --quiet -
    elif [ -n "${DRY_RUN:-}" ]; then
      echo "DRY_RUN: would start the deploy workflow on main as it is"
      exit 0
    fi
    # The canary, the promotion, the verification and the rollback are the
    # deploy workflow's.
    started="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    gh workflow run deploy.yml --ref main -f confirm=deploy -f issuer=production
    run=""
    for _ in $(seq 1 30); do
      run="$(gh run list --workflow deploy.yml --branch main --limit 1 --json databaseId,createdAt \
        -q ".[] | select(.createdAt >= \"$started\") | .databaseId")"
      [ -z "$run" ] || break
      sleep 5
    done
    [ -n "$run" ] || die "the deploy workflow did not start; look at the Actions page"
    echo "deploy: $(gh run view "$run" --json url -q .url)"
    if gh run watch "$run" --exit-status --interval 30 >/dev/null; then
      echo "released. The summary of that run says what was promoted."
    else
      die "the release failed. The summary of that run says where, and whether it was rolled back."
    fi
    ;;

  *) die "usage: see the top of hack/release.sh" ;;
esac
