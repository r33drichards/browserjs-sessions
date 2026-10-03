# Everything below is derived from the catalogue: no price, product or rate
# is written twice.

locals {
  catalogue = yamldecode(file(var.catalogue_file))
  currency  = lower(local.catalogue.currency)

  # Entries by lookup key: the identity of a Stripe price. A price is never
  # edited; a new amount is a new entry with a new lookup key, and the old
  # entry stays, with enabled: false.
  plan_prices = { for p in local.catalogue.plans : p.lookupKey => p }
  pack_prices = { for p in local.catalogue.packs : p.lookupKey => p }
  prices      = merge(local.plan_prices, local.pack_prices)

  # Products by the catalogue's productId. Several entries may name one
  # product (the three packs do; so do a plan's old and new price).
  plan_products = {
    for id in distinct([for p in local.catalogue.plans : p.productId]) : id => {
      name          = "Computer Use ${[for p in local.catalogue.plans : p.name if p.productId == id][0]}"
      catalogue_key = [for p in local.catalogue.plans : p.key if p.productId == id][0]
      active        = anytrue([for p in local.catalogue.plans : p.enabled if p.productId == id])
    }
  }
  pack_products = {
    for id in distinct([for p in local.catalogue.packs : p.productId]) : id => {
      name          = "Computer Use credit"
      catalogue_key = "credit"
      active        = anytrue([for p in local.catalogue.packs : p.enabled if p.productId == id])
    }
  }
  products = merge(local.plan_products, local.pack_products)

  # Metronome prices are US cents and may be fractional; the catalogue's
  # rates are micro-dollars. 10000 micro-dollars are a cent.
  awake_cents_per_hour = local.catalogue.rates.awakeMicrosPerHour / 10000
  # The same for each size of session other than small.
  awake_cents_per_hour_by_size = {
    for size, s in try(local.catalogue.sizes, {}) : size => s.awakeMicrosPerHour / 10000
  }
  # A GB-month is 730 hours, rounded to the cent (metronome.md).
  disk_cents_per_gb_month = floor(local.catalogue.rates.diskMicrosPerGBHour * 730 / 10000 + 0.5)
}
