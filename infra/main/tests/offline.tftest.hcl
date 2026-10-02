# Offline checks of the configuration's own logic. The provider is mocked:
# nothing here contacts Google Cloud or needs credentials.
#   tofu init -backend=false && tofu test

mock_provider "google" {
  # Computed values the mock would otherwise fill with random strings, where
  # the provider validates the shape of what they are passed on to.
  mock_resource "google_service_account" {
    defaults = {
      email  = "mock-account@browserjs-sessions-test.iam.gserviceaccount.com"
      member = "serviceAccount:mock-account@browserjs-sessions-test.iam.gserviceaccount.com"
      name   = "projects/browserjs-sessions-test/serviceAccounts/mock-account@browserjs-sessions-test.iam.gserviceaccount.com"
    }
  }

  mock_data "google_project" {
    defaults = {
      number = "123456789012"
    }
  }

  mock_resource "google_certificate_manager_dns_authorization" {
    defaults = {
      dns_resource_record = [{
        name = "_acme-challenge.mock.computeruse.site."
        type = "CNAME"
        data = "mock.authorize.certificatemanager.goog."
      }]
    }
  }

  mock_resource "google_compute_address" {
    defaults = {
      address = "203.0.113.10"
    }
  }

  mock_resource "google_compute_global_address" {
    defaults = {
      address = "203.0.113.20"
    }
  }
}

variables {
  project_id = "browserjs-sessions-test"
}

