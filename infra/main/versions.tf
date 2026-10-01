terraform {
  required_version = ">= 1.8.0"

  required_providers {
    google = {
      source = "hashicorp/google"
      # addons_config.agent_sandbox_config is GA from 7.39.0 and
      # addons_config.pod_snapshot_config from 7.33.0; 8.5.0 is what this
      # configuration was validated against.
      version = "~> 8.5"
    }
  }

  # The bucket comes from infra/bootstrap and is given at init time:
  #   tofu init -backend-config="bucket=<state_bucket output>"
  backend "gcs" {
    prefix = "main"
  }
}

provider "google" {
  project = var.project_id
  region  = var.region

  default_labels = var.labels
}
