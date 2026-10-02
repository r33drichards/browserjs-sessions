#!/usr/bin/env bash
# Puts an export of the Accounts (the billing-export CronJob's file: every
# Account as `kubectl get -o yaml` printed it) into the cluster of the
# current kubectl context. For a cluster that was recreated;
# docs/billing-deployment.md has the whole procedure, which ends with the
# reconciles against Stripe and Metronome.
#
#   hack/billing-restore.sh export.yaml
#
# The Account CRD must be installed and billing must be off (the backend
# would otherwise make Accounts of its own for whoever signs in meanwhile).
# Accounts that exist already are updated, not duplicated: running it twice
# changes nothing.
#
# Needs kubectl and jq.
set -euo pipefail

[ $# -eq 1 ] && [ -f "$1" ] || { echo "usage: hack/billing-restore.sh <export.yaml>" >&2; exit 1; }
NS="${NS:-browserjs-sessions}"

replicas="$(kubectl -n "$NS" get deployment billing-operator -o jsonpath='{.status.replicas}' 2>/dev/null || true)"
if [ -n "$replicas" ] && [ "$replicas" != 0 ]; then
  echo "billing-restore: the billing operator has pods: restore with billing off (hack/billing-stage.sh), then turn it on" >&2
  exit 1
fi

# The file as JSON, read without asking the cluster about it; then what
# only the cluster it came from could know is taken off each object.
objects="$(kubectl create --dry-run=client --validate=false -o json -f "$1" |
  jq -s '[.[] | if .kind == "List" then .items[] else . end] | map(
    del(.metadata.resourceVersion, .metadata.uid, .metadata.creationTimestamp, .metadata.generation,
        .metadata.managedFields, .metadata.selfLink, .metadata.ownerReferences, .status,
        .metadata.annotations["kubectl.kubernetes.io/last-applied-configuration"]))')"
echo "billing-restore: $(jq -r 'group_by(.kind) | map("\(length) \(.[0].kind)") | join(", ")' <<<"$objects")"
if [ "$(jq -r '[.[] | select(.kind != "Account")] | length' <<<"$objects")" != 0 ]; then
  echo "billing-restore: the file has something other than Accounts in it" >&2
  exit 1
fi
jq '{apiVersion: "v1", kind: "List", items: .}' <<<"$objects" | kubectl -n "$NS" apply -f - >/dev/null
echo "billing-restore: applied. Next: the Stripe and Metronome reconciles, then billing on."
