# --- Project and location ---------------------------------------------------------

variable "project_id" {
  description = "The project created by infra/bootstrap/bootstrap.sh. GitHub Actions sets it from the GCP_PROJECT_ID repository variable (TF_VAR_project_id)."
  type        = string

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{4,28}[a-z0-9]$", var.project_id))
    error_message = "A project ID is 6-30 characters: lowercase letters, digits and hyphens, starting with a letter and not ending with a hyphen."
  }
}

variable "region" {
  description = "Region for the network, registry, buckets and load balancer address. GitHub Actions sets it from the GCP_REGION repository variable (TF_VAR_region)."
  type        = string
  default     = "us-west1"

  validation {
    condition     = can(regex("^[a-z]+-[a-z]+[0-9]+$", var.region))
    error_message = "Expected a region such as us-west1."
  }
}

variable "cluster_location" {
  description = <<-EOT
    Where the control plane runs. A zone (us-west1-a) gives a zonal cluster:
    one control plane replica, whose management fee the GKE free tier covers.
    A region (us-west1) gives a regional cluster: three replicas, no free
    tier. Changing this later replaces the cluster.
  EOT
  type        = string
  default     = "us-west1-a"

  validation {
    condition     = can(regex("^[a-z]+-[a-z]+[0-9]+(-[a-z])?$", var.cluster_location))
    error_message = "Expected a zone such as us-west1-a or a region such as us-west1."
  }
}

variable "node_zones" {
  description = <<-EOT
    Zones the nodes run in. Keep it to ONE zone: a session's disk is zonal, so
    its pod can only ever restart in the zone the disk was created in, and a
    Pod Snapshot only restores onto the same CPU type. null means the
    cluster_location zone (zonal cluster) or all of the region's zones
    (regional cluster).
  EOT
  type        = list(string)
  default     = null

  validation {
    condition     = var.node_zones == null ? true : length(var.node_zones) > 0
    error_message = "node_zones is null or a non-empty list of zones."
  }
}

variable "name" {
  description = "Prefix for resource names: the cluster, network, registry and service accounts."
  type        = string
  default     = "browserjs"

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{1,14}[a-z0-9]$", var.name))
    error_message = "3-16 characters: lowercase letters, digits and hyphens, starting with a letter. (Service account IDs built from it are limited to 30.)"
  }
}

variable "labels" {
  description = "Labels put on every resource that takes them."
  type        = map(string)
  default = {
    app        = "browserjs-sessions"
    managed-by = "opentofu"
  }
}

# --- Network ---------------------------------------------------------------------

variable "subnet_cidr" {
  description = "Primary range of the subnet: node addresses."
  type        = string
  default     = "10.10.0.0/20"

  validation {
    condition     = can(cidrhost(var.subnet_cidr, 0))
    error_message = "Not a CIDR range."
  }
}

variable "pods_cidr" {
  description = "Secondary range for pod addresses. Each node takes a /24 from it."
  type        = string
  default     = "10.20.0.0/16"

  validation {
    condition     = can(cidrhost(var.pods_cidr, 0))
    error_message = "Not a CIDR range."
  }
}

variable "services_cidr" {
  description = "Secondary range for Service cluster IPs."
  type        = string
  default     = "10.30.0.0/20"

  validation {
    condition     = can(cidrhost(var.services_cidr, 0))
    error_message = "Not a CIDR range."
  }
}

# --- Cluster ---------------------------------------------------------------------

variable "release_channel" {
  description = <<-EOT
    GKE release channel. Agent Sandbox (v1beta1 API) needs GKE
    1.36.3-gke.1767000 or later and Pod Snapshots 1.35.3-gke.1234000 or later.
    If the channel's default is older than that, set kubernetes_version or use
    RAPID.
  EOT
  type        = string
  default     = "REGULAR"

  validation {
    condition     = contains(["RAPID", "REGULAR", "STABLE", "EXTENDED"], var.release_channel)
    error_message = "One of RAPID, REGULAR, STABLE, EXTENDED."
  }
}

variable "kubernetes_version" {
  description = "Minimum control plane version, e.g. \"1.36.3-gke.1767000\" or the prefix \"1.36\". null takes the release channel's default."
  type        = string
  default     = null
}

