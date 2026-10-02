# The public side: one address, the DNS zone and its records, and whatever the
# chosen edge_mode needs to get certificates.

locals {
  edge_nlb = var.edge_mode == "pomerium_nlb"
  edge_alb = var.edge_mode == "gateway_alb"

  fqdn = {
    api          = "${var.hostnames.api}.${var.domain}"
    app          = "${var.hostnames.app}.${var.domain}"
    authenticate = "${var.hostnames.authenticate}.${var.domain}"
    dex          = "${var.hostnames.dex}.${var.domain}"
    sessions     = "${var.hostnames.sessions}.${var.domain}"
  }

  # Names on the certificate and in DNS. Every session is under one host,
  # sessions.<domain>/<id>. The wildcard is where sessions were before, a
  # host each, kept while URLs handed out then are still in use: remove the
  # "sessions" entry (and rename nothing: a new key would replace a record)
  # once deploy/gke has dropped its legacy-session-* routes.
  public_names = {
    api           = local.fqdn.api
    app           = local.fqdn.app
    authenticate  = local.fqdn.authenticate
    dex           = local.fqdn.dex
    sessions_host = local.fqdn.sessions
    sessions      = "*.${local.fqdn.sessions}"
  }

  edge_ip = local.edge_nlb ? google_compute_address.edge[0].address : google_compute_global_address.edge[0].address

  zone_name = replace(var.domain, ".", "-")

  # The same names again under each of additional_domains: every public name
  # with the domain at its end exchanged, keyed "<domain>/<key of public_names>".
  additional_records = merge([
    for domain in var.additional_domains : {
      for key, name in local.public_names :
      "${domain}/${key}" => { domain = domain, name = "${trimsuffix(name, var.domain)}${domain}" }
    }
  ]...)
}

# --- Address ----------------------------------------------------------------------

# pomerium_nlb: a regional address for the passthrough Network Load Balancer
# that GKE creates for Pomerium's Service. The Service names it with the
# annotation networking.gke.io/load-balancer-ip-addresses.
resource "google_compute_address" "edge" {
  count = local.edge_nlb ? 1 : 0

  name         = "${var.name}-edge"
  region       = var.region
  address_type = "EXTERNAL"
  network_tier = "PREMIUM"
  description  = "Public address of Pomerium (browserjs sessions)"

  depends_on = [google_project_service.this]
}

# gateway_alb: a global address for the Gateway (spec.addresses, type
# NamedAddress).
resource "google_compute_global_address" "edge" {
  count = local.edge_alb ? 1 : 0

  name         = "${var.name}-edge"
  address_type = "EXTERNAL"
  ip_version   = "IPV4"
  description  = "Public address of the Gateway (browserjs sessions)"

  depends_on = [google_project_service.this]
}

# --- DNS ----------------------------------------------------------------------------

resource "google_dns_managed_zone" "this" {
  count = var.create_dns_zone ? 1 : 0

  name        = local.zone_name
  dns_name    = "${var.domain}."
  description = "browserjs sessions public zone"
  visibility  = "public"

  dnssec_config {
    state = var.enable_dnssec ? "on" : "off"
  }

  depends_on = [google_project_service.this]
}

resource "google_dns_record_set" "public" {
  for_each = var.create_dns_zone ? local.public_names : {}

  managed_zone = google_dns_managed_zone.this[0].name
  name         = "${each.value}."
  type         = "A"
  ttl          = var.dns_ttl
  rrdatas      = [local.edge_ip]
}

# Further domains (additional_domains): a zone each, with the records of the
# zone above, to the same address. Nothing is served under them until
# deploy/gke names them (Pomerium's routes, the certificate, Dex); the zones
# are there first so that a domain can be delegated, and its certificate
# issued, before anything moves to it (docs/domain-switch.md).
resource "google_dns_managed_zone" "domains" {
  for_each = var.create_dns_zone ? toset(var.additional_domains) : []

  name        = replace(each.value, ".", "-")
  dns_name    = "${each.value}."
  description = "browserjs sessions public zone"
  visibility  = "public"

  dnssec_config {
    state = var.enable_dnssec ? "on" : "off"
  }

  depends_on = [google_project_service.this]
}

resource "google_dns_record_set" "domains" {
  for_each = var.create_dns_zone ? local.additional_records : {}

  managed_zone = google_dns_managed_zone.domains[each.value.domain].name
  name         = "${each.value.name}."
  type         = "A"
  ttl          = var.dns_ttl
  rrdatas      = [local.edge_ip]
}

