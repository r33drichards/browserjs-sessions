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
