# APIs this configuration needs beyond the ones infra/bootstrap/bootstrap.sh
# turns on (IAM, IAM credentials, STS, Resource Manager, Service Usage,
# Cloud Storage).

locals {
  services = toset(concat(
    [
      "artifactregistry.googleapis.com",
      "compute.googleapis.com",
      "container.googleapis.com",
      "dns.googleapis.com",
      "logging.googleapis.com",
      "monitoring.googleapis.com",
    ],
    var.edge_mode == "gateway_alb" ? ["certificatemanager.googleapis.com"] : [],
  ))
}

resource "google_project_service" "this" {
  for_each = local.services

  project = var.project_id
  service = each.value

  # Destroying this configuration, or dropping an API from the list, leaves
  # the API on: turning one off under resources that still exist breaks them.
  disable_on_destroy = false
}
