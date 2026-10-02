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

  # The bucket is created by infra/bootstrap/bootstrap.sh and given at init
  # time (GitHub Actions passes the TOFU_STATE_BUCKET repository variable):
  #   tofu init -backend-config="bucket=<project>-tofu-state"
  backend "gcs" {
    prefix = "main"
  }
}

provider "google" {
  project = var.project_id
  region  = var.region

  default_labels = var.labels
}
