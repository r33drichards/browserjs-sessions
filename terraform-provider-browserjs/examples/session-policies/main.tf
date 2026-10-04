# Three sessions and their policies, managed as code.
#
#   export BROWSERJS_ENDPOINT=https://api.computeruse.site   # the default
#   export BROWSERJS_TOKEN=bjs_...                        # from the Tokens page
#   tofu plan && tofu apply                               # or terraform

terraform {
  required_providers {
    browserjs = { source = "r33drichards/browserjs" }
  }
}

provider "browserjs" {
  # endpoint and token from BROWSERJS_ENDPOINT / BROWSERJS_TOKEN
}

locals {
  managed_url = "https://github.com/r33drichards/infra/tree/main/browserjs"
}

resource "session" "research" {
  provider = browserjs

  name = "research"

  # Destroying a session deletes its disk and the browser's logins.
  lifecycle {
    prevent_destroy = true
  }
}

# A policy is a Rego module. This one allows everything in the browser except
# script in the page, and denies desktop control and the shell: either could
# drive the browser around its rules, and the API warns when one is left open.
resource "session_policy" "research" {
  provider = browserjs

  session_id  = session.research.id
  managed_url = local.managed_url
  rego        = file("${path.module}/no-scripting.rego")
}

# The same policy on two more sessions: reuse is the configuration's job.
resource "session" "worker" {
  provider = browserjs

  for_each = toset(["worker-a", "worker-b"])
  name     = each.key
}

resource "session_policy" "worker" {
  provider = browserjs

  for_each    = session.worker
  session_id  = each.value.id
  managed_url = local.managed_url
  rego        = file("${path.module}/one-site.rego")
}

output "mcp_url" {
  value = session.research.mcp_url
}

output "worker_mcp_urls" {
  value = { for name, s in session.worker : name => s.mcp_url }
}

output "policy_versions" {
  value = merge(
    { research = session_policy.research.version },
    { for name, p in session_policy.worker : name => p.version },
  )
}