run "defaults_pomerium_nlb" {
  command = plan

  assert {
    condition     = length(google_compute_address.edge) == 1 && length(google_compute_global_address.edge) == 0
    error_message = "pomerium_nlb wants one regional address and no global one."
  }

  assert {
    condition     = length(google_certificate_manager_certificate.this) == 0 && length(google_certificate_manager_dns_authorization.this) == 0
    error_message = "pomerium_nlb must not create Certificate Manager resources: their CNAME would collide with cert-manager's TXT record."
  }

  assert {
    condition     = length(google_service_account.cert_manager) == 1
    error_message = "pomerium_nlb needs cert-manager's service account."
  }

  assert {
    condition = toset([for record in google_dns_record_set.domains : record.name]) == toset([
      "api.computeruse.site.",
      "app.computeruse.site.",
      "authenticate.computeruse.site.",
      "dex.computeruse.site.",
      "sessions.computeruse.site.",
      "*.sessions.computeruse.site.",
    ])
    error_message = "Expected A records for the six public names."
  }

  assert {
    condition     = google_container_cluster.this.location == "us-west1-a" && try(length(google_container_cluster.this.node_locations), 0) == 0
    error_message = "A zonal cluster must not repeat its own zone in node_locations."
  }

  assert {
    condition     = google_container_node_pool.sessions.autoscaling[0].min_node_count == 0
    error_message = "The session pool must be able to scale to zero."
  }

  assert {
    condition     = google_container_node_pool.sessions.node_config[0].sandbox_config[0].type == "GVISOR"
    error_message = "The session pool must run gVisor."
  }

  assert {
    condition = alltrue([
      for pool in concat([google_container_node_pool.sessions], values(google_container_node_pool.sessions_fallback)) :
      pool.node_config[0].gcfs_config[0].enabled
    ])
    error_message = "Every session pool must have image streaming on."
  }

  assert {
    condition     = length(google_container_node_pool.system.node_config[0].gcfs_config) == 0
    error_message = "The system pool is not changed: turning image streaming on would recreate its node."
  }

  assert {
    condition     = contains(keys(google_project_service.this), "containerfilesystem.googleapis.com") && google_project_iam_member.nodes_image_streaming.role == "roles/serviceusage.serviceUsageConsumer"
    error_message = "Image streaming needs the Container File System API and the Service Usage Consumer role for the node service account."
  }

  assert {
    condition     = length(google_container_node_pool.system.node_config[0].sandbox_config) == 0
    error_message = "The system pool must not run gVisor: GKE Sandbox needs one ordinary pool."
  }

  assert {
    condition     = !contains(keys(google_project_service.this), "certificatemanager.googleapis.com") && contains(keys(google_project_service.this), "container.googleapis.com")
    error_message = "pomerium_nlb enables the GKE API but not Certificate Manager."
  }

  assert {
    condition     = google_container_cluster.this.deletion_protection
    error_message = "Deletion protection must default to on."
  }

  assert {
    condition     = length(google_storage_bucket_iam_member.snapshots_session_writer) == 1 && length(google_storage_bucket_iam_member.snapshots_node_agent) == 0
    error_message = "podKSA grants the session ServiceAccount, not the node service agent."
  }

  assert {
    condition     = google_storage_bucket.billing_export.name == "browserjs-sessions-test-billing-export" && google_storage_bucket.billing_export.versioning[0].enabled && one(google_storage_bucket.billing_export.lifecycle_rule[0].condition).age == 90
    error_message = "The ledger's export bucket is <project>-billing-export, versioned, and keeps 90 days."
  }

  assert {
    condition     = google_storage_bucket_iam_member.billing_export_writer.role == "roles/storage.objectCreator" && endswith(google_storage_bucket_iam_member.billing_export_writer.member, "/subject/ns/browserjs-sessions/sa/billing-export")
    error_message = "Only the billing-export ServiceAccount's Workload Identity principal may add an export, and it may do nothing else."
  }

  assert {
    condition     = endswith(google_storage_bucket_iam_member.snapshots_session_writer[0].member, "/subject/ns/browserjs-sessions/sa/session")
    error_message = "The snapshot writer must be the session ServiceAccount's Workload Identity principal."
  }

  assert {
    condition     = google_service_account_iam_member.images_push_github.member == "principalSet://iam.googleapis.com/projects/123456789012/locations/global/workloadIdentityPools/github/attribute.repo_ref/r33drichards/browserjs-sessions@refs/heads/main"
    error_message = "Image pushes must be limited to the main branch of the repository, through the bootstrap's pool."
  }

  assert {
    condition     = google_service_account_iam_member.images_push_github.role == "roles/iam.workloadIdentityUser" && google_artifact_registry_repository_iam_member.images_push.role == "roles/artifactregistry.writer"
    error_message = "images-push may be impersonated and may write to the image repository; nothing else."
  }

  assert {
    condition     = google_service_account_iam_member.deployer_github.member == "principalSet://iam.googleapis.com/projects/123456789012/locations/global/workloadIdentityPools/github/attribute.repo_ref/r33drichards/browserjs-sessions@refs/heads/main"
    error_message = "Deploys must be limited to the main branch of the repository, through the bootstrap's pool."
  }

  assert {
    condition     = google_service_account_iam_member.deployer_github.role == "roles/iam.workloadIdentityUser" && google_project_iam_member.deployer_cluster.role == "roles/container.admin"
    error_message = "deployer may be impersonated and may administer Kubernetes Engine; nothing else."
  }

  assert {
    condition     = google_service_account.deployer.account_id == "deployer" && output.deployer_service_account_email != null
    error_message = "The deployer account must exist and be an output (the repository variable DEPLOY_SA)."
  }

  assert {
    condition     = output.certificate_map_name == null && output.cert_manager_service_account_email != null
    error_message = "Outputs must follow the edge mode."
  }
}

