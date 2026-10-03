# Offline checks of the configuration's own logic, against the real
# catalogue. Both providers are mocked: nothing here contacts Stripe or
# Metronome or needs a key.
#   export TF_CLI_CONFIG_FILE="$(./provider.sh)"
#   tofu init -backend=false && tofu test

mock_provider "stripe" {
  mock_resource "stripe_webhook_endpoint" {
    defaults = {
      livemode = false
    }
  }
}

mock_provider "metronome" {
  # Computed IDs the mock would otherwise fill with random strings, where
  # the provider checks that what it is passed is a Metronome ID.
  mock_resource "metronome_billable_metric" {
    defaults = {
      id = "11111111-1111-4111-8111-111111111111"
    }
  }

  mock_resource "metronome_product" {
    defaults = {
      id = "22222222-2222-4222-8222-222222222222"
    }
  }

  mock_resource "metronome_rate_card" {
    defaults = {
      id                  = "33333333-3333-4333-8333-333333333333"
      fiat_credit_type_id = "2714e483-4ff1-48e4-9e25-ac732e8f24f2"
    }
  }

  mock_resource "metronome_rate" {
    defaults = {
      credit_type_id = "2714e483-4ff1-48e4-9e25-ac732e8f24f2"
    }
  }
}

variables {
  mode = "test"
}

run "stripe_objects_from_the_catalogue" {
  command = plan

  assert {
    condition     = toset(keys(stripe_product.this)) == toset(["cu_plan_starter", "cu_plan_pro", "cu_plan_scale", "cu_credit"])
    error_message = "One product for each plan and one for credit."
  }

  assert {
    condition     = stripe_product.this["cu_plan_pro"].name == "Computer Use Pro" && stripe_product.this["cu_credit"].name == "Computer Use credit"
    error_message = "Product names are those of the contract."
  }

  assert {
    condition     = toset(keys(stripe_price.plan)) == toset(["cu_starter_monthly_v1", "cu_pro_monthly_v1", "cu_scale_monthly_v1"])
    error_message = "One recurring price for each plan, by lookup key."
  }

  assert {
    condition     = toset(keys(stripe_price.pack)) == toset(["cu_credit_5_v1", "cu_credit_20_v1", "cu_credit_50_v1"])
    error_message = "One one-off price for each pack, by lookup key."
  }

  assert {
    condition = alltrue([
      stripe_price.plan["cu_starter_monthly_v1"].unit_amount == 500,
      stripe_price.plan["cu_pro_monthly_v1"].unit_amount == 2000,
      stripe_price.plan["cu_scale_monthly_v1"].unit_amount == 10000,
      stripe_price.pack["cu_credit_5_v1"].unit_amount == 500,
      stripe_price.pack["cu_credit_20_v1"].unit_amount == 2000,
      stripe_price.pack["cu_credit_50_v1"].unit_amount == 5000,
    ])
    error_message = "Amounts are the catalogue's, in cents."
  }

  assert {
    condition     = alltrue([for p in stripe_price.plan : p.currency == "usd" && p.recurring[0].interval == "month" && p.lookup_key != null])
    error_message = "Plans are monthly, in US dollars."
  }

  assert {
    condition     = alltrue([for p in stripe_price.pack : length(p.recurring) == 0])
    error_message = "Packs are one-off."
  }

  assert {
    condition     = stripe_price.plan["cu_pro_monthly_v1"].metadata.credit_micros == "44000000"
    error_message = "A price carries the credit it gives, for people."
  }

  # Scale is not sold yet: its product and price exist, inactive.
  assert {
    condition     = !stripe_product.this["cu_plan_scale"].active && !stripe_price.plan["cu_scale_monthly_v1"].active
    error_message = "A plan with enabled: false is inactive in Stripe."
  }

  assert {
    condition     = stripe_product.this["cu_plan_starter"].active && stripe_price.plan["cu_starter_monthly_v1"].active
    error_message = "A plan with enabled: true is active in Stripe."
  }
}

