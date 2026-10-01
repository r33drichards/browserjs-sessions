locals {
  budget_enabled = var.budget_amount != null

  project_id     = var.create_project ? google_project.this[0].project_id : data.google_project.existing[0].project_id
  project_number = var.create_project ? google_project.this[0].number : data.google_project.existing[0].number

  state_bucket_name = coalesce(var.state_bucket_name, "${var.project_id}-tfstate")

  # Everything infra/main touches. Enabled here so that main never fails on a
  # disabled API half way through an apply.
  services = toset(concat(
    [
      "artifactregistry.googleapis.com",
      "certificatemanager.googleapis.com",
      "cloudresourcemanager.googleapis.com",
      "compute.googleapis.com",
      "container.googleapis.com",
      "dns.googleapis.com",
      "iam.googleapis.com",
      "iamcredentials.googleapis.com",
      "logging.googleapis.com",
      "monitoring.googleapis.com",
      "serviceusage.googleapis.com",
      "storage.googleapis.com",
    ],
    local.budget_enabled ? ["billingbudgets.googleapis.com"] : [],
  ))
}

# --- The project --------------------------------------------------------------

resource "google_project" "this" {
  count = var.create_project ? 1 : 0

  project_id      = var.project_id
  name            = var.project_name
  org_id          = var.org_id
  folder_id       = var.folder_id
  billing_account = var.billing_account
  labels          = var.labels

  # No "default" VPC with its wide-open firewall rules; main creates its own.
  auto_create_network = false

  # `tofu destroy` must not be able to delete the project.
  deletion_policy = "PREVENT"

  lifecycle {
    precondition {
      condition     = var.billing_account != null
      error_message = "billing_account is required when create_project is true: nothing below can be created in a project without billing."
    }
    precondition {
      condition     = var.org_id == null || var.folder_id == null
      error_message = "Set org_id or folder_id, not both."
    }
  }
}

data "google_project" "existing" {
  count = var.create_project ? 0 : 1

  project_id = var.project_id
}

# --- APIs ---------------------------------------------------------------------

resource "google_project_service" "this" {
  for_each = local.services

  project = local.project_id
  service = each.value

  # Destroying this configuration leaves the APIs on: turning them off under
  # resources that main still manages would break it.
  disable_on_destroy = false
}

# --- State bucket ---------------------------------------------------------------

resource "google_storage_bucket" "state" {
  project  = local.project_id
  name     = local.state_bucket_name
  location = upper(var.region)
  labels   = var.labels

  storage_class               = "STANDARD"
  uniform_bucket_level_access = true
  public_access_prevention    = "enforced"

  # Every write of the state keeps the previous object, so a bad apply or a
  # corrupted state can be rolled back.
  versioning {
    enabled = true
  }

  lifecycle_rule {
    condition {
      num_newer_versions = var.state_versions_to_keep
      with_state         = "ARCHIVED"
    }
    action {
      type = "Delete"
    }
  }

  lifecycle {
    prevent_destroy = true
  }

  depends_on = [google_project_service.this]
}

# --- Budget (optional) ----------------------------------------------------------

resource "google_billing_budget" "this" {
  count    = local.budget_enabled ? 1 : 0
  provider = google.billing

  billing_account = var.billing_account
  display_name    = "${var.project_id} monthly"

  budget_filter {
    projects               = ["projects/${local.project_number}"]
    credit_types_treatment = "INCLUDE_ALL_CREDITS"
  }

  amount {
    specified_amount {
      currency_code = var.budget_currency
      units         = tostring(var.budget_amount)
    }
  }

  # With no notification channels configured, the e-mails go to the billing
  # account's administrators and users.
  threshold_rules {
    threshold_percent = 0.5
  }
  threshold_rules {
    threshold_percent = 0.9
  }
  threshold_rules {
    threshold_percent = 1.0
  }
  threshold_rules {
    threshold_percent = 1.0
    spend_basis       = "FORECASTED_SPEND"
  }

  lifecycle {
    precondition {
      condition     = var.billing_account != null
      error_message = "billing_account is required when budget_amount is set."
    }
  }

  depends_on = [google_project_service.this]
}