variable "enable_agent_sandbox" {
  description = <<-EOT
    Turn on the managed Agent Sandbox add-on (GKE installs and upgrades the
    Sandbox controller, its CRDs and its admission policies). If creating the
    cluster with this on fails because no gVisor node pool exists yet, apply
    once with false, then again with true.
  EOT
  type        = bool
  default     = true
}

variable "enable_pod_snapshots" {
  description = "Turn on the Pod Snapshots add-on (podsnapshot.gke.io CRDs and controller)."
  type        = bool
  default     = true
}

variable "deletion_protection" {
  description = "Refuse to destroy the cluster. To destroy it: set false, apply, then destroy."
  type        = bool
  default     = true
}

variable "master_authorized_cidrs" {
  description = <<-EOT
    CIDR ranges allowed to reach the control plane's public IP endpoint, as
    name => CIDR. Empty means nobody can: use the DNS-based endpoint instead
    (`gcloud container clusters get-credentials --dns-endpoint`), which is
    authorised by IAM and needs no allow-list.
  EOT
  type        = map(string)
  default     = {}

  validation {
    condition     = alltrue([for cidr in values(var.master_authorized_cidrs) : can(cidrhost(cidr, 0))])
    error_message = "Every value must be a CIDR range, e.g. 203.0.113.7/32."
  }
}

variable "maintenance_start_time" {
  description = "Start of the daily-recurring maintenance window, RFC 3339 in UTC. Only the time of day matters. The default is 02:00 Pacific."
  type        = string
  default     = "2026-01-01T10:00:00Z"

  validation {
    condition     = can(formatdate("YYYY", var.maintenance_start_time))
    error_message = "Not an RFC 3339 timestamp."
  }
}

variable "maintenance_end_time" {
  description = "End of the maintenance window. GKE wants at least 48 hours of window in any 32 days, in windows of at least 4 hours."
  type        = string
  default     = "2026-01-01T14:00:00Z"

  validation {
    condition     = can(formatdate("YYYY", var.maintenance_end_time))
    error_message = "Not an RFC 3339 timestamp."
  }
}

variable "maintenance_recurrence" {
  description = "RFC 5545 recurrence of the maintenance window."
  type        = string
  default     = "FREQ=WEEKLY;BYDAY=TU,WE,TH"
}

# --- System node pool (backend, Pomerium, Dex, cert-manager, kube-system) -------------

variable "system_machine_type" {
  description = "Machine type of the always-on pool. GKE Sandbox requires at least one node pool without gVisor, with at least one node."
  type        = string
  default     = "e2-standard-2"
}

variable "system_min_nodes" {
  description = "Minimum nodes in the system pool, per zone."
  type        = number
  default     = 1

  validation {
    condition     = var.system_min_nodes >= 1
    error_message = "The system pool must always have a node: the cluster's own components run there."
  }
}

variable "system_max_nodes" {
  description = "Maximum nodes in the system pool, per zone."
  type        = number
  default     = 2

  validation {
    condition     = var.system_max_nodes >= 1
    error_message = "At least 1."
  }
}

variable "system_disk_size_gb" {
  description = "Boot disk of a system node, GB."
  type        = number
  default     = 50

  validation {
    condition     = var.system_disk_size_gb >= 20
    error_message = "At least 20 GB."
  }
}

# --- Session node pool (gVisor) -------------------------------------------------------

variable "session_machine_type" {
  description = <<-EOT
    Machine type of the gVisor pool that runs session pods. Not E2: Pod
    Snapshots of a whole pod do not support E2. On Intel types gVisor nodes
    run with SMT off, so an n2-standard-4 offers 2 usable CPUs for its 4 billed
    vCPUs and 16 GB.
  EOT
  type        = string
  default     = "n2-standard-4"

  validation {
    condition     = !startswith(var.session_machine_type, "e2-")
    error_message = "E2 machine types cannot take whole-pod Pod Snapshots."
  }
}

