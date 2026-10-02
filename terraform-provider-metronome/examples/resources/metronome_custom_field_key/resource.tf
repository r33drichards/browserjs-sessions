# Lets a credit carry the key of what caused it.
resource "metronome_custom_field_key" "grant_key" {
  entity             = "contract_credit"
  key                = "grant_key"
  enforce_uniqueness = false
}
