locals {
  cluster_is_zonal = can(regex("-[a-z]$", var.cluster_location))

  # null leaves a regional cluster on all of the region's zones.
  node_zones = var.node_zones != null ? var.node_zones : (local.cluster_is_zonal ? [var.cluster_location] : null)

  # On a zonal cluster the control plane's own zone is implied and must not be
  # repeated in the cluster's node_locations; with nothing left, leave it unset.
  extra_node_zones       = [for zone in coalesce(local.node_zones, []) : zone if zone != var.cluster_location]
  cluster_node_locations = length(local.extra_node_zones) > 0 ? local.extra_node_zones : null
}

resource "google_container_cluster" "this" {
  name     = var.name
  location = var.cluster_location

  node_locations = local.cluster_node_locations

  deletion_protection = var.deletion_protection

  # --- Versions and maintenance ---

  release_channel {
    channel = var.release_channel
  }

  min_master_version = var.kubernetes_version

  maintenance_policy {
    recurring_window {
      start_time = var.maintenance_start_time
      end_time   = var.maintenance_end_time
      recurrence = var.maintenance_recurrence
    }
  }

  # --- Network ---

  network         = google_compute_network.this.id
  subnetwork      = google_compute_subnetwork.nodes.id
  networking_mode = "VPC_NATIVE"

  ip_allocation_policy {
    cluster_secondary_range_name  = local.pods_range_name
    services_secondary_range_name = local.services_range_name
  }

  # Dataplane V2 (eBPF/Cilium). It enforces NetworkPolicy itself, so there is
  # no separate network_policy block (Calico) to turn on.
  datapath_provider = "ADVANCED_DATAPATH"

  private_cluster_config {
    # Nodes get internal addresses only; outbound goes through Cloud NAT.
    enable_private_nodes = true
    # The control plane keeps a public IP endpoint, closed by the allow-list
    # below, and the IAM-authorised DNS endpoint.
    enable_private_endpoint = false
  }

  control_plane_endpoints_config {
    dns_endpoint_config {
      allow_external_traffic = true
    }
  }

  # With no entries this denies every caller of the public IP endpoint.
  master_authorized_networks_config {
    gcp_public_cidrs_access_enabled = false

    dynamic "cidr_blocks" {
      for_each = var.master_authorized_cidrs
      content {
        display_name = cidr_blocks.key
        cidr_block   = cidr_blocks.value
      }
    }
  }

  # The Gateway API CRDs and GKE's GatewayClasses. Needed by edge_mode =
  # gateway_alb; harmless otherwise.
  gateway_api_config {
    channel = "CHANNEL_STANDARD"
  }

  # --- Identity ---

  # Workload Identity Federation for GKE: pods authenticate to Google APIs as
  # their Kubernetes ServiceAccount. Pod Snapshots require it.
  workload_identity_config {
    workload_pool = "${var.project_id}.svc.id.goog"
  }

  # --- Add-ons ---

  addons_config {
    # The managed Agent Sandbox controller, CRDs (agents.x-k8s.io,
    # extensions.agents.x-k8s.io) and admission policies.
    agent_sandbox_config {
      enabled = var.enable_agent_sandbox
    }

    # podsnapshot.gke.io CRDs and controller.
    pod_snapshot_config {
      enabled = var.enable_pod_snapshots
    }

    # Session disks are dynamically provisioned Persistent Disks.
    gce_persistent_disk_csi_driver_config {
      enabled = true
    }

    # Needed for Gateways and for LoadBalancer Services.
    http_load_balancing {
      disabled = false
    }
  }

  # --- Autoscaling ---

  # Scale idle nodes down sooner and pack pods tighter, so the session pool
  # gets back to zero promptly. Node auto-provisioning stays off: the two
  # pools below are the only ones.
  cluster_autoscaling {
    autoscaling_profile = "OPTIMIZE_UTILIZATION"
  }

  # --- The default pool ---

  # A cluster cannot be created without a node pool. This one is deleted as
  # soon as the cluster is up and the pools below replace it; it still gets
  # the custom service account so that its short life does not depend on the
  # Compute Engine default account.
  remove_default_node_pool = true
  initial_node_count       = 1

  node_config {
    service_account = google_service_account.nodes.email
    oauth_scopes    = ["https://www.googleapis.com/auth/cloud-platform"]
  }

  lifecycle {
    ignore_changes = [
      # The default pool is gone after creation; its settings must not make
      # later plans want to replace the cluster.
      node_config,
      initial_node_count,
    ]

    precondition {
      condition     = var.cluster_location == var.region || startswith(var.cluster_location, "${var.region}-")
      error_message = "cluster_location must be var.region or one of its zones: the subnet, address and buckets are in var.region."
    }
  }

  depends_on = [
    google_project_service.this,
    google_project_iam_member.nodes_default,
  ]
}

# --- System pool: everything that is not a session --------------------------------