# The one deployment, as terraform.tfvars has it: served under
# computeruse.site, with the zone of browserjs.com kept. The resource
# addresses asserted here are the ones in its state: another address for the
# same zone or record would delete it and make it again.
run "previous_domain_keeps_its_addresses" {
  command = plan

  variables {
    domain             = "computeruse.site"
    previous_domain    = "browserjs.com"
    additional_domains = []
  }

  assert {
    condition     = keys(google_dns_managed_zone.domains) == ["computeruse.site"] && google_dns_managed_zone.domains["computeruse.site"].dns_name == "computeruse.site." && google_dns_managed_zone.domains["computeruse.site"].name == "computeruse-site"
    error_message = "The served domain's zone is google_dns_managed_zone.domains[\"computeruse.site\"], named computeruse-site."
  }

  assert {
    condition     = google_dns_record_set.site[0].name == "computeruse.site." && google_dns_record_set.site[0].managed_zone == "computeruse-site" && google_dns_record_set.site[0].type == "A"
    error_message = "The public site is an A record on the served domain itself, in that domain's zone."
  }

  assert {
    condition     = google_dns_record_set.www[0].name == "www.computeruse.site." && google_dns_record_set.www[0].managed_zone == "computeruse-site" && google_dns_record_set.www[0].type == "A"
    error_message = "www is an A record in the served domain's zone."
  }

  assert {
    condition     = length(google_dns_managed_zone.this) == 1 && google_dns_managed_zone.this[0].dns_name == "browserjs.com." && google_dns_managed_zone.this[0].name == "browserjs-com"
    error_message = "The previous domain's zone is google_dns_managed_zone.this[0], named browserjs-com."
  }

  assert {
    condition = { for key, record in google_dns_record_set.domains : key => record.name } == {
      "computeruse.site/api"           = "api.computeruse.site."
      "computeruse.site/app"           = "app.computeruse.site."
      "computeruse.site/authenticate"  = "authenticate.computeruse.site."
      "computeruse.site/dex"           = "dex.computeruse.site."
      "computeruse.site/sessions_host" = "sessions.computeruse.site."
      "computeruse.site/sessions"      = "*.sessions.computeruse.site."
    }
    error_message = "The served domain's records are keyed <domain>/<key of public_names>."
  }

  assert {
    condition = { for key, record in google_dns_record_set.public : key => record.name } == {
      api           = "api.browserjs.com."
      app           = "app.browserjs.com."
      authenticate  = "authenticate.browserjs.com."
      dex           = "dex.browserjs.com."
      sessions_host = "sessions.browserjs.com."
      sessions      = "*.sessions.browserjs.com."
    }
    error_message = "The previous domain's records are keyed by the key of public_names alone."
  }

  assert {
    condition = alltrue([
      for record in google_dns_record_set.domains : record.managed_zone == "computeruse-site" && record.type == "A" && record.ttl == 300
      ]) && alltrue([
      for record in google_dns_record_set.public : record.managed_zone == "browserjs-com" && record.type == "A" && record.ttl == 300
    ])
    error_message = "Each record is an A record in the zone of its own domain."
  }

  assert {
    condition     = output.hostnames.app == "app.computeruse.site" && output.dns_zone_name == "computeruse-site" && toset(output.certificate_dns_names) == toset(["api.computeruse.site", "app.computeruse.site", "authenticate.computeruse.site", "dex.computeruse.site", "sessions.computeruse.site", "*.sessions.computeruse.site"])
    error_message = "Only domain is served: the hostnames and the certificate's names are under it alone."
  }

  assert {
    condition     = keys(output.additional_dns_name_servers) == ["browserjs.com"] && toset(output.additional_dns_records[*].name) == toset(["api.browserjs.com", "app.browserjs.com", "authenticate.browserjs.com", "dex.browserjs.com", "sessions.browserjs.com", "*.sessions.browserjs.com"])
    error_message = "The previous domain's nameservers and records must be outputs, apart from the served domain's."
  }
}

# The same zones and records at the same addresses with the domains the other
# way round: what the configuration was before the move, and what undoing the
# move by terraform.tfvars alone gives.
run "served_under_the_previous_domain" {
  command = plan

  variables {
    domain             = "browserjs.com"
    previous_domain    = "browserjs.com"
    additional_domains = ["computeruse.site"]
  }

  assert {
    condition     = keys(google_dns_managed_zone.domains) == ["computeruse.site"] && google_dns_managed_zone.this[0].dns_name == "browserjs.com."
    error_message = "Which domain is served must not change which resource holds which zone."
  }

  assert {
    condition     = google_dns_record_set.domains["computeruse.site/app"].name == "app.computeruse.site." && google_dns_record_set.public["app"].name == "app.browserjs.com." && length(google_dns_record_set.domains) == 6 && length(google_dns_record_set.public) == 6
    error_message = "Which domain is served must not change which resource holds which record."
  }

  assert {
    condition     = output.hostnames.app == "app.browserjs.com" && output.dns_zone_name == "browserjs-com" && keys(output.additional_dns_name_servers) == ["computeruse.site"]
    error_message = "The outputs follow the served domain."
  }
}

