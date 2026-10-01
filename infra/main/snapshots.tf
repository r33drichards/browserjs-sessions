# Where Pod Snapshots (a suspended session's memory and root filesystem) live.
# The settings are the ones GKE's "Prepare for Pod snapshots" page requires.

locals {
  snapshot_bucket_name = coalesce(var.snapshot_bucket_name, "${var.project_id}-pod-snapshots")

  workload_pool_prefix = "iam.googleapis.com/projects/${local.project_number}/locations/global/workloadIdentityPools/${var.project_id}.svc.id.goog"

  # Workload Identity Federation principals: no Google service account and no
  # key, the Kubernetes ServiceAccount is itself the IAM principal.
  sessions_namespace_principal_set = "principalSet://${local.workload_pool_prefix}/namespace/${var.sessions_namespace}"
  session_principal                = "principal://${local.workload_pool_prefix}/subject/ns/${var.sessions_namespace}/sa/${var.session_service_account}"

  # Google-managed service agents.
  gke_service_agent      = "serviceAccount:service-${local.project_number}@container-engine-robot.iam.gserviceaccount.com"
  gke_node_service_agent = "serviceAccount:service-${local.project_number}@gcp-sa-gkenode.iam.gserviceaccount.com"

  snapshot_pod_ksa = var.snapshot_token_source == "podKSA"
}

resource "google_storage_bucket" "snapshots" {
  name = local.snapshot_bucket_name
  # Same region as the cluster: snapshots are gigabytes and are read back while
  # a user waits.
  location      = upper(var.region)
  storage_class = "STANDARD"
  force_destroy = var.snapshot_bucket_force_destroy

  # Hierarchical namespace (required, for write throughput) needs uniform
  # bucket-level access.
  uniform_bucket_level_access = true
  public_access_prevention    = "enforced"

  hierarchical_namespace {
    enabled = true
  }

  # Snapshots upload as parallel composite objects, which creates and deletes
  # many temporary objects; soft delete would bill for all of them.
  soft_delete_policy {
    retention_duration_seconds = 0
  }

  # No lifecycle rule: how many snapshots are kept is decided by the
  # PodSnapshotPolicy in deploy/, and the controller deletes the objects.
}

# The Pod Snapshot controller (GKE's service agent) deletes a snapshot's
# objects when its PodSnapshot resource is deleted.
resource "google_storage_bucket_iam_member" "snapshots_controller" {
  bucket = google_storage_bucket.snapshots.name
  role   = "roles/storage.objectUser"
  member = local.gke_service_agent

  depends_on = [google_container_cluster.this]
}

# --- tokenSource: podKSA -----------------------------------------------------------

# Bucket metadata only; no object access.
resource "google_storage_bucket_iam_member" "snapshots_namespace_viewer" {
  count = local.snapshot_pod_ksa ? 1 : 0

  bucket = google_storage_bucket.snapshots.name
  role   = "roles/storage.bucketViewer"
  member = local.sessions_namespace_principal_set

  # The workload identity pool only exists once the cluster does.
  depends_on = [google_container_cluster.this]
}

# Read, write and delete snapshot objects and folders.
resource "google_storage_bucket_iam_member" "snapshots_session_writer" {
  count = local.snapshot_pod_ksa ? 1 : 0

  bucket = google_storage_bucket.snapshots.name
  role   = "roles/storage.objectUser"
  member = local.session_principal

  depends_on = [google_container_cluster.this]
}

# --- tokenSource: federatedP4SA ------------------------------------------------------

# GKE's node service agent mints short-lived tokens scoped to one path of the
# bucket per pod. The role is the one Google's documentation asks for.
resource "google_storage_bucket_iam_member" "snapshots_node_agent" {
  count = local.snapshot_pod_ksa ? 0 : 1

  bucket = google_storage_bucket.snapshots.name
  role   = "roles/storage.admin"
  member = local.gke_node_service_agent

  depends_on = [google_container_cluster.this]
}
