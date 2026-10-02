# The identity of every node VM. Not the Compute Engine default account (which
# is often a project Editor): only what a node needs to run and report.
resource "google_service_account" "nodes" {
  account_id   = "${var.name}-nodes"
  display_name = "GKE nodes (${var.name})"
}

# Logs, metrics and autoscaling metrics: the minimum Google documents for a
# custom node service account.
resource "google_project_iam_member" "nodes_default" {
  project = var.project_id
  role    = "roles/container.defaultNodeServiceAccount"
  member  = google_service_account.nodes.member
}

# Image pulls: the kubelet pulls with the node's identity. Read access to this
# one repository, nothing else in the project.
resource "google_artifact_registry_repository_iam_member" "nodes_pull" {
  project    = var.project_id
  location   = google_artifact_registry_repository.images.location
  repository = google_artifact_registry_repository.images.name
  role       = "roles/artifactregistry.reader"
  member     = google_service_account.nodes.member
}

# --- Image pushes from GitHub Actions ------------------------------------------------

# The identity .github/workflows/images.yml pushes with. Its only permission
# is writing to the one image repository.
resource "google_service_account" "images_push" {
  account_id   = "images-push"
  display_name = "Image pushes (GitHub Actions, ${var.images_push_ref} only)"
}

resource "google_artifact_registry_repository_iam_member" "images_push" {
  project    = var.project_id
  location   = google_artifact_registry_repository.images.location
  repository = google_artifact_registry_repository.images.name
  role       = "roles/artifactregistry.writer"
  member     = google_service_account.images_push.member
}

# Keyless: a workflow of this repository, running on this one ref, may act as
# the service account. The Workload Identity pool and its provider are made
# by infra/bootstrap/bootstrap.sh and are not in this state; the provider
# maps attribute.repo_ref to "<owner>/<repo>@<ref>". Pull requests run on
# refs/pull/…, so they never match.
resource "google_service_account_iam_member" "images_push_github" {
  service_account_id = google_service_account.images_push.name
  role               = "roles/iam.workloadIdentityUser"
  member             = "principalSet://iam.googleapis.com/projects/${local.project_number}/locations/global/workloadIdentityPools/${var.github_wif_pool_id}/attribute.repo_ref/${var.github_repository}@${var.images_push_ref}"
}
