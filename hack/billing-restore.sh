#!/usr/bin/env bash
# Puts an export of the ledger (the billing-export CronJob's file: every
# Account, Grant and UsagePeriod as `kubectl get -o yaml` printed them) into
# the cluster of the current kubectl context. For a cluster that was
# recreated; docs/billing-deployment.md has the whole procedure, which ends
# with the Stripe reconcile (docs/contracts/billing/stripe.md).
#
#   hack/billing-restore.sh export.yaml
#
# The three CRDs must be installed and the billing operator must have no
# pods (it would meter against a half-restored ledger). Objects that exist
# already are updated, not duplicated: running it twice changes nothing.
#
# Needs kubectl and jq.
set -euo pipefail

[ $# -eq 1 ] && [ -f "$1" ] || { echo "usage: hack/billing-restore.sh <export.yaml>" >&2; exit 1; }
NS="${NS:-browserjs-sessions}"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

ready="$(kubectl -n "$NS" get deployment billing-operator -o jsonpath='{.status.replicas}' 2>/dev/null || true)"
if [ -n "$ready" ] && [ "$ready" != 0 ]; then
  echo "billing-restore: the billing operator has pods: restore with billing off (hack/billing-stage.sh), then turn it on" >&2
  exit 1
fi

# The file as JSON, read without asking the cluster about it; then what
# only the cluster it came from could know is taken off each object.
kubectl create --dry-run=client --validate=false -o json -f "$1" |
  jq -s '[.[] | if .kind == "List" then .items[] else . end] | map(
    del(.metadata.resourceVersion, .metadata.uid, .metadata.creationTimestamp, .metadata.generation,
        .metadata.managedFields, .metadata.selfLink, .metadata.ownerReferences,
        .metadata.annotations["kubectl.kubernetes.io/last-applied-configuration"]))' >"$work/objects.json"
echo "billing-restore: $(jq -r 'group_by(.kind) | map("\(length) \(.[0].kind)") | join(", ")' "$work/objects.json")"

# Accounts first: nothing requires it, but a Grant then never names an
# Account that is not there yet.
jq '{apiVersion: "v1", kind: "List", items: (map(del(.status)) | sort_by(.kind))}' "$work/objects.json" |
  kubectl -n "$NS" apply -f - >/dev/null

# The status is a subresource, which apply does not write: for an Account
# it is the ledger itself (what was used of each grant, the period so far).
jq -c '.[] | select(.status != null) | {kind: (.kind | ascii_downcase), name: .metadata.name, patch: {status}}' "$work/objects.json" |
  while read -r line; do
    kubectl -n "$NS" patch "$(jq -r '.kind' <<<"$line").browserjs.dev" "$(jq -r '.name' <<<"$line")" \
      --subresource=status --type=merge -p "$(jq -c '.patch' <<<"$line")" >/dev/null
  done
echo "billing-restore: applied, with the status of each. Next: the Stripe reconcile, then billing on."