# --- pomerium_nlb: cert-manager issues the certificate in the cluster ------------------

# A wildcard certificate can only be proven by DNS-01: cert-manager writes a
# TXT record at _acme-challenge.<name> in the zone above. This is the identity
# it does that with, used through Workload Identity (no key is created).
resource "google_service_account" "cert_manager" {
  count = local.edge_nlb ? 1 : 0

  account_id   = "${var.name}-cert-manager"
  display_name = "cert-manager DNS-01 solver (${var.name})"
}

# The permissions cert-manager's documentation lists as the least-privilege
# alternative to roles/dns.admin. Granted on the project (below), so they
# cover every zone in it, those of additional_domains too.
resource "google_project_iam_custom_role" "dns01_solver" {
  count = local.edge_nlb ? 1 : 0

  role_id     = "${replace(var.name, "-", "_")}_dns01_solver"
  title       = "DNS-01 solver (${var.name})"
  description = "Create and delete ACME challenge records in Cloud DNS"
  permissions = [
    "dns.changes.create",
    "dns.changes.get",
    "dns.changes.list",
    "dns.managedZones.list",
    "dns.resourceRecordSets.create",
    "dns.resourceRecordSets.delete",
    "dns.resourceRecordSets.get",
    "dns.resourceRecordSets.list",
    "dns.resourceRecordSets.update",
  ]
}

resource "google_project_iam_member" "cert_manager_dns" {
  count = local.edge_nlb ? 1 : 0

  project = var.project_id
  role    = google_project_iam_custom_role.dns01_solver[0].id
  member  = google_service_account.cert_manager[0].member
}

# Lets cert-manager's Kubernetes ServiceAccount act as the Google one. The
# ServiceAccount must carry the annotation
#   iam.gke.io/gcp-service-account: <cert_manager_service_account_email>
resource "google_service_account_iam_member" "cert_manager_workload_identity" {
  count = local.edge_nlb ? 1 : 0

  service_account_id = google_service_account.cert_manager[0].name
  role               = "roles/iam.workloadIdentityUser"
  member             = "serviceAccount:${var.project_id}.svc.id.goog[${var.cert_manager_namespace}/${var.cert_manager_service_account}]"

  # The workload identity pool only exists once the cluster does.
  depends_on = [google_container_cluster.this]
}

# --- gateway_alb: Google-managed certificates in Certificate Manager -------------------

# One authorisation per name; the wildcard is authorised through its parent
# (sessions.<domain> covers *.sessions.<domain>).
resource "google_certificate_manager_dns_authorization" "this" {
  for_each = local.edge_alb ? local.fqdn : {}

  name        = "${var.name}-${each.key}"
  location    = "global"
  domain      = each.value
  type        = "FIXED_RECORD"
  description = "Proves control of ${each.value}"

  depends_on = [google_project_service.this]
}

# The CNAMEs Certificate Manager checks. Each must be the only record at its
# name.
resource "google_dns_record_set" "dns_authorization" {
  for_each = local.edge_alb && var.create_dns_zone ? local.fqdn : {}

  managed_zone = google_dns_managed_zone.this[0].name
  name         = google_certificate_manager_dns_authorization.this[each.key].dns_resource_record[0].name
  type         = google_certificate_manager_dns_authorization.this[each.key].dns_resource_record[0].type
  ttl          = 300
  rrdatas      = [google_certificate_manager_dns_authorization.this[each.key].dns_resource_record[0].data]
}

resource "google_certificate_manager_certificate" "this" {
  count = local.edge_alb ? 1 : 0

  name        = var.name
  location    = "global"
  description = "browserjs sessions: app, authenticate, dex and the session wildcard"

  managed {
    domains            = values(local.public_names)
    dns_authorizations = [for key in keys(local.fqdn) : google_certificate_manager_dns_authorization.this[key].id]
  }
}

# The Gateway names this map with the annotation networking.gke.io/certmap.
resource "google_certificate_manager_certificate_map" "this" {
  count = local.edge_alb ? 1 : 0

  name        = var.name
  description = "browserjs sessions Gateway"

  depends_on = [google_project_service.this]
}

resource "google_certificate_manager_certificate_map_entry" "this" {
  for_each = local.edge_alb ? local.public_names : {}

  name         = "${var.name}-${each.key}"
  map          = google_certificate_manager_certificate_map.this[0].name
  hostname     = each.value
  certificates = [google_certificate_manager_certificate.this[0].id]
}
