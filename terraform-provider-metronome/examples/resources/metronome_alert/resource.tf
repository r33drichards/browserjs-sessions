# Every customer whose credit and commits are used up.
resource "metronome_alert" "zero_balance" {
  name               = "zero-balance"
  alert_type         = "low_remaining_contract_credit_and_commit_balance_reached"
  threshold          = 0
  uniqueness_key     = "zero-balance"
  evaluate_on_create = false
}
