# A policy in the JSON format.
resource "browserjs_session_policy" "research" {
  session_id  = browserjs_session.research.id
  managed_url = "https://github.com/example/infra/tree/main/browserjs"

  json = jsonencode({
    version = 1
    allow   = { operations = ["*"] }
    deny    = { operations = ["evaluate", "setContent"] }
  })
}

# A policy in Rego, on a session that was made in the UI and is not managed
# here: only its policy is.
data "browserjs_session" "scratch" {
  name = "scratch"
}

resource "browserjs_session_policy" "scratch" {
  session_id  = data.browserjs_session.scratch.id
  managed_url = "https://github.com/example/infra/tree/main/browserjs"
  rego        = file("${path.module}/one-site.rego")

  timeouts {
    update = "5m"
  }
}
