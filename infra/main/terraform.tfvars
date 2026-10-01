# Copy to terraform.tfvars (which git ignores) and edit. Only project_id has
# no default; everything else below shows the default or a common change.

# The project_id output of infra/bootstrap.
project_id = "browserjs-sessions-xxxx"

region           = "us-west1"
cluster_location = "us-west1-a" # a zone: zonal control plane, covered by the GKE free tier

domain = "browserjs.com"

# "pomerium_nlb": Pomerium terminates TLS behind a passthrough load balancer,
#                 certificates from cert-manager (recommended).
# "gateway_alb":  a GKE Gateway terminates TLS with Certificate Manager.
edge_mode = "pomerium_nlb"

# Agent Sandbox needs GKE 1.36.3-gke.1767000 or later. If REGULAR's default is
# older when you apply, pin the version or switch to RAPID.
release_channel = "REGULAR"
# kubernetes_version = "1.36"

# Who may reach the control plane's public IP endpoint. Leave empty and use
# the DNS endpoint (see the get_credentials_command output) instead.
# master_authorized_cidrs = {
#   home = "203.0.113.7/32"
# }

# Session nodes: each n2-standard-4 holds about four 3 GiB sessions.
session_machine_type = "n2-standard-4"
session_max_nodes    = 3
session_spot         = false

# Must match the Sandbox template and PodSnapshotStorageConfig in deploy/.
sessions_namespace      = "browserjs-sessions"
session_service_account = "session"
snapshot_token_source   = "podKSA"
