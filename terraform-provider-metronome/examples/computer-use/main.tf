# The pricing of a product that sells prepaid credit and stops at zero:
# sessions cost $0.20 for each hour they are awake and $0.28 a month for each
# GB of disk they keep, both paid from credit only.
#
#   export METRONOME_BEARER_TOKEN=...   # a sandbox token
#   tofu plan

terraform {
  required_providers {
    metronome = {
      source = "r33drichards/metronome"
    }
  }
}

provider "metronome" {}

# --- What is measured ---

# A metric's definition is fixed for good: to change one, add a metric with
# a new name (_v2), point the product at it, and only then remove this one.
resource "metronome_billable_metric" "awake_seconds" {
  name             = "cu_awake_seconds_v1"
  aggregation_type = "SUM"
  aggregation_key  = "seconds"
  event_type_filter = {
    in_values = ["session.awake"]
  }
  property_filters = [{ name = "seconds", exists = true }]
  group_keys       = [["session_id"]]
}

resource "metronome_billable_metric" "disk_gb_seconds" {
  name             = "cu_disk_gb_seconds_v1"
  aggregation_type = "SUM"
  aggregation_key  = "gb_seconds"
  event_type_filter = {
    in_values = ["session.kept"]
  }
  property_filters = [{ name = "gb_seconds", exists = true }]
  group_keys       = [["session_id"]]
}

# --- What is sold ---

resource "metronome_product" "awake" {
  name               = "Awake time"
  type               = "USAGE"
  billable_metric_id = metronome_billable_metric.awake_seconds.id
  quantity_conversion = {
    name              = "seconds to hours"
    conversion_factor = 3600
    operation         = "DIVIDE"
  }
}

resource "metronome_product" "disk" {
  name               = "Disk"
  type               = "USAGE"
  billable_metric_id = metronome_billable_metric.disk_gb_seconds.id
  quantity_conversion = {
    name              = "GB-seconds to GB-months of 730 hours"
    conversion_factor = 2628000
    operation         = "DIVIDE"
  }
}

# What every credit is attached to.
resource "metronome_product" "credit" {
  name = "Credit"
  type = "FIXED"
}

# --- What it costs ---

resource "metronome_rate_card" "standard" {
  name        = "Computer Use standard v1"
  description = "List price 0, the real price as the commit rate: usage is paid from credit, and costs nothing when there is none."
  aliases     = [{ name = "cu-standard-v1" }]
}

# Rates are only ever added. To change a price, change starting_at with it.
resource "metronome_rate" "awake" {
  rate_card_id = metronome_rate_card.standard.id
  product_id   = metronome_product.awake.id
  starting_at  = "2026-10-01T00:00:00Z"
  entitled     = true
  rate_type    = "FLAT"
  price        = 0
  commit_rate = {
    rate_type = "FLAT"
    price     = 20 # cents an hour
  }
}

resource "metronome_rate" "disk" {
  rate_card_id = metronome_rate_card.standard.id
  product_id   = metronome_product.disk.id
  starting_at  = "2026-10-01T00:00:00Z"
  entitled     = true
  rate_type    = "FLAT"
  price        = 0
  commit_rate = {
    rate_type = "FLAT"
    price     = 28 # cents a GB-month
  }
}

# --- Being told when credit is gone ---

resource "metronome_alert" "zero_balance" {
  name               = "cu-zero-balance"
  alert_type         = "low_remaining_contract_credit_and_commit_balance_reached"
  threshold          = 0
  uniqueness_key     = "cu-zero-balance"
  evaluate_on_create = false
}

# --- What a credit may carry ---

resource "metronome_custom_field_key" "credit" {
  for_each = toset(["grant_key", "source", "payment_intent"])

  entity             = "contract_credit"
  key                = each.key
  enforce_uniqueness = false
}

output "rate_card_id" {
  value = metronome_rate_card.standard.id
}
