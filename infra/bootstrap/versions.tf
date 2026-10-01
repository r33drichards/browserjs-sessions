terraform {
  required_version = ">= 1.8.0"

  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "~> 8.5"
    }
  }

  # State for this directory is local on purpose: it creates the bucket that
  # holds everyone else's state. README.md shows how to move it into that
  # bucket afterwards.
}

provider "google" {
  region = var.region
}

# The Budgets API bills and counts quota against a project the caller names,
# so it needs a provider that sends one. Everything else must NOT send one:
# on the first apply the project does not exist yet.
provider "google" {
  alias                 = "billing"
  region                = var.region
  billing_project       = var.project_id
  user_project_override = true
}
