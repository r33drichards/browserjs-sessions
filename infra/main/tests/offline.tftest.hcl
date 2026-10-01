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
        name = "_acme-challenge.mock.browserjs.com."
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
    condition = toset([for record in google_dns_record_set.public : record.name]) == toset([
      "app.browserjs.com.",
      "authenticate.browserjs.com.",
      "dex.browserjs.com.",
      "*.sessions.browserjs.com.",
    ])
    error_message = "Expected A records for the four public names."
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
    condition     = length(google_container_node_pool.system.node_config[0].sandbox_config) == 0
    error_message = "The system pool must not run gVisor: GKE Sandbox needs one ordinary pool."
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
    condition     = endswith(google_storage_bucket_iam_member.snapshots_session_writer[0].member, "/subject/ns/browserjs-sessions/sa/session")
    error_message = "The snapshot writer must be the session ServiceAccount's Workload Identity principal."
  }

  assert {
    condition     = output.certificate_map_name == null && output.cert_manager_service_account_email != null
    error_message = "Outputs must follow the edge mode."
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
    condition     = length(google_certificate_manager_dns_authorization.this) == 4 && length(google_dns_record_set.dns_authorization) == 4
    error_message = "Expected a DNS authorisation and its CNAME for each of the four names."
  }

  assert {
    condition     = google_certificate_manager_dns_authorization.this["sessions"].domain == "sessions.browserjs.com"
    error_message = "The wildcard is authorised through its parent name."
  }

  assert {
    condition     = toset(google_certificate_manager_certificate.this[0].managed[0].domains) == toset(["app.browserjs.com", "authenticate.browserjs.com", "dex.browserjs.com", "*.sessions.browserjs.com"])
    error_message = "The certificate must cover the three hosts and the session wildcard."
  }

  assert {
    condition     = length(google_certificate_manager_certificate_map_entry.this) == 4
    error_message = "Expected a certificate map entry per public name."
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
    condition     = length(google_dns_managed_zone.this) == 0 && length(google_dns_record_set.public) == 0 && output.dns_name_servers == null
    error_message = "create_dns_zone = false must create no DNS resources."
  }

  assert {
    condition     = length(output.dns_records) == 4
    error_message = "The records to create by hand must still be listed."
  }
}

run "rejects_e2_session_nodes" {
  command = plan

  variables {
    session_machine_type = "e2-standard-4"
  }

  expect_failures = [var.session_machine_type]
}

run "rejects_unknown_edge_mode" {
  command = plan

  variables {
    edge_mode = "ingress"
  }

  expect_failures = [var.edge_mode]
}
