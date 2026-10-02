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

# Session nodes: each n2-standard-4 holds about four 3 GiB sessions.
# session_max_nodes is the ceiling on what sessions can cost.
session_machine_type = "n2-standard-4"
session_max_nodes    = 3
session_spot         = false

# Must match the Sandbox template and PodSnapshotStorageConfig in deploy/.
sessions_namespace      = "browserjs-sessions"
session_service_account = "session"
snapshot_token_source   = "podKSA"
