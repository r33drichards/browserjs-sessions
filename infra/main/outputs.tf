# Everything the kustomize manifests in deploy/ and the person applying them
# need. Nothing here is secret.

# --- Project and cluster ---

output "project_id" {
  description = "The project."
  value       = var.project_id
}

output "project_number" {
  description = "The project's number."
  value       = local.project_number
}

output "region" {
  description = "Region of the registry, buckets and (pomerium_nlb) the address."
  value       = var.region
}

output "cluster_name" {
  description = "Name of the GKE cluster."
  value       = google_container_cluster.this.name
}

output "cluster_location" {
  description = "Zone or region of the GKE control plane."
  value       = google_container_cluster.this.location
}

output "cluster_version" {
  description = "Control plane version after the last apply. Agent Sandbox v1beta1 needs 1.36.3-gke.1767000 or later."
  value       = google_container_cluster.this.master_version
}

output "get_credentials_command" {
  description = "Writes a kubeconfig entry that uses the IAM-authorised DNS endpoint."
  value       = "gcloud container clusters get-credentials ${google_container_cluster.this.name} --location ${google_container_cluster.this.location} --project ${var.project_id} --dns-endpoint"
}

output "workload_pool" {
  description = "Workload Identity pool of the cluster."
  value       = "${var.project_id}.svc.id.goog"
}

output "session_node_selector" {
  description = "nodeSelector, toleration and runtimeClassName a session pod needs to land on the gVisor pool."
  value = {
    runtime_class_name = "gvisor"
    node_selector      = { "sandbox.gke.io/runtime" = "gvisor" }
    toleration         = { key = "sandbox.gke.io/runtime", operator = "Equal", value = "gvisor", effect = "NoSchedule" }
  }
}

# --- Images ---

output "registry_url" {
  description = "Image name prefix: <registry_url>/backend, /browser, /mcp-js. Set it as the repository variable IMAGE_REGISTRY."
  value       = local.registry_url
}

output "docker_login_command" {
  description = "Lets a local docker push to the registry."
  value       = "gcloud auth configure-docker ${google_artifact_registry_repository.images.location}-docker.pkg.dev"
}

# --- Service accounts ---

output "images_push_service_account_email" {
  description = "Google service account GitHub Actions pushes images with. Set it as the repository variable IMAGE_PUSH_SA (and registry_url as IMAGE_REGISTRY)."
  value       = google_service_account.images_push.email
}

output "node_service_account_email" {
  description = "Google service account of every node. It can pull from the registry."
  value       = google_service_account.nodes.email
}

output "cert_manager_service_account_email" {
  description = "Google service account for cert-manager's DNS-01 solver (pomerium_nlb only). Annotate cert-manager's ServiceAccount: iam.gke.io/gcp-service-account=<this>."
  value       = local.edge_nlb ? google_service_account.cert_manager[0].email : null
}

# --- Pod Snapshots ---

output "snapshot_bucket" {
  description = "Bucket for PodSnapshotStorageConfig spec.snapshotStorageConfig.gcs.bucket."
  value       = google_storage_bucket.snapshots.name
}

output "snapshot_token_source" {
  description = "Value for PodSnapshotStorageConfig spec.snapshotStorageConfig.gcs.tokenSource."
  value       = var.snapshot_token_source
}

output "sessions_namespace" {
  description = "Namespace whose session pods may use the snapshot bucket."
  value       = var.sessions_namespace
}

output "session_service_account" {
  description = "Kubernetes ServiceAccount the session pods must run as to write snapshots (podKSA mode)."
  value       = var.session_service_account
}

# --- Edge ---

output "edge_mode" {
  description = "pomerium_nlb or gateway_alb."
  value       = var.edge_mode
}

output "edge_ip_address" {
  description = "The public IPv4 address every hostname resolves to."
  value       = local.edge_ip
}

output "edge_ip_name" {
  description = "Name of the address resource. pomerium_nlb: the Service annotation networking.gke.io/load-balancer-ip-addresses. gateway_alb: the Gateway's spec.addresses[].value with type NamedAddress."
  value       = local.edge_nlb ? google_compute_address.edge[0].name : google_compute_global_address.edge[0].name
}

output "hostnames" {
  description = "The public names."
  value       = local.public_names
}

output "certificate_map_name" {
  description = "Certificate Manager map for the Gateway annotation networking.gke.io/certmap (gateway_alb only)."
  value       = local.edge_alb ? google_certificate_manager_certificate_map.this[0].name : null
}

output "certificate_dns_names" {
  description = "dnsNames for the cert-manager Certificate that fills Pomerium's TLS secret (pomerium_nlb only)."
  value       = local.edge_nlb ? values(local.public_names) : null
}

# --- DNS ---

output "dns_zone_name" {
  description = "Cloud DNS zone name (cert-manager's cloudDNS solver can be pinned to it with hostedZoneName)."
  value       = var.create_dns_zone ? google_dns_managed_zone.this[0].name : null
}

output "dns_name_servers" {
  description = "Set these as the domain's custom nameservers at the registrar (Namecheap: Domain List > Manage > Nameservers > Custom DNS). Nothing resolves until that is done."
  value       = var.create_dns_zone ? google_dns_managed_zone.this[0].name_servers : null
}

output "dns_records" {
  description = "The records that must exist. Created here when create_dns_zone is true; otherwise create them wherever the domain's DNS lives."
  value = concat(
    [for name in values(local.public_names) : { name = name, type = "A", value = local.edge_ip }],
    [for auth in values(google_certificate_manager_dns_authorization.this) : {
      name  = trimsuffix(auth.dns_resource_record[0].name, ".")
      type  = auth.dns_resource_record[0].type
      value = auth.dns_resource_record[0].data
    }],
  )
}
