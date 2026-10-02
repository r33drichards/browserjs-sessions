#!/usr/bin/env bash
# Delete the local cluster and everything in it (sessions and their disks
# included). The images stay in Docker; .local/ keeps the throwaway CA.
set -euo pipefail
. "$(dirname "$0")/lib.sh"

kind delete cluster --name "$CLUSTER" --kubeconfig "$KUBECONFIG"
rm -f "$KUBECONFIG"
