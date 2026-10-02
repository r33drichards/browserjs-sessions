terraform {
  required_providers {
    browserjs = { source = "r33drichards/browserjs" }
  }
}

# The token is best left out of the configuration: set BROWSERJS_TOKEN.
provider "browserjs" {
  endpoint = "https://api.browserjs.com" # the default; or BROWSERJS_ENDPOINT
}
