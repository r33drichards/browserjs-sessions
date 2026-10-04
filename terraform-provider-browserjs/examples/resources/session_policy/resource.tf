# The policy is a Rego module, kept in a file beside the configuration. This
# one restricts browser_execute, so it denies desktop_execute and the exec
# server: either could drive the browser around its rules.
resource "session_policy" "research" {
  provider = browserjs

  session_id  = session.research.id
  managed_url = "https://github.com/example/infra/tree/main/browserjs"
  rego        = file("${path.module}/one-site.rego")
}

# A policy written in place, on a session that was made in the UI and is not
# managed here: only its policy is. The whole browser, and nothing else.
data "session" "scratch" {
  provider = browserjs

  name = "scratch"
}

resource "session_policy" "scratch" {
  provider = browserjs

  session_id  = data.session.scratch.id
  managed_url = "https://github.com/example/infra/tree/main/browserjs"

  rego = <<-EOT
    package browserjs.policy

    import rego.v1

    allow_tool_call if {
    	input.server == "browser"
    	input.tool == "browser_execute"
    }
  EOT

  timeouts {
    update = "5m"
  }
}
