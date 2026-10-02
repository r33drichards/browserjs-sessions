# Stripe: what is sold, where events go, and what the Customer Portal offers.
# Contract: docs/contracts/billing/stripe.md.
#
# Customers, Checkout Sessions, subscriptions and payments are made by the
# backend and by Stripe at run time; nothing here touches them.

# One product for each plan (the portal can only switch between products)
# and one for credit. Stripe gives them their IDs: the provider cannot set
# one (https://github.com/stripe/terraform-provider-stripe/issues/41), so the
# catalogue's productId is carried as metadata. The backend never needs a
# product ID: it finds prices by lookup key.
resource "stripe_product" "this" {
  for_each = local.products

  name   = each.value.name
  active = each.value.active
  metadata = {
    catalogue_product = each.key
    catalogue_key     = each.value.catalogue_key
    managed_by        = "opentofu"
  }
}

# A price's amount, currency and interval cannot change: the provider would
# replace the price, which deactivates the old one under existing
# subscribers. So an entry of the catalogue is never edited; see
# docs/billing-iac.md, "Changing a price".
resource "stripe_price" "plan" {
  for_each = local.plan_prices

  product     = stripe_product.this[each.value.productId].id
  lookup_key  = each.key
  currency    = local.currency
  unit_amount = each.value.amount
  active      = each.value.enabled

  recurring {
    interval = "month"
  }

  metadata = {
    catalogue_key = each.value.key
    credit_micros = tostring(each.value.creditMicros)
    managed_by    = "opentofu"
  }

  lifecycle {
    precondition {
      condition     = each.value.amount >= 50
      error_message = "Stripe's smallest charge is $0.50."
    }
  }
}

resource "stripe_price" "pack" {
  for_each = local.pack_prices

  product     = stripe_product.this[each.value.productId].id
  lookup_key  = each.key
  currency    = local.currency
  unit_amount = each.value.amount
  active      = each.value.enabled

  metadata = {
    catalogue_key = each.value.key
    credit_micros = tostring(each.value.creditMicros)
    managed_by    = "opentofu"
  }

  lifecycle {
    precondition {
      condition     = each.value.amount >= 50
      error_message = "Stripe's smallest charge is $0.50."
    }
  }
}

locals {
  # Exactly the events the backend handles (stripe.md, "Webhook").
  webhook_events = [
    "charge.dispute.closed",
    "charge.dispute.created",
    "charge.refunded",
    "checkout.session.async_payment_failed",
    "checkout.session.async_payment_succeeded",
    "checkout.session.completed",
    "customer.deleted",
    "customer.subscription.created",
    "customer.subscription.deleted",
    "customer.subscription.paused",
    "customer.subscription.resumed",
    "customer.subscription.updated",
    "customer.updated",
    "invoice.paid",
    "invoice.payment_failed",
    "payment_intent.payment_failed",
    "payment_intent.succeeded",
    "payment_method.attached",
    "payment_method.automatically_updated",
    "payment_method.detached",
    "payment_method.updated",
    "refund.created",
    "setup_intent.succeeded",
  ]
}

# Raising webhook_generation replaces the endpoint, which is how its signing
# secret is rotated.
resource "terraform_data" "webhook_generation" {
  input = var.webhook_generation
}

# Stripe returns the endpoint's signing secret once, when it is created. It
# is kept in the state (the private bucket) and nowhere else: the apply
# workflow copies it from there into the cluster's Secret `stripe-webhook`
# without printing it. Replacing this resource makes a new secret.
resource "stripe_webhook_endpoint" "backend" {
  url            = var.webhook_url
  description    = "Computer Use backend (${var.mode}); managed by OpenTofu, infra/billing"
  enabled_events = local.webhook_events
  api_version    = var.webhook_api_version

  metadata = {
    managed_by = "opentofu"
  }

  # A live key with mode = test, or the reverse, would put this state's
  # objects in the wrong half of the account. The workflow checks the key's
  # prefix before it plans; this is the same check from Stripe's side.
  lifecycle {
    replace_triggered_by = [terraform_data.webhook_generation]

    postcondition {
      condition     = self.livemode == (var.mode == "live")
      error_message = "The Stripe key is not a key of mode ${var.mode}: Stripe reports livemode = ${self.livemode}."
    }
  }
}

locals {
  # What the portal may switch between: each product that is sold, with its
  # prices that are sold.
  portal_products = [
    for id, product in local.plan_products : {
      product = stripe_product.this[id].id
      prices  = [for key, p in local.plan_prices : stripe_price.plan[key].id if p.productId == id && p.enabled]
    } if product.active
  ]
}

# The Customer Portal: cards, invoices, cancelling, and (when the provider
# allows) switching plan. Stripe has no call that deletes a configuration;
# destroying this one deactivates it.
resource "stripe_billing_portal_configuration" "this" {
  name               = "Computer Use"
  default_return_url = "${var.public_url}/billing"

  business_profile = {
    privacy_policy_url   = "${var.site_url}/legal/privacy"
    terms_of_service_url = "${var.site_url}/legal/terms"
  }

  features = {
    invoice_history       = { enabled = true }
    payment_method_update = { enabled = true }
    customer_update = {
      enabled         = true
      allowed_updates = ["email", "address", "tax_id"]
    }
    subscription_cancel = {
      enabled = true
      mode    = "at_period_end"
    }
    # An upgrade starts a new period, paid in full less the unused time of
    # the old; a downgrade waits for the period's end.
    subscription_update = var.portal_plan_switching ? {
      enabled                 = true
      default_allowed_updates = ["price"]
      proration_behavior      = "always_invoice"
      billing_cycle_anchor    = "now"
      products                = local.portal_products
      schedule_at_period_end = {
        conditions = [{ type = "decreasing_item_amount" }]
      }
      } : {
      enabled                 = false
      default_allowed_updates = null
      proration_behavior      = null
      billing_cycle_anchor    = null
      products                = null
      schedule_at_period_end  = null
    }
  }

  # The value the backend looks for (stripe.md); kept from the time a setup
  # command made this object.
  metadata = {
    managed_by = "stripe-setup"
  }
}