variable "session_min_cpu_platform" {
  description = <<-EOT
    Minimum CPU platform of session nodes. A snapshot only restores on a CPU
    with the features it was taken on, and an N2 type can land on Cascade Lake
    or Ice Lake hardware; pinning the platform keeps nodes alike. null leaves
    it to Compute Engine.
  EOT
  type        = string
  default     = "Intel Ice Lake"
}

variable "session_max_nodes" {
  description = "Maximum nodes in the session pool, per zone. The minimum is always 0. This is the hard ceiling on what sessions can cost."
  type        = number
  default     = 3

  validation {
    condition     = var.session_max_nodes >= 1
    error_message = "At least 1."
  }
}

variable "session_spot" {
  description = <<-EOT
    Run session nodes as Spot VMs (roughly 40 % cheaper in us-west1 for N2).
    Compute Engine can take a Spot node back with 30 seconds' notice: the
    sessions on it stop without a fresh snapshot and restart from their disks
    (tabs reopen, pages reload).
  EOT
  type        = bool
  default     = false
}

variable "session_disk_size_gb" {
  description = "Boot disk of a session node, GB. It holds the browser and mcp-js images and every pod's writable layer and emptyDirs."
  type        = number
  default     = 100

  validation {
    condition     = var.session_disk_size_gb >= 50
    error_message = "At least 50 GB."
  }
}

# --- Pod Snapshots storage --------------------------------------------------------------

variable "snapshot_bucket_name" {
  description = "Name of the Pod Snapshots bucket. The default is <project_id>-pod-snapshots."
  type        = string
  default     = null

  validation {
    condition     = var.snapshot_bucket_name == null || can(regex("^[a-z0-9][a-z0-9_.-]{1,61}[a-z0-9]$", var.snapshot_bucket_name))
    error_message = "A bucket name is 3-63 characters: lowercase letters, digits, hyphens, underscores and dots."
  }
}

variable "snapshot_token_source" {
  description = <<-EOT
    How snapshot uploads authenticate; it must match tokenSource in the
    PodSnapshotStorageConfig manifest.
    "podKSA": the session pod's own Kubernetes ServiceAccount is granted the
    bucket roles (sessions_namespace / session_service_account below).
    "federatedP4SA": GKE's node service agent is granted roles/storage.admin on
    the bucket and mints short-lived, path-scoped tokens; no per-ServiceAccount
    grants (needs GKE 1.35.3-gke.1737000 or later).
  EOT
  type        = string
  default     = "podKSA"

  validation {
    condition     = contains(["podKSA", "federatedP4SA"], var.snapshot_token_source)
    error_message = "One of podKSA, federatedP4SA."
  }
}

variable "sessions_namespace" {
  description = "Kubernetes namespace the session Sandboxes run in (deploy/ uses browserjs-sessions)."
  type        = string
  default     = "browserjs-sessions"

  validation {
    condition     = can(regex("^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$", var.sessions_namespace))
    error_message = "Not a valid namespace name."
  }
}

variable "session_service_account" {
  description = "Kubernetes ServiceAccount the session pods run as. Only it may write snapshots (podKSA mode). The Sandbox template in deploy/ must name it."
  type        = string
  default     = "session"

  validation {
    condition     = can(regex("^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$", var.session_service_account))
    error_message = "Not a valid ServiceAccount name."
  }
}

variable "snapshot_bucket_force_destroy" {
  description = "Let `tofu destroy` delete the snapshots bucket even when it still holds snapshots."
  type        = bool
  default     = false
}

# --- Registry ---------------------------------------------------------------------------

variable "registry_keep_versions" {
  description = "Artifact Registry keeps this many most recent versions of each image; older ones are deleted once they are more than registry_delete_older_than_days old."
  type        = number
  default     = 10

  validation {
    condition     = var.registry_keep_versions >= 1
    error_message = "Keep at least one version."
  }
}

variable "registry_delete_older_than_days" {
  description = "Age after which an image version outside the kept set is deleted."
  type        = number
  default     = 30

  validation {
    condition     = var.registry_delete_older_than_days >= 1
    error_message = "At least one day."
  }
}

# --- Edge: address, DNS, certificates -----------------------------------------------------

