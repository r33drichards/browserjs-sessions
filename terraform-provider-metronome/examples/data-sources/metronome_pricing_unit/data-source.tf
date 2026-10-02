data "metronome_pricing_unit" "usd" {
  name = "USD (cents)"
}

resource "metronome_rate_card" "standard" {
  name                = "Standard"
  fiat_credit_type_id = data.metronome_pricing_unit.usd.id
}
