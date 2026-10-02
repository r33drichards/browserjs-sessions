# One Docker repository for the three images: backend, browser and mcp-js.
resource "google_artifact_registry_repository" "images" {
  repository_id = var.name
  location      = var.region
  format        = "DOCKER"
  description   = "browserjs sessions images: backend, browser, mcp-js"

  docker_config {
    # Tags stay movable (":main"); deploy/ should pin digests.
    immutable_tags = false
  }

  cleanup_policy_dry_run = false

  # A version is deleted only if a DELETE policy matches it and no KEEP policy
  # does: old versions go, except the most recent few of each image.
  cleanup_policies {
    id     = "delete-old"
    action = "DELETE"
    condition {
      older_than = "${var.registry_delete_older_than_days * 24 * 60 * 60}s"
    }
  }

  cleanup_policies {
    id     = "keep-recent"
    action = "KEEP"
    most_recent_versions {
      keep_count = var.registry_keep_versions
    }
  }

  depends_on = [google_project_service.this]
}

locals {
  registry_url = "${google_artifact_registry_repository.images.location}-docker.pkg.dev/${var.project_id}/${google_artifact_registry_repository.images.repository_id}"
}
