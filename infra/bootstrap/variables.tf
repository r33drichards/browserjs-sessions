variable "project_id" {
  description = <<-EOT
    ID of the Google Cloud project that holds everything. Project IDs are
    globally unique and can never be reused, so plain "browserjs-sessions" is
    probably taken: add a short suffix, e.g. "browserjs-sessions-7f3a"
    (`openssl rand -hex 2` gives one).
  EOT
  type        = string

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{4,28}[a-z0-9]$", var.project_id))
    error_message = "A project ID is 6-30 characters: lowercase letters, digits and hyphens, starting with a letter and not ending with a hyphen."
  }
}

variable "create_project" {
  description = <<-EOT
    true: create the project here (needs roles/resourcemanager.projectCreator
    on the organisation or folder, or a personal account with no organisation,
    plus roles/billing.user on the billing account).
    false: the project already exists with billing attached; only enable APIs
    and create the state bucket in it.
  EOT
  type        = bool
  default     = false
}

variable "project_name" {
  description = "Display name of the project. Only used when create_project is true."
  type        = string
  default     = "browserjs sessions"

  validation {
    condition     = length(var.project_name) >= 4 && length(var.project_name) <= 30
    error_message = "A project display name is 4-30 characters."
  }
}

variable "org_id" {
  description = "Numeric organisation ID to create the project under. Leave null for a folder, or for a personal account with no organisation."
  type        = string
  default     = null

  validation {
    condition     = var.org_id == null || can(regex("^[0-9]+$", var.org_id))
    error_message = "org_id is the numeric ID, without the \"organizations/\" prefix."
  }
}

variable "folder_id" {
  description = "Numeric folder ID to create the project under. Mutually exclusive with org_id."
  type        = string
  default     = null

  validation {
    condition     = var.folder_id == null || can(regex("^[0-9]+$", var.folder_id))
    error_message = "folder_id is the numeric ID, without the \"folders/\" prefix."
  }
}

variable "billing_account" {
  description = "Billing account ID (XXXXXX-XXXXXX-XXXXXX). Required when create_project is true or budget_amount is set."
  type        = string
  default     = null

  validation {
    condition     = var.billing_account == null || can(regex("^[0-9A-F]{6}-[0-9A-F]{6}-[0-9A-F]{6}$", var.billing_account))
    error_message = "A billing account ID looks like 01ABCD-234567-89EF01."
  }
}

variable "region" {
  description = "Region for the state bucket. Keep it the same as the cluster's."
  type        = string
  default     = "us-west1"

  validation {
    condition     = can(regex("^[a-z]+-[a-z]+[0-9]+$", var.region))
    error_message = "Expected a region such as us-west1."
  }
}

variable "state_bucket_name" {
  description = "Name of the OpenTofu state bucket. Bucket names are globally unique; the default is <project_id>-tfstate."
  type        = string
  default     = null

  validation {
    condition     = var.state_bucket_name == null || can(regex("^[a-z0-9][a-z0-9_.-]{1,61}[a-z0-9]$", var.state_bucket_name))
    error_message = "A bucket name is 3-63 characters: lowercase letters, digits, hyphens, underscores and dots."
  }
}

variable "state_versions_to_keep" {
  description = "How many superseded versions of each state file to keep."
  type        = number
  default     = 30

  validation {
    condition     = var.state_versions_to_keep >= 1 && floor(var.state_versions_to_keep) == var.state_versions_to_keep
    error_message = "Keep at least one old version."
  }
}

variable "budget_amount" {
  description = <<-EOT
    Monthly budget for the project, in whole units of budget_currency. null
    creates no budget. A budget only sends e-mail; it never stops spending.
    Creating one needs roles/billing.costsManager (or billing.admin) on the
    billing account, which a project owner does not have by default.
  EOT
  type        = number
  default     = null

  validation {
    condition     = var.budget_amount == null || (var.budget_amount > 0 && floor(var.budget_amount) == var.budget_amount)
    error_message = "budget_amount is a positive whole number."
  }
}

variable "budget_currency" {
  description = "ISO 4217 currency of budget_amount. It must be the billing account's currency."
  type        = string
  default     = "USD"

  validation {
    condition     = can(regex("^[A-Z]{3}$", var.budget_currency))
    error_message = "Use a three-letter currency code such as USD."
  }
}

variable "labels" {
  description = "Labels put on the project (when created here) and the state bucket."
  type        = map(string)
  default = {
    app        = "browserjs-sessions"
    managed-by = "opentofu"
  }
}
