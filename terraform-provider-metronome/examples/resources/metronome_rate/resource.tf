# 20 cents an hour.
resource "metronome_rate" "awake" {
  rate_card_id = metronome_rate_card.standard.id
  product_id   = metronome_product.awake.id
  starting_at  = "2026-10-01T00:00:00Z"
  entitled     = true
  rate_type    = "FLAT"
  price        = 20
}

# Free at list price and 28 cents against credit: usage draws credit down
# and costs nothing once it is gone.
resource "metronome_rate" "disk" {
  rate_card_id = metronome_rate_card.standard.id
  product_id   = metronome_product.disk.id
  starting_at  = "2026-10-01T00:00:00Z"
  entitled     = true
  rate_type    = "FLAT"
  price        = 0
  commit_rate = {
    rate_type = "FLAT"
    price     = 28
  }
}
