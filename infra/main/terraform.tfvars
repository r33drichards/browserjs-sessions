# Settings for the one deployment, read by GitHub Actions on every plan and
# apply. No secrets belong here, and none are needed.
#
# project_id and region are NOT set here: the workflows pass them from the
# repository variables GCP_PROJECT_ID and GCP_REGION. To run tofu by hand, set
# TF_VAR_project_id.
#
# Every line below restates a default, as the place to change it.

# A zone: zonal control plane, covered by the GKE free tier. Must be in
# GCP_REGION.
cluster_location = "us-west1-a"

domain = "browserjs.com"

# The domain the deployment is moving to (docs/domain-switch.md): a second
# zone with the same records. Nothing is served under it yet.
additional_domains = ["computeruse.site"]

# "pomerium_nlb": Pomerium terminates TLS behind a passthrough load balancer,
#                 certificates from cert-manager (recommended).
# "gateway_alb":  a GKE Gateway terminates TLS with Certificate Manager.
edge_mode = "pomerium_nlb"

# The backend speaks Agent Sandbox's v1beta1 API (spec.operatingMode), which
# the managed add-on serves from GKE 1.36.3-gke.1767000. The cluster was
# created on REGULAR's default, 1.35.8, where the add-on serves v1alpha1 only.
# "1.36" asks for the newest 1.36 the channel offers; changing it upgrades the
# control plane in place (the API is unreachable for some minutes on a zonal
# cluster) and the node pools follow by auto-upgrade. See what the channel
# offers first:
#   gcloud container get-server-config --location us-west1-a --format='yaml(channels)'
# If REGULAR has nothing at or above 1.36.3-gke.1767000, use RAPID.
release_channel    = "REGULAR"
kubernetes_version = "1.36"

# Who may reach the control plane's public IP endpoint. Leave empty and use
# the DNS endpoint (see the get_credentials_command output) instead.
# master_authorized_cidrs = {
#   home = "203.0.113.7/32"
# }

# Session nodes: a 16 GB node holds about ten sessions (packed by their
# requests in deploy/gke/blueprint.yaml; CPU is overcommitted).
# session_max_nodes is the ceiling on what sessions can cost.
session_machine_type = "n2-standard-4"
session_max_nodes    = 3
# Spot: cheaper, but Compute Engine can take a node back with 30 seconds'
# notice; sessions on it restart from their disks (tabs reopen, pages reload).
session_spot = true

# A new node starts the browser from a remote mount of its image instead of
# pulling all 905 MB first (docs/cold-start.md). Changing this recreates
# every session node that is running, at once.
session_image_streaming = true

# us-west1-a alone had no n2-standard-4 capacity when the first session was
# created ("GCE out of resources"), so session nodes may start in any zone of
# the region. A session stays in the zone its disk was created in.
session_node_zones = ["us-west1-a", "us-west1-b", "us-west1-c"]

# The same again on other machine series, tried when N2 has no capacity. Each
# pool scales 0..session_max_nodes, so the ceiling is three pools' worth.
session_fallback_machine_types = {
  "n2d-standard-4" = "AMD Milan"
  "c3-standard-4"  = null
}

# Must match the Sandbox template and PodSnapshotStorageConfig in deploy/.
sessions_namespace      = "browserjs-sessions"
session_service_account = "session"
snapshot_token_source   = "podKSA"

# Boot disk of a session node. The project's SSD quota in us-west1 is 250 GB
# and not adjustable; pd-balanced counts against it, together with the system
# node's 50 GB and 5 GB per session. At the default 100 GB a second session
# node did not fit ("GCE quota exceeded"). With 50 GB two session nodes fit;
# the 12-CPU quota allows no more than two anyway.
session_disk_size_gb = 50
