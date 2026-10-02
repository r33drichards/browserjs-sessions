# The ledger's daily backup (deploy/gke/billing-export.yaml): every Account,
# Grant and UsagePeriod, as YAML, one object a day. etcd is not a database:
# without this, a cluster that is recreated loses the usage counted in the
# current period and the record of which cards have had the sign-up credit.
# Restoring is hack/billing-restore.sh (docs/billing-deployment.md).

locals {
  billing_export_bucket_name = coalesce(var.billing_export_bucket_name, "${var.project_id}-billing-export")

  # Workload Identity Federation: the CronJob's Kubernetes ServiceAccount is
  # itself the IAM principal. No Google service account and no key.
  billing_export_principal = "principal://${local.workload_pool_prefix}/subject/ns/${var.sessions_namespace}/sa/${var.billing_export_service_account}"
}

resource "google_storage_bucket" "billing_export" {
  name          = local.billing_export_bucket_name
  location      = upper(var.region)
  storage_class = "STANDARD"
  force_destroy = false

  uniform_bucket_level_access = true
  public_access_prevention    = "enforced"

  # An object is never overwritten in normal use (each export has its own
  # name); versioning is for the abnormal one.
  versioning {
    enabled = true
  }

  # 90 days of exports. The record that matters is the latest one.
  lifecycle_rule {
    condition {
      age        = var.billing_export_retention_days
      with_state = "ANY"
    }
    action {
      type = "Delete"
    }
  }
}

# Create only: the CronJob can add an export, and can neither read, replace
# nor delete one. Who restores reads the bucket with their own identity.
resource "google_storage_bucket_iam_member" "billing_export_writer" {
  bucket = google_storage_bucket.billing_export.name
  role   = "roles/storage.objectCreator"
  member = local.billing_export_principal

  # The workload identity pool only exists once the cluster does.
  depends_on = [google_container_cluster.this]
}
