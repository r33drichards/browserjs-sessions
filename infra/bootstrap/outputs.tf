output "project_id" {
  description = "The project. Use it as project_id in infra/main/terraform.tfvars."
  value       = local.project_id
}

output "project_number" {
  description = "The project's number (it appears in service-agent e-mail addresses)."
  value       = local.project_number
}

output "state_bucket" {
  description = "The OpenTofu state bucket. Pass it to infra/main: tofu init -backend-config=\"bucket=<this>\"."
  value       = google_storage_bucket.state.name
}

output "main_init_command" {
  description = "The init command for infra/main."
  value       = "tofu init -backend-config=\"bucket=${google_storage_bucket.state.name}\""
}

output "enabled_services" {
  description = "APIs enabled in the project."
  value       = sort(local.services)
}