run "webhook_endpoint" {
  command = plan

  assert {
    condition     = stripe_webhook_endpoint.backend.url == "https://api.computeruse.site/stripe/webhook"
    error_message = "The endpoint is the backend's route."
  }

  assert {
    condition     = length(stripe_webhook_endpoint.backend.enabled_events) == 23 && length(distinct(stripe_webhook_endpoint.backend.enabled_events)) == 23
    error_message = "Exactly the 23 events of stripe.md, each once."
  }

  assert {
    condition = alltrue([for e in [
      "checkout.session.completed", "setup_intent.succeeded", "payment_method.detached", "customer.subscription.updated",
      "invoice.paid", "payment_intent.succeeded", "refund.created", "charge.dispute.created",
    ] : contains(stripe_webhook_endpoint.backend.enabled_events, e)])
    error_message = "The events the card gate, subscriptions, recharge, refunds and disputes depend on are subscribed."
  }

  assert {
    condition     = !contains(stripe_webhook_endpoint.backend.enabled_events, "*")
    error_message = "Never every event."
  }
}

run "portal_without_plan_switching" {
  command = plan

  assert {
    condition     = !stripe_billing_portal_configuration.this.features.subscription_update.enabled
    error_message = "Plan switching is off by default (stripe/stripe issue 53)."
  }

  assert {
    condition     = stripe_billing_portal_configuration.this.features.subscription_cancel.mode == "at_period_end"
    error_message = "Cancelling is at the period's end."
  }

  assert {
    condition     = stripe_billing_portal_configuration.this.metadata.managed_by == "stripe-setup"
    error_message = "The backend finds the configuration by this metadata."
  }

  assert {
    condition     = stripe_billing_portal_configuration.this.default_return_url == "https://app.computeruse.site/billing"
    error_message = "The portal returns to the billing page."
  }
}

run "portal_with_plan_switching" {
  command = plan

  variables {
    portal_plan_switching = true
  }

  assert {
    condition     = stripe_billing_portal_configuration.this.features.subscription_update.enabled
    error_message = "Plan switching is on."
  }

  # Starter and Pro are sold; Scale is not, and must not be offered.
  assert {
    condition     = length(local.portal_products) == 2 && alltrue([for p in local.portal_products : length(p.prices) == 1])
    error_message = "The portal offers each plan that is sold, with its one price."
  }

  assert {
    condition = alltrue([
      stripe_billing_portal_configuration.this.features.subscription_update.proration_behavior == "always_invoice",
      stripe_billing_portal_configuration.this.features.subscription_update.billing_cycle_anchor == "now",
      stripe_billing_portal_configuration.this.features.subscription_update.schedule_at_period_end.conditions[0].type == "decreasing_item_amount",
    ])
    error_message = "An upgrade is invoiced at once and starts a new period; a downgrade waits."
  }
}

