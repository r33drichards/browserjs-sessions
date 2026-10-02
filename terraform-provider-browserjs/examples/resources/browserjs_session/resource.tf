resource "browserjs_session" "research" {
  name = "research"

  # Destroying a session deletes its disk and the browser's logins.
  lifecycle {
    prevent_destroy = true
  }
}

output "mcp_url" {
  value = browserjs_session.research.mcp_url
}
