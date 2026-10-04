data "sessions" "all" {
  provider = browserjs
}

output "session_names" {
  value = [for s in data.sessions.all.sessions : s.name]
}
