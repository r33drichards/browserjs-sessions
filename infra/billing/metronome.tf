# Metronome: what is metered, what it costs, and the alert at zero.
# Contract: docs/contracts/billing/metronome.md, "Objects defined in
# OpenTofu". The backend and the observer find these objects by the names
# and the alias below, so each name here is a contract.
#
# Customers, contracts and credits are made by the backend at run time;
# usage events are sent by the observer.

locals {
  # A billable metric's definition is fixed for good. To change one: ADD an
  # entry with the next suffix (_v2), point the product at it, apply; the old
  # entry is removed only when nothing meters with it, and removing it
  # archives the metric (it needs its prevent_destroy lifted, deliberately).
  #
  # A size of session other than small (the catalogue's `sizes`) has a metric
  # of its own, on an event type of its own, rather than a `size` dimension
  # on cu_awake_seconds_v1: that metric's definition cannot be given one, and
  # a rate by dimension would mean replacing the product every customer's
  # usage is rated with. A new size is a new entry here, by itself.
  metronome_metrics = var.metronome_enabled ? merge({
    cu_awake_seconds_v1 = {
      event_type = "session.awake"
      property   = "seconds"
    }
    cu_disk_gb_seconds_v1 = {
      event_type = "session.kept"
      property   = "gb_seconds"
    }
    }, {
    for size, _ in local.awake_cents_per_hour_by_size : "cu_awake_${size}_seconds_v1" => {
      event_type = "session.awake.${size}"
      property   = "seconds"
    }
  }) : {}

  metronome_usage_products = var.metronome_enabled ? merge({
    "Awake time" = {
      metric          = "cu_awake_seconds_v1"
      conversion      = "seconds to hours"
      divide_by       = 3600
      commit_rate     = local.awake_cents_per_hour
      commit_rate_per = "hour"
    }
    "Disk" = {
      metric          = "cu_disk_gb_seconds_v1"
      conversion      = "GB-seconds to GB-months of 730 hours"
      divide_by       = 2628000
      commit_rate     = local.disk_cents_per_gb_month
      commit_rate_per = "GB-month"
    }
    }, {
    for size, cents in local.awake_cents_per_hour_by_size : "Awake time (${size})" => {
      metric          = "cu_awake_${size}_seconds_v1"
      conversion      = "seconds to hours"
      divide_by       = 3600
      commit_rate     = cents
      commit_rate_per = "hour"
    }
  }) : {}

  # Created or absent together with the rest.
  metronome_once = var.metronome_enabled ? toset(["this"]) : toset([])
}

resource "metronome_billable_metric" "this" {
  for_each = local.metronome_metrics

  name             = each.key
  aggregation_type = "SUM"
  aggregation_key  = each.value.property
  event_type_filter = {
    in_values = [each.value.event_type]
  }
  # Metronome wants the aggregated property named by a filter.
  property_filters = [{ name = each.value.property, exists = true }]
  group_keys       = [["session_id"]]

  # Archiving a metric that a product meters with cannot be undone, and a
  # changed definition would do it (replace). Refuse both in the plan.
  lifecycle {
    prevent_destroy = true
  }
}

resource "metronome_product" "usage" {
  for_each = local.metronome_usage_products

  name               = each.key
  type               = "USAGE"
  billable_metric_id = metronome_billable_metric.this[each.value.metric].id
  quantity_conversion = {
    name              = each.value.conversion
    conversion_factor = each.value.divide_by
    operation         = "DIVIDE"
  }

  lifecycle {
    prevent_destroy = true
  }
}

# What every credit is attached to.
resource "metronome_product" "credit" {
  for_each = local.metronome_once

  name = "Credit"
  type = "FIXED"

  lifecycle {
    prevent_destroy = true
  }
}

resource "metronome_rate_card" "standard" {
  for_each = local.metronome_once

  name        = "Computer Use standard v1"
  description = "List price 0, the real price as the commit rate: usage is paid from credit and costs nothing when there is none."
  aliases     = [{ name = "cu-standard-v1" }]

  lifecycle {
    prevent_destroy = true
  }
}

# List rate 0, the real price as the commit rate (Metronome's "guarantee
# zero overages" pattern): the balance never goes below zero and nothing is
# ever owed.
#
# A rate is never edited or removed, only followed by a later one: a change
# of price here must come with a later metronome_rates_starting_at.
resource "metronome_rate" "usage" {
  for_each = local.metronome_usage_products

  rate_card_id = metronome_rate_card.standard["this"].id
  product_id   = metronome_product.usage[each.key].id
  starting_at  = var.metronome_rates_starting_at
  entitled     = true
  rate_type    = "FLAT"
  price        = 0
  commit_rate = {
    rate_type = "FLAT"
    price     = each.value.commit_rate
  }
}

# Tells the backend, by webhook, that a customer's credit is used up. For
# every customer; not evaluated for those who already are at zero when it is
# created (every new account is, until its first grant).
resource "metronome_alert" "zero_balance" {
  for_each = local.metronome_once

  name               = "cu-zero-balance"
  alert_type         = "low_remaining_contract_credit_and_commit_balance_reached"
  threshold          = 0
  uniqueness_key     = "cu-zero-balance"
  evaluate_on_create = false
}

# What a credit carries: the key of what caused it, where it came from, and
# the payment a refund finds it by.
resource "metronome_custom_field_key" "credit" {
  for_each = var.metronome_enabled ? toset(["grant_key", "source", "payment_intent"]) : toset([])

  entity             = "contract_credit"
  key                = each.key
  enforce_uniqueness = false

  # Removing a key makes every value under it unreadable.
  lifecycle {
    prevent_destroy = true
  }
}
