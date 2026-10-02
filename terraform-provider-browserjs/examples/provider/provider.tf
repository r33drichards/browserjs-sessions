terraform {
  required_providers {
    browserjs = { source = "r33drichards/browserjs" }
  }
}

# The token is best left out of the configuration: set BROWSERJS_TOKEN.
provider "browserjs" {
  endpoint = "https://api.computeruse.site" # the default; or BROWSERJS_ENDPOINT
}
