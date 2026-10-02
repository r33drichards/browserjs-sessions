# The seconds each session was awake, added up.
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
