terraform {
  required_version = ">= 1.8.0"

  required_providers {
    # Stripe's own provider. 0.x and generated from Stripe's API description:
    # pinned exactly, and raised deliberately (docs/billing-iac.md, "Providers").
    stripe = {
      source  = "stripe/stripe"
      version = "0.3.0"
    }
    # Built from this repository (terraform-provider-metronome/) and installed
    # from a filesystem mirror by provider.sh: it is in no registry.
    metronome = {
      source  = "r33drichards/metronome"
      version = "0.1.0"
    }
  }

  # The bucket of infra/main, another prefix for each mode, so that the test
  # and live objects never share a state and neither shares one with the
  # cluster:
  #   tofu init -backend-config="bucket=<project>-tofu-state" -backend-config="prefix=billing/<mode>"
  backend "gcs" {}
}

# The key comes from STRIPE_API_KEY and the token from METRONOME_BEARER_TOKEN,
# set by the workflow from the secrets of the mode. Neither is ever a
# variable: a variable's value is written into a saved plan.
provider "stripe" {}

provider "metronome" {}