run "one_domain" {
  command = plan

  variables {
    domain             = "example.org"
    previous_domain    = null
    additional_domains = []
  }

  assert {
    condition     = keys(google_dns_managed_zone.domains) == ["example.org"] && length(google_dns_managed_zone.this) == 0 && length(google_dns_record_set.public) == 0 && length(google_dns_record_set.domains) == 6
    error_message = "A deployment with one domain has one zone, in google_dns_managed_zone.domains."
  }

  assert {
    condition     = output.dns_zone_name == "example-org" && output.additional_dns_name_servers == {} && length(output.additional_dns_records) == 0
    error_message = "With one domain there is nothing additional."
  }
}

run "gateway_alb" {
  command = plan

  variables {
    edge_mode = "gateway_alb"
  }

  assert {
    condition     = length(google_compute_global_address.edge) == 1 && length(google_compute_address.edge) == 0
    error_message = "gateway_alb wants one global address and no regional one."
  }

  assert {
    condition     = length(google_certificate_manager_dns_authorization.this) == 5 && length(google_dns_record_set.dns_authorization) == 5
    error_message = "Expected a DNS authorisation and its CNAME for each of the five names (the sessions' host and its wildcard share one)."
  }

  assert {
    condition     = google_certificate_manager_dns_authorization.this["sessions"].domain == "sessions.computeruse.site"
    error_message = "The sessions' host and the wildcard under it are authorised through the one name."
  }

  assert {
    condition     = toset(google_certificate_manager_certificate.this[0].managed[0].domains) == toset(["api.computeruse.site", "app.computeruse.site", "authenticate.computeruse.site", "dex.computeruse.site", "sessions.computeruse.site", "*.sessions.computeruse.site"])
    error_message = "The certificate must cover the five hosts and the session wildcard."
  }

  assert {
    condition     = length(google_certificate_manager_certificate_map_entry.this) == 6
    error_message = "Expected a certificate map entry per public name."
  }

  assert {
    condition     = contains(keys(google_project_service.this), "certificatemanager.googleapis.com")
    error_message = "gateway_alb must enable the Certificate Manager API."
  }

  assert {
    condition     = length(google_service_account.cert_manager) == 0
    error_message = "gateway_alb must not create cert-manager's service account."
  }
}

run "federated_tokens_and_regional_cluster" {
  command = plan

  variables {
    snapshot_token_source = "federatedP4SA"
    cluster_location      = "us-west1"
    node_zones            = ["us-west1-b"]
    create_dns_zone       = false
  }

  assert {
    condition     = length(google_storage_bucket_iam_member.snapshots_node_agent) == 1 && length(google_storage_bucket_iam_member.snapshots_session_writer) == 0
    error_message = "federatedP4SA grants the node service agent only."
  }

  assert {
    condition     = google_container_cluster.this.node_locations == toset(["us-west1-b"])
    error_message = "A regional cluster pins its nodes with node_locations."
  }

  assert {
    condition     = length(google_dns_managed_zone.this) == 0 && length(google_dns_record_set.public) == 0 && length(google_dns_managed_zone.domains) == 0 && length(google_dns_record_set.domains) == 0 && length(google_dns_record_set.site) == 0 && length(google_dns_record_set.www) == 0 && output.dns_name_servers == null
    error_message = "create_dns_zone = false must create no DNS resources."
  }

  assert {
    condition     = length(output.dns_records) == 6
    error_message = "The records to create by hand must still be listed."
  }
}

run "rejects_cluster_outside_region" {
  command = plan

  variables {
    cluster_location = "us-central1-a"
  }

  expect_failures = [google_container_cluster.this]
}

run "rejects_e2_session_nodes" {
  command = plan

  variables {
    session_machine_type = "e2-standard-4"
  }

  expect_failures = [var.session_machine_type]
}

run "rejects_pull_request_push_ref" {
  command = plan

  variables {
    images_push_ref = "refs/pull/1/merge"
  }

  expect_failures = [var.images_push_ref]
}

run "rejects_pull_request_deploy_ref" {
  command = plan

  variables {
    deploy_ref = "refs/pull/1/merge"
  }

  expect_failures = [var.deploy_ref]
}

run "rejects_unknown_edge_mode" {
  command = plan

  variables {
    edge_mode = "ingress"
  }

  expect_failures = [var.edge_mode]
}
