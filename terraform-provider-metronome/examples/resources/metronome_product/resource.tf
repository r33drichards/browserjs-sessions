# Usage, priced by the hour from a metric that counts seconds.
resource "metronome_product" "awake" {
  name               = "Awake time"
  type               = "USAGE"
  billable_metric_id = metronome_billable_metric.awake_seconds.id
  quantity_conversion = {
    conversion_factor = 3600
    operation         = "DIVIDE"
  }
}

# What a commit or a credit is sold as.
resource "metronome_product" "credit" {
  name = "Credit"
  type = "FIXED"
}
