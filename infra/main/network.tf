data "google_project" "this" {
  project_id = var.project_id
}

locals {
  project_number = data.google_project.this.number

  pods_range_name     = "pods"
  services_range_name = "services"
}

resource "google_compute_network" "this" {
  name                    = var.name
  auto_create_subnetworks = false
  routing_mode            = "REGIONAL"

  depends_on = [google_project_service.this]
}

resource "google_compute_subnetwork" "nodes" {
  name          = "${var.name}-nodes"
  region        = var.region
  network       = google_compute_network.this.id
  ip_cidr_range = var.subnet_cidr

  # Nodes have no public addresses; this lets them reach Google APIs
  # (Artifact Registry, Cloud Storage, logging) without going through NAT.
  private_ip_google_access = true

  secondary_ip_range {
    range_name    = local.pods_range_name
    ip_cidr_range = var.pods_cidr
  }

  secondary_ip_range {
    range_name    = local.services_range_name
    ip_cidr_range = var.services_cidr
  }
}

# Outbound internet for the private nodes and their pods: the browsers in the
# sessions, image pulls from Docker Hub, Dex and Pomerium reaching Google and
# GitHub.
resource "google_compute_router" "this" {
  name    = var.name
  region  = var.region
  network = google_compute_network.this.id
}

resource "google_compute_router_nat" "this" {
  name   = var.name
  region = var.region
  router = google_compute_router.this.name

  nat_ip_allocate_option             = "AUTO_ONLY"
  source_subnetwork_ip_ranges_to_nat = "ALL_SUBNETWORKS_ALL_IP_RANGES"

  # A node full of browsers opens far more connections than the static default
  # of 64 ports per VM allows.
  enable_dynamic_port_allocation      = true
  enable_endpoint_independent_mapping = false
  min_ports_per_vm                    = 256
  max_ports_per_vm                    = 32768

  log_config {
    enable = true
    filter = "ERRORS_ONLY"
  }
}
