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

# Image streaming: the node reads image blocks through the Container File
# System API, which a custom node service account may only call with this
# role. It lets the account use the project's enabled APIs and nothing more.
resource "google_project_iam_member" "nodes_image_streaming" {
  project = var.project_id
  role    = "roles/serviceusage.serviceUsageConsumer"
  member  = google_service_account.nodes.member

  depends_on = [google_project_service.this]
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

# The same grant by the repository's numeric ID, which a rename or a transfer
# does not change: the provider maps attribute.repository_id_ref to
# "<repository id>@<ref>". Both grants exist while the repository is renamed;
# the one by name above is removed afterwards.
resource "google_service_account_iam_member" "images_push_github_id" {
  service_account_id = google_service_account.images_push.name
  role               = "roles/iam.workloadIdentityUser"
  member             = "principalSet://iam.googleapis.com/projects/${local.project_number}/locations/global/workloadIdentityPools/${var.github_wif_pool_id}/attribute.repository_id_ref/${var.github_repository_id}@${var.images_push_ref}"
}

# --- Deploys from GitHub Actions -----------------------------------------------------

# The identity .github/workflows/deploy.yml and cluster-info.yml reach the
# cluster with. Like images-push it has no key and can only be used by a
# workflow of this repository running on one ref.
resource "google_service_account" "deployer" {
  account_id   = "deployer"
  display_name = "Deploys to the cluster (GitHub Actions, ${var.deploy_ref} only)"
}

# Kubernetes Engine Admin, because the deploy creates what Kubernetes Engine
# Developer may not: ClusterRoles and their bindings (Dex, cert-manager),
# Roles that grant more than the caller holds, and cert-manager's admission
# webhooks. Developer has only get and list on all of those. This role is
# also allowed to change and delete clusters; nothing narrower covers the
# Kubernetes objects, and the cluster has deletion protection. It reaches no
# other Google Cloud service.
resource "google_project_iam_member" "deployer_cluster" {
  project = var.project_id
  role    = "roles/container.admin"
  member  = google_service_account.deployer.member
}

resource "google_service_account_iam_member" "deployer_github" {
  service_account_id = google_service_account.deployer.name
  role               = "roles/iam.workloadIdentityUser"
  member             = "principalSet://iam.googleapis.com/projects/${local.project_number}/locations/global/workloadIdentityPools/${var.github_wif_pool_id}/attribute.repo_ref/${var.github_repository}@${var.deploy_ref}"
}

# By the repository's numeric ID as well, as for images-push above.
resource "google_service_account_iam_member" "deployer_github_id" {
  service_account_id = google_service_account.deployer.name
  role               = "roles/iam.workloadIdentityUser"
  member             = "principalSet://iam.googleapis.com/projects/${local.project_number}/locations/global/workloadIdentityPools/${var.github_wif_pool_id}/attribute.repository_id_ref/${var.github_repository_id}@${var.deploy_ref}"
}
