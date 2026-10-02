#!/usr/bin/env bash
# One-time bootstrap, run by a human with Owner rights (e.g. in Cloud Shell).
# Creates: the project, a state bucket, and keyless GitHub Actions access
# (Workload Identity Federation) for two service accounts:
#   tofu-plan   read-only, usable from any branch or PR of the repo
#   tofu-apply  Owner on the project, usable only from refs/heads/main
# The repository is identified by its numeric ID, not its name: a rename or a
# transfer keeps the ID, and a new repository that takes the old name gets
# another one.
# Everything else is managed by OpenTofu in infra/main, run from GitHub Actions.
# Safe to re-run.
#
# Settings (environment variables): PROJECT_ID, REPO, REPO_ID (default: looked
# up from REPO, with gh or without credentials if the repository is public),
# REGION, BILLING (default: the first open billing account) and ORG_ID
# (numeric organisation ID to create the project under; needed when the
# account belongs to an organisation).
set -euo pipefail

PROJECT_ID="${PROJECT_ID:-browserjs-sessions}"
REPO="${REPO:-r33drichards/computer-use}"
REPO_ID="${REPO_ID:-$(gh api "repos/$REPO" -q .id 2>/dev/null ||
  curl -fsS "https://api.github.com/repos/$REPO" 2>/dev/null | sed -n 's/^  "id": \([0-9]*\),$/\1/p' | head -n 1 || true)}"
case "$REPO_ID" in
  "" | *[!0-9]*)
    echo "REPO_ID is not set and could not be looked up: run again with REPO_ID=\$(gh api repos/$REPO -q .id)" >&2
    exit 1
    ;;
esac
REGION="${REGION:-us-west1}"
BILLING="${BILLING:-$(gcloud billing accounts list --filter=open=true --format='value(name)' --limit=1)}"
ORG_ID="${ORG_ID:-}"

retry() { # command...: IAM in a brand-new project can deny for a minute or so
  local attempt
  for attempt in 1 2 3 4 5 6; do
    "$@" && return 0
    echo "attempt $attempt failed; retrying in 20s: $*" >&2
    sleep 20
  done
  "$@"
}

gcloud projects describe "$PROJECT_ID" >/dev/null 2>&1 ||
  gcloud projects create "$PROJECT_ID" --name="browserjs sessions" \
    ${ORG_ID:+--organization="$ORG_ID"}
gcloud billing projects link "$PROJECT_ID" --billing-account="$BILLING" >/dev/null
gcloud config set project "$PROJECT_ID" >/dev/null
gcloud services enable iam.googleapis.com iamcredentials.googleapis.com sts.googleapis.com \
  cloudresourcemanager.googleapis.com serviceusage.googleapis.com storage.googleapis.com
NUM="$(gcloud projects describe "$PROJECT_ID" --format='value(projectNumber)')"

BUCKET="${PROJECT_ID}-tofu-state"
gcloud storage buckets describe "gs://$BUCKET" >/dev/null 2>&1 ||
  gcloud storage buckets create "gs://$BUCKET" --location="$REGION" \
    --uniform-bucket-level-access --public-access-prevention
gcloud storage buckets update "gs://$BUCKET" --versioning >/dev/null

POOL="projects/$NUM/locations/global/workloadIdentityPools/github"
gcloud iam workload-identity-pools describe github --location=global >/dev/null 2>&1 ||
  retry gcloud iam workload-identity-pools create github --location=global --display-name="GitHub Actions"
# Only tokens of this one repository are accepted at all (the condition), and
# what a service account is granted to is the repository ID, or the ID and the
# ref. attribute.repository (the name) is mapped for the audit log only.
MAPPING="google.subject=assertion.sub,attribute.repository=assertion.repository,attribute.repository_id=assertion.repository_id,attribute.repository_id_ref=assertion.repository_id+'@'+assertion.ref"
CONDITION="assertion.repository_id=='$REPO_ID'"
if gcloud iam workload-identity-pools providers describe github --location=global \
  --workload-identity-pool=github >/dev/null 2>&1; then
  # A provider made by an earlier version of this script: bring it up to date.
  gcloud iam workload-identity-pools providers update-oidc github --location=global \
    --workload-identity-pool=github --attribute-mapping="$MAPPING" --attribute-condition="$CONDITION"
else
  retry gcloud iam workload-identity-pools providers create-oidc github --location=global \
    --workload-identity-pool=github --issuer-uri="https://token.actions.githubusercontent.com" \
    --attribute-mapping="$MAPPING" --attribute-condition="$CONDITION"
fi

sa() { # name, display name
  gcloud iam service-accounts describe "$1@$PROJECT_ID.iam.gserviceaccount.com" >/dev/null 2>&1 ||
    gcloud iam service-accounts create "$1" --display-name="$2"
}
grant() { # service account name, role
  gcloud projects add-iam-policy-binding "$PROJECT_ID" --condition=None -q \
    --member="serviceAccount:$1@$PROJECT_ID.iam.gserviceaccount.com" --role="$2" >/dev/null
}
sa tofu-plan "OpenTofu plan (GitHub Actions, any ref)"
sa tofu-apply "OpenTofu apply (GitHub Actions, main only)"
sleep 10 # new service accounts take a moment to be usable in IAM bindings
grant tofu-plan roles/viewer
grant tofu-plan roles/iam.securityReviewer
# roles/viewer lists buckets but cannot read one (no storage.buckets.get),
# which plan needs to refresh the buckets infra/main manages
grant tofu-plan roles/storage.bucketViewer
grant tofu-apply roles/owner
# plan needs to read state and take the state lock
gcloud storage buckets add-iam-policy-binding "gs://$BUCKET" -q \
  --member="serviceAccount:tofu-plan@$PROJECT_ID.iam.gserviceaccount.com" \
  --role=roles/storage.objectAdmin >/dev/null

gcloud iam service-accounts add-iam-policy-binding "tofu-plan@$PROJECT_ID.iam.gserviceaccount.com" -q \
  --role=roles/iam.workloadIdentityUser \
  --member="principalSet://iam.googleapis.com/$POOL/attribute.repository_id/$REPO_ID" >/dev/null
gcloud iam service-accounts add-iam-policy-binding "tofu-apply@$PROJECT_ID.iam.gserviceaccount.com" -q \
  --role=roles/iam.workloadIdentityUser \
  --member="principalSet://iam.googleapis.com/$POOL/attribute.repository_id_ref/$REPO_ID@refs/heads/main" >/dev/null

cat <<EOF

=== set these as GitHub Actions repository variables ===
GCP_PROJECT_ID=$PROJECT_ID
GCP_REGION=$REGION
TOFU_STATE_BUCKET=$BUCKET
GCP_WIF_PROVIDER=$POOL/providers/github
TOFU_PLAN_SA=tofu-plan@$PROJECT_ID.iam.gserviceaccount.com
TOFU_APPLY_SA=tofu-apply@$PROJECT_ID.iam.gserviceaccount.com

=== and this in infra/main/terraform.tfvars ===
github_repository_id = "$REPO_ID"
EOF
