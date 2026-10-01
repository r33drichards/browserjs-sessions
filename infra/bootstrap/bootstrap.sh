#!/usr/bin/env bash
# One-time bootstrap, run by a human with Owner rights (e.g. in Cloud Shell).
# Creates: the project, a state bucket, and keyless GitHub Actions access
# (Workload Identity Federation) for two service accounts:
#   tofu-plan   read-only, usable from any branch or PR of the repo
#   tofu-apply  Owner on the project, usable only from refs/heads/main
# Everything else is managed by OpenTofu in infra/main, run from GitHub Actions.
# Safe to re-run.
set -euo pipefail

PROJECT_ID="${PROJECT_ID:-browserjs-sessions}"
REPO="${REPO:-r33drichards/browserjs-sessions}"
REGION="${REGION:-us-west1}"
BILLING="${BILLING:-$(gcloud billing accounts list --filter=open=true --format='value(name)' --limit=1)}"

gcloud projects describe "$PROJECT_ID" >/dev/null 2>&1 ||
  gcloud projects create "$PROJECT_ID" --name="browserjs sessions"
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
  gcloud iam workload-identity-pools create github --location=global --display-name="GitHub Actions"
gcloud iam workload-identity-pools providers describe github --location=global \
  --workload-identity-pool=github >/dev/null 2>&1 ||
  gcloud iam workload-identity-pools providers create-oidc github --location=global \
    --workload-identity-pool=github --issuer-uri="https://token.actions.githubusercontent.com" \
    --attribute-mapping="google.subject=assertion.sub,attribute.repository=assertion.repository,attribute.repo_ref=assertion.repository+'@'+assertion.ref" \
    --attribute-condition="assertion.repository=='$REPO'"

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
grant tofu-apply roles/owner
# plan needs to read state and take the state lock
gcloud storage buckets add-iam-policy-binding "gs://$BUCKET" -q \
  --member="serviceAccount:tofu-plan@$PROJECT_ID.iam.gserviceaccount.com" \
  --role=roles/storage.objectAdmin >/dev/null

gcloud iam service-accounts add-iam-policy-binding "tofu-plan@$PROJECT_ID.iam.gserviceaccount.com" -q \
  --role=roles/iam.workloadIdentityUser \
  --member="principalSet://iam.googleapis.com/$POOL/attribute.repository/$REPO" >/dev/null
gcloud iam service-accounts add-iam-policy-binding "tofu-apply@$PROJECT_ID.iam.gserviceaccount.com" -q \
  --role=roles/iam.workloadIdentityUser \
  --member="principalSet://iam.googleapis.com/$POOL/attribute.repo_ref/$REPO@refs/heads/main" >/dev/null

cat <<EOF

=== set these as GitHub Actions repository variables ===
GCP_PROJECT_ID=$PROJECT_ID
GCP_REGION=$REGION
TOFU_STATE_BUCKET=$BUCKET
GCP_WIF_PROVIDER=$POOL/providers/github
TOFU_PLAN_SA=tofu-plan@$PROJECT_ID.iam.gserviceaccount.com
TOFU_APPLY_SA=tofu-apply@$PROJECT_ID.iam.gserviceaccount.com
EOF
