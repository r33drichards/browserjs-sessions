#!/usr/bin/env bash
# Configure a user-targeted skills rollout. See docs/mcp-skills.md.
set -euo pipefail
NS="${NS:-browserjs-sessions}"
case "${1:-}" in
  on)
    digest="${2:?usage: skills-stage.sh on sha256:... verified-email}"
    email="${3:?supply one authenticated user email}"
    [[ "$digest" =~ ^sha256:[0-9a-f]{64}$ ]] || { echo "expected an immutable image digest" >&2; exit 1; }
    [[ "$email" == *@* && "$email" != *','* && "$email" != *' '* ]] || { echo "supply one verified email" >&2; exit 1; }
    email="$(printf '%s' "$email" | tr '[:upper:]' '[:lower:]')"
    ;;
  off)
    digest=""
    email=""
    ;;
  *) echo "usage: skills-stage.sh on sha256:... verified-email | off" >&2; exit 1 ;;
esac
kubectl -n "$NS" create configmap mcp-skills \
  --from-literal="image-digest=$digest" --from-literal="emails=$email" \
  --dry-run=client -o yaml | kubectl -n "$NS" apply -f -
# GKE's Rollout references this Deployment's template. Changing it initiates
# the same analyzed blue-green rollout used for backend image changes.
kubectl -n "$NS" rollout restart deployment/backend
echo "Configured mcp-skills. Verify the backend rollout before creating a new session."