run "metronome_objects" {
  command = plan

  assert {
    condition = toset(keys(metronome_billable_metric.this)) == toset([
      "cu_awake_seconds_v1", "cu_disk_gb_seconds_v1", "cu_awake_medium_seconds_v1", "cu_awake_large_seconds_v1",
    ])
    error_message = "The metrics, under the names the observer's events are matched by: awake time of each size, and the disk."
  }

  assert {
    condition = alltrue([
      metronome_billable_metric.this["cu_awake_seconds_v1"].aggregation_type == "SUM",
      metronome_billable_metric.this["cu_awake_seconds_v1"].aggregation_key == "seconds",
      metronome_billable_metric.this["cu_awake_seconds_v1"].event_type_filter.in_values == tolist(["session.awake"]),
      metronome_billable_metric.this["cu_awake_medium_seconds_v1"].event_type_filter.in_values == tolist(["session.awake.medium"]),
      metronome_billable_metric.this["cu_awake_large_seconds_v1"].event_type_filter.in_values == tolist(["session.awake.large"]),
      metronome_billable_metric.this["cu_awake_large_seconds_v1"].aggregation_key == "seconds",
      metronome_billable_metric.this["cu_awake_large_seconds_v1"].group_keys == tolist([tolist(["session_id"])]),
      metronome_billable_metric.this["cu_disk_gb_seconds_v1"].aggregation_key == "gb_seconds",
      metronome_billable_metric.this["cu_disk_gb_seconds_v1"].event_type_filter.in_values == tolist(["session.kept"]),
      metronome_billable_metric.this["cu_disk_gb_seconds_v1"].group_keys == tolist([tolist(["session_id"])]),
    ])
    error_message = "The metrics are those of metronome.md."
  }

  assert {
    condition = alltrue([
      metronome_product.usage["Awake time"].quantity_conversion.conversion_factor == 3600,
      metronome_product.usage["Disk"].quantity_conversion.conversion_factor == 2628000,
      metronome_product.usage["Disk"].quantity_conversion.operation == "DIVIDE",
      metronome_product.credit["this"].name == "Credit" && metronome_product.credit["this"].type == "FIXED",
    ])
    error_message = "Seconds become hours, GB-seconds become GB-months, and credit has its product."
  }

  assert {
    condition     = metronome_rate_card.standard["this"].aliases[0].name == "cu-standard-v1"
    error_message = "Contracts name the rate card by this alias."
  }

  # $0.20 an hour and $0.28 a GB-month, from the catalogue's micro-dollars.
  assert {
    condition = alltrue([
      metronome_rate.usage["Awake time"].price == 0 && metronome_rate.usage["Awake time"].commit_rate.price == 20,
      metronome_rate.usage["Disk"].price == 0 && metronome_rate.usage["Disk"].commit_rate.price == 28,
      metronome_rate.usage["Awake time (medium)"].price == 0 && metronome_rate.usage["Awake time (medium)"].commit_rate.price == 40,
      metronome_rate.usage["Awake time (large)"].price == 0 && metronome_rate.usage["Awake time (large)"].commit_rate.price == 80,
      metronome_product.usage["Awake time (large)"].quantity_conversion.conversion_factor == 3600,
      alltrue([for r in metronome_rate.usage : r.rate_type == "FLAT" && r.entitled && r.starting_at == "2026-10-01T00:00:00Z"]),
    ])
    error_message = "List rate 0, commit rates 20 and 28 cents."
  }

  assert {
    condition = alltrue([
      metronome_alert.zero_balance["this"].name == "cu-zero-balance",
      metronome_alert.zero_balance["this"].uniqueness_key == "cu-zero-balance",
      metronome_alert.zero_balance["this"].alert_type == "low_remaining_contract_credit_and_commit_balance_reached",
      metronome_alert.zero_balance["this"].threshold == 0,
      metronome_alert.zero_balance["this"].evaluate_on_create == false,
      metronome_alert.zero_balance["this"].customer_id == null,
    ])
    error_message = "The zero-balance alert is that of metronome.md, for every customer."
  }

  assert {
    condition     = toset(keys(metronome_custom_field_key.credit)) == toset(["grant_key", "source", "payment_intent"]) && alltrue([for k in metronome_custom_field_key.credit : k.entity == "contract_credit"])
    error_message = "The three custom field keys of a credit."
  }
}

run "metronome_can_be_left_out" {
  command = plan

  variables {
    metronome_enabled = false
  }

  assert {
    condition = alltrue([
      length(metronome_billable_metric.this) == 0, length(metronome_product.usage) == 0, length(metronome_product.credit) == 0,
      length(metronome_rate_card.standard) == 0, length(metronome_rate.usage) == 0, length(metronome_alert.zero_balance) == 0,
      length(metronome_custom_field_key.credit) == 0,
    ])
    error_message = "With metronome_enabled = false nothing of Metronome's is managed."
  }

  assert {
    condition     = length(stripe_price.plan) == 3
    error_message = "Stripe is managed all the same."
  }
}

run "a_mode_that_is_neither" {
  command = plan

  variables {
    mode = "staging"
  }

  expect_failures = [var.mode]
}

run "a_rate_start_off_the_hour" {
  command = plan

  variables {
    metronome_rates_starting_at = "2026-10-01T00:30:00Z"
  }

  expect_failures = [var.metronome_rates_starting_at]
}
