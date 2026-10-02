# Nothing at run time depends on these: the backend finds prices by lookup
# key and Metronome's objects by name. They are for people, and for the
# apply workflow, which reads only webhook_secret.

output "mode" {
  description = "test or live."
  value       = var.mode
}

output "stripe_livemode" {
  description = "What Stripe says the key was: false for a sandbox or test key. The apply workflow refuses a state whose mode disagrees."
  value       = stripe_webhook_endpoint.backend.livemode
}

output "stripe_product_ids" {
  description = "Stripe product IDs by the catalogue's productId."
  value       = { for id, p in stripe_product.this : id => p.id }
}

output "stripe_price_ids" {
  description = "Stripe price IDs by lookup key."
  value       = merge({ for k, p in stripe_price.plan : k => p.id }, { for k, p in stripe_price.pack : k => p.id })
}

output "stripe_portal_configuration_id" {
  description = "The Customer Portal configuration (bpc_...)."
  value       = stripe_billing_portal_configuration.this.id
}

output "stripe_webhook_endpoint_id" {
  description = "The webhook endpoint (we_...)."
  value       = stripe_webhook_endpoint.backend.id
}

output "stripe_webhook_secret" {
  description = "The endpoint's signing secret (whsec_...). Never printed: the apply workflow pipes it into the cluster's Secret."
  value       = stripe_webhook_endpoint.backend.secret
  sensitive   = true
}

output "metronome" {
  description = "Metronome IDs, by the names the backend looks them up with. Empty when Metronome is not managed."
  value = {
    billable_metrics = { for k, m in metronome_billable_metric.this : k => m.id }
    products = merge(
      { for k, p in metronome_product.usage : k => p.id },
      { for k, p in metronome_product.credit : "Credit" => p.id },
    )
    rate_cards = { for k, c in metronome_rate_card.standard : "cu-standard-v1" => c.id }
    alerts     = { for k, a in metronome_alert.zero_balance : "cu-zero-balance" => a.id }
    commit_rates_cents = {
      for k, r in metronome_rate.usage : k => "${r.commit_rate.price} per ${local.metronome_usage_products[k].commit_rate_per} from ${r.starting_at}"
    }
  }
}
