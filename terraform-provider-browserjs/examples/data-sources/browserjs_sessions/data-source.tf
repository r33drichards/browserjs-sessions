data "browserjs_sessions" "all" {}

output "session_names" {
  value = [for s in data.browserjs_sessions.all.sessions : s.name]
}