resource "google_container_node_pool" "system" {
  name     = "system"
  cluster  = google_container_cluster.this.name
  location = google_container_cluster.this.location

  node_locations = local.node_zones

  initial_node_count = var.system_min_nodes

  autoscaling {
    min_node_count = var.system_min_nodes
    max_node_count = max(var.system_min_nodes, var.system_max_nodes)
  }

  management {
    auto_repair  = true
    auto_upgrade = true
  }

  upgrade_settings {
    max_surge       = 1
    max_unavailable = 0
  }

  node_config {
    machine_type = var.system_machine_type
    image_type   = "COS_CONTAINERD"
    disk_type    = "pd-balanced"
    disk_size_gb = var.system_disk_size_gb

    service_account = google_service_account.nodes.email
    oauth_scopes    = ["https://www.googleapis.com/auth/cloud-platform"]

    workload_metadata_config {
      mode = "GKE_METADATA"
    }

    shielded_instance_config {
      enable_secure_boot          = true
      enable_integrity_monitoring = true
    }

    labels = {
      "browserjs.com/pool" = "system"
    }

    resource_labels = var.labels
  }

  lifecycle {
    ignore_changes = [initial_node_count]
  }
}

# --- Session pool: gVisor, scales from zero -----------------------------------------

resource "google_container_node_pool" "sessions" {
  name     = "sessions"
  cluster  = google_container_cluster.this.name
  location = google_container_cluster.this.location

  node_locations = var.session_node_zones != null ? var.session_node_zones : local.node_zones

  initial_node_count = 0

  autoscaling {
    min_node_count = 0
    max_node_count = var.session_max_nodes
    # ANY lets the autoscaler take Spot capacity wherever there is some.
    location_policy = var.session_spot ? "ANY" : "BALANCED"
  }

  management {
    auto_repair  = true
    auto_upgrade = true
  }

  upgrade_settings {
    max_surge       = 1
    max_unavailable = 0
  }

  node_config {
    machine_type     = var.session_machine_type
    min_cpu_platform = var.session_min_cpu_platform
    spot             = var.session_spot

    # GKE Sandbox: the gVisor runtime ("runtimeClassName: gvisor"). GKE itself
    # adds the label and the NoSchedule taint sandbox.gke.io/runtime=gvisor to
    # these nodes, so neither is declared here. cos_containerd is the only
    # image type GKE Sandbox supports.
    sandbox_config {
      type = "GVISOR"
    }
    image_type = "COS_CONTAINERD"

    # Image streaming: a container starts on a mount of its image and reads
    # blocks from Artifact Registry as it touches them, instead of waiting
    # for the whole image to be pulled and unpacked onto a new node.
    gcfs_config {
      enabled = var.session_image_streaming
    }

    disk_type    = "pd-balanced"
    disk_size_gb = var.session_disk_size_gb

    service_account = google_service_account.nodes.email
    oauth_scopes    = ["https://www.googleapis.com/auth/cloud-platform"]

    workload_metadata_config {
      mode = "GKE_METADATA"
    }

    shielded_instance_config {
      enable_secure_boot          = true
      enable_integrity_monitoring = true
    }

    labels = {
      "browserjs.com/pool" = "sessions"
    }

    resource_labels = var.labels
  }

  lifecycle {
    ignore_changes = [initial_node_count]
  }

  # GKE Sandbox needs a node pool without gVisor to exist first.
  depends_on = [
    google_container_node_pool.system,
    google_project_iam_member.nodes_image_streaming,
  ]
}

# --- Fallback session pools: other machine types, same shape --------------------------
# Session pods select only on sandbox.gke.io/runtime=gvisor, so the autoscaler
# may grow whichever of these pools Compute Engine has capacity for.

resource "google_container_node_pool" "sessions_fallback" {
  for_each = var.session_fallback_machine_types

  name     = "sessions-${each.key}"
  cluster  = google_container_cluster.this.name
  location = google_container_cluster.this.location

  node_locations = var.session_node_zones != null ? var.session_node_zones : local.node_zones

  initial_node_count = 0

  autoscaling {
    min_node_count = 0
    max_node_count = var.session_max_nodes
    # ANY lets the autoscaler take Spot capacity wherever there is some.
    location_policy = var.session_spot ? "ANY" : "BALANCED"
  }

  management {
    auto_repair  = true
    auto_upgrade = true
  }

  upgrade_settings {
    max_surge       = 1
    max_unavailable = 0
  }

  node_config {
    machine_type     = each.key
    min_cpu_platform = each.value
    spot             = var.session_spot

    sandbox_config {
      type = "GVISOR"
    }
    image_type = "COS_CONTAINERD"

    gcfs_config {
      enabled = var.session_image_streaming
    }

    disk_type    = "pd-balanced"
    disk_size_gb = var.session_disk_size_gb

    service_account = google_service_account.nodes.email
    oauth_scopes    = ["https://www.googleapis.com/auth/cloud-platform"]

    workload_metadata_config {
      mode = "GKE_METADATA"
    }

    shielded_instance_config {
      enable_secure_boot          = true
      enable_integrity_monitoring = true
    }

    labels = {
      "browserjs.com/pool" = "sessions-${each.key}"
    }

    resource_labels = var.labels
  }

  lifecycle {
    ignore_changes = [initial_node_count]
  }

  depends_on = [
    google_container_node_pool.system,
    google_project_iam_member.nodes_image_streaming,
  ]
}
