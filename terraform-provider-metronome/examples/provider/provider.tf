terraform {
  required_providers {
    metronome = {
      source = "r33drichards/metronome"
    }
  }
}

# The token comes from the environment variable METRONOME_BEARER_TOKEN, which
# keeps it out of the configuration and the plan. It belongs to one
# environment of the account (sandbox or production), and so does everything
# this configuration creates.
provider "metronome" {}