variable "domain" {
  description = "The registered domain, without a trailing dot."
  type        = string
  default     = "browserjs.com"

  validation {
    condition     = can(regex("^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\\.)+[a-z]{2,}$", var.domain))
    error_message = "Expected a domain such as browserjs.com (lowercase, no trailing dot)."
  }
}

variable "hostnames" {
  description = "Left-hand labels of the four public names under domain. sessions is the parent of the per-session wildcard: *.<sessions>.<domain>."
  type = object({
    app          = optional(string, "app")
    authenticate = optional(string, "authenticate")
    dex          = optional(string, "dex")
    sessions     = optional(string, "sessions")
  })
  default = {}

  validation {
    condition     = alltrue([for label in values(var.hostnames) : can(regex("^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$", label))])
    error_message = "Each value is a single DNS label, e.g. \"app\"."
  }
}

variable "create_dns_zone" {
  description = "Create the Cloud DNS public zone for domain and the records in it. false: DNS is managed elsewhere; create the records from the dns_records output by hand."
  type        = bool
  default     = true
}

variable "dns_ttl" {
  description = "TTL of the address records, seconds."
  type        = number
  default     = 300

  validation {
    condition     = var.dns_ttl >= 30
    error_message = "At least 30 seconds."
  }
}

variable "enable_dnssec" {
  description = "Sign the zone. Only useful once the DS record from the zone has been added at the registrar (Namecheap: Advanced DNS > DNSSEC); turning it on without that changes nothing for resolvers."
  type        = bool
  default     = false
}

variable "edge_mode" {
  description = <<-EOT
    How traffic reaches Pomerium.
    "pomerium_nlb" (recommended): Pomerium's Service is type LoadBalancer; a
    regional passthrough Network Load Balancer hands TCP 443 straight to
    Pomerium, which terminates TLS with a certificate issued in-cluster by
    cert-manager (DNS-01 against this Cloud DNS zone). Creates a regional
    address and cert-manager's Google service account.
    "gateway_alb": a GKE Gateway (gke-l7-global-external-managed) terminates
    TLS in a global external Application Load Balancer with Google-managed
    Certificate Manager certificates and forwards to Pomerium. Creates a
    global address, DNS authorisations, the certificate and a certificate map.
    The two cannot be mixed: both want _acme-challenge.<sessions>.<domain>.
  EOT
  type        = string
  default     = "pomerium_nlb"

  validation {
    condition     = contains(["pomerium_nlb", "gateway_alb"], var.edge_mode)
    error_message = "One of pomerium_nlb, gateway_alb."
  }
}

variable "cert_manager_namespace" {
  description = "Namespace cert-manager is installed in (edge_mode = pomerium_nlb)."
  type        = string
  default     = "cert-manager"
}

variable "cert_manager_service_account" {
  description = "Kubernetes ServiceAccount of the cert-manager controller (edge_mode = pomerium_nlb)."
  type        = string
  default     = "cert-manager"
}

# --- GitHub Actions ------------------------------------------------------------------------

variable "github_repository" {
  description = "The GitHub repository whose workflows may push images, as owner/name. It must be the repository bootstrap.sh restricted the Workload Identity provider to."
  type        = string
  default     = "r33drichards/browserjs-sessions"

  validation {
    condition     = can(regex("^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$", var.github_repository))
    error_message = "Expected owner/name."
  }
}

variable "github_wif_pool_id" {
  description = "ID of the Workload Identity pool bootstrap.sh created for GitHub Actions. It is referenced, never managed, here."
  type        = string
  default     = "github"

  validation {
    condition     = can(regex("^[a-z0-9-]{4,32}$", var.github_wif_pool_id))
    error_message = "A pool ID is 4-32 lowercase letters, digits and hyphens."
  }
}

variable "images_push_ref" {
  description = "The one git ref whose workflows may push images."
  type        = string
  default     = "refs/heads/main"

  validation {
    condition     = startswith(var.images_push_ref, "refs/heads/") || startswith(var.images_push_ref, "refs/tags/")
    error_message = "A full ref: refs/heads/<branch> or refs/tags/<tag>. Never a refs/pull/ ref."
  }
}
