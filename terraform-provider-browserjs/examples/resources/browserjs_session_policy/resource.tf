# The policy is a Rego module, kept in a file beside the configuration. This
# one restricts browser_execute, so it denies desktop_execute and the exec
# server: either could drive the browser around its rules.
resource "browserjs_session_policy" "research" {
  session_id  = browserjs_session.research.id
  managed_url = "https://github.com/example/infra/tree/main/browserjs"
  rego        = file("${path.module}/one-site.rego")
}

# A policy written in place, on a session that was made in the UI and is not
# managed here: only its policy is. The whole browser, and nothing else.
data "browserjs_session" "scratch" {
  name = "scratch"
}

resource "browserjs_session_policy" "scratch" {
  session_id  = data.browserjs_session.scratch.id
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
