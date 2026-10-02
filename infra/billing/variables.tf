variable "mode" {
  description = "Which half of the two accounts this state describes: `test` is Stripe's sandbox with Metronome's sandbox, `live` is Stripe live mode with Metronome production. It must match the keys the workflow was given; the state prefix is billing/<mode>."
  type        = string

  validation {
    condition     = contains(["test", "live"], var.mode)
    error_message = "mode must be test or live."
  }
}

variable "catalogue_file" {
  description = "The catalogue: what is sold, at what price, and the two rates. The same file the backend and the observer read."
  type        = string
  default     = "../../docs/contracts/billing/catalogue.yaml"
}

variable "public_url" {
  description = "Where the app is served; the portal sends people back to <public_url>/billing."
  type        = string
  default     = "https://app.computeruse.site"
}

variable "site_url" {
  description = "Where the public site is served; the portal links to its terms and privacy pages."
  type        = string
  default     = "https://computeruse.site"
}

# --- Stripe ---

variable "webhook_url" {
  description = "The backend's Stripe webhook."
  type        = string
  default     = "https://api.computeruse.site/stripe/webhook"

  validation {
    condition     = startswith(var.webhook_url, "https://")
    error_message = "Stripe sends live events to https URLs only."
  }
}

variable "webhook_api_version" {
  description = "The Stripe API version events are rendered in: that of the backend's pinned stripe-go (v86: 2026-08-26.dahlia). null leaves it to the account's default. Changing it replaces nothing; Stripe applies it to later events."
  type        = string
  default     = "2026-08-26.dahlia"
}

variable "webhook_generation" {
  description = "Raise by one to replace the webhook endpoint and so rotate its signing secret (docs/billing-iac.md)."
  type        = number
  default     = 1
}

variable "portal_plan_switching" {
  description = "Whether the Customer Portal lets a subscriber switch plan. Off until stripe/stripe can read back features.subscription_update.products: in 0.3.0 any configuration that sets it fails on apply (https://github.com/stripe/terraform-provider-stripe/issues/53)."
  type        = bool
  default     = false
}

# --- Metronome ---

variable "metronome_enabled" {
  description = "Whether this mode's Metronome environment is managed. false leaves Metronome alone (the workflow then needs no Metronome token). Turning it off again once objects exist is refused by their prevent_destroy."
  type        = bool
  default     = true
}

variable "metronome_rates_starting_at" {
  description = "From when the rates of the catalogue apply (RFC 3339, on the hour). Rates are only ever added to a rate card: when a rate in the catalogue changes, move this to the moment the new price starts, in the same pull request."
  type        = string
  default     = "2026-10-01T00:00:00Z"

  validation {
    condition     = can(regex("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:00:00Z$", var.metronome_rates_starting_at))
    error_message = "metronome_rates_starting_at must be a UTC timestamp on the hour, such as 2026-10-01T00:00:00Z."
  }
}
