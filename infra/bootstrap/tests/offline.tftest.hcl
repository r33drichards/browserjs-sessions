# Offline checks of the configuration's own logic. The provider is mocked:
# nothing here contacts Google Cloud or needs credentials.
#   tofu init -backend=false && tofu test

mock_provider "google" {}

mock_provider "google" {
  alias = "billing"
}

variables {
  project_id = "browserjs-sessions-test"
}

run "existing_project" {
  command = plan

  assert {
    condition     = length(google_project.this) == 0 && length(data.google_project.existing) == 1
    error_message = "By default the project is looked up, not created."
  }

  assert {
    condition     = google_storage_bucket.state.name == "browserjs-sessions-test-tfstate"
    error_message = "The state bucket name defaults to <project_id>-tfstate."
  }

  assert {
    condition     = google_storage_bucket.state.versioning[0].enabled && google_storage_bucket.state.uniform_bucket_level_access && google_storage_bucket.state.public_access_prevention == "enforced"
    error_message = "The state bucket must be versioned, uniformly access-controlled and never public."
  }

  assert {
    condition     = length(google_billing_budget.this) == 0 && !contains(keys(google_project_service.this), "billingbudgets.googleapis.com")
    error_message = "No budget and no Budgets API unless budget_amount is set."
  }
}

run "create_project_with_budget" {
  command = plan

  variables {
    create_project  = true
    billing_account = "01ABCD-234567-89EF01"
    budget_amount   = 300
  }

  assert {
    condition     = length(google_project.this) == 1 && google_project.this[0].deletion_policy == "PREVENT" && !google_project.this[0].auto_create_network
    error_message = "A created project is protected from deletion and has no default network."
  }

  assert {
    condition     = length(google_billing_budget.this) == 1 && contains(keys(google_project_service.this), "billingbudgets.googleapis.com")
    error_message = "budget_amount creates the budget and enables its API."
  }
}

run "create_project_needs_billing_account" {
  command = plan

  variables {
    create_project = true
  }

  expect_failures = [google_project.this]
}

run "budget_needs_billing_account" {
  command = plan

  variables {
    budget_amount = 300
  }

  expect_failures = [google_billing_budget.this]
}

run "rejects_bad_project_id" {
  command = plan

  variables {
    project_id = "Browserjs"
  }

  expect_failures = [var.project_id]
}
