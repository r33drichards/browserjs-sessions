data "browserjs_session" "by_name" {
  name = "research"
}

data "browserjs_session" "by_id" {
  id = "s-ab2cd"
}

output "mcp_url" {
  value = data.browserjs_session.by_name.mcp_url
}
