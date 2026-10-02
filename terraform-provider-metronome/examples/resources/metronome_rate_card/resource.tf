resource "metronome_rate_card" "standard" {
  name        = "Standard"
  description = "List prices"

  # Contracts name the alias, so a later rate card can take it over for new
  # customers while existing ones keep their prices.
  aliases = [{ name = "standard" }]
}
