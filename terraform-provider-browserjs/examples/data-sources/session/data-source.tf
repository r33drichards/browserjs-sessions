data "session" "by_name" {
  provider = browserjs

  name = "research"
}

data "session" "by_id" {
  provider = browserjs

  id = "s-ab2cd"
}

output "mcp_url" {
  value = data.session.by_name.mcp_url
}
