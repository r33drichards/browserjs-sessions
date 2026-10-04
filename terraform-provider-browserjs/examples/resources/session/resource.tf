resource "session" "research" {
  provider = browserjs

  name = "research"

  # Destroying a session deletes its disk and the browser's logins.
  lifecycle {
    prevent_destroy = true
  }
}

output "mcp_url" {
  value = session.research.mcp_url
}
