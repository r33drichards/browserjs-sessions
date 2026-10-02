package provider

import (
	"reflect"
	"testing"
)

const alertRes = "metronome_alert"

func lowBalance() cfg {
	return cfg{
		"name":           "Credit low",
		"alert_type":     "low_remaining_contract_credit_and_commit_balance_reached",
		"threshold":      200,
		"uniqueness_key": "computeruse-credit-low",
	}
}

func TestAlertCreate(t *testing.T) {
	h := newHarness(t)
	state := h.mustApply(alertRes, h.null(alertRes), lowBalance())
	want := map[string]any{
		"name": "Credit low", "alert_type": "low_remaining_contract_credit_and_commit_balance_reached",
		"threshold": 200.0, "uniqueness_key": "computeruse-credit-low",
	}
	if sent := lastJSON(t, h, "/v1/alerts/create"); !reflect.DeepEqual(sent, want) {
		t.Errorf("sent %v\nwant %v", sent, want)
	}
	if got := attrs(state)["release_uniqueness_key_on_destroy"]; got != true {
		t.Errorf("release_uniqueness_key_on_destroy defaults to %v, want true", got)
	}

	// A notification for every customer cannot be read: a refresh asks the
	// API nothing and keeps the state.
	before := len(h.fake.Requests())
	state = h.mustRead(alertRes, state)
	if n := len(h.fake.Requests()) - before; n != 0 {
		t.Errorf("refreshing made %d requests, want none", n)
	}
	h.unchanged(alertRes, state, lowBalance())
}

// Nothing about a notification can change. Replacing one archives it first,
// releasing its key, so the replacement can have the same key.
func TestAlertChangeReplaces(t *testing.T) {
	h := newHarness(t)
	state := h.mustApply(alertRes, h.null(alertRes), lowBalance())
	old := str(state, "id")
	for attr, v := range map[string]any{"name": "Low", "threshold": 100, "alert_type": "spend_threshold_reached", "uniqueness_key": "k2", "evaluate_on_create": false} {
		if p := h.plan(alertRes, state, with(lowBalance(), attr, v)); len(p.replace) != 1 {
			t.Errorf("changing %s replaces because of %v, want that attribute", attr, p.replace)
		}
	}
	state = h.mustReplace(alertRes, state, with(lowBalance(), "threshold", 100), "threshold")
	if got := h.fake.AlertStatus(old); got != "archived" {
		t.Errorf("the old notification is %q, want archived", got)
	}
	if got := h.fake.AlertStatus(str(state, "id")); got != "enabled" {
		t.Errorf("the new notification is %q, want enabled", got)
	}
	want := map[string]any{"id": old, "release_uniqueness_key": true}
	if sent := lastJSON(t, h, "/v1/alerts/archive"); !reflect.DeepEqual(sent, want) {
		t.Errorf("archive sent %v, want %v", sent, want)
	}
}

func TestAlertKeptKeyIsNotReleased(t *testing.T) {
	h := newHarness(t)
	c := with(lowBalance(), "release_uniqueness_key_on_destroy", false)
	state := h.mustApply(alertRes, h.null(alertRes), c)
	h.destroy(alertRes, state)
	_, diags := h.apply(h.plan(alertRes, h.null(alertRes), c))
	wantError(t, diags, "The uniqueness key is taken", "409", "computeruse-credit-low")

	// The setting itself changes in place.
	state = h.mustApply(alertRes, h.null(alertRes), with(c, "uniqueness_key", "k2"))
	h.inPlace(alertRes, state, with(lowBalance(), "uniqueness_key", "k2"))
}

// With a customer, the notification can be read, and one archived in
// Metronome's app leaves the state.
func TestAlertForOneCustomerIsRead(t *testing.T) {
	h := newHarness(t)
	const customer = "9b85c1c1-5238-4f2a-a409-61412905e1e1"
	c := with(lowBalance(), "customer_id", customer)
	state := h.mustApply(alertRes, h.null(alertRes), c)
	state = h.mustRead(alertRes, state)
	h.unchanged(alertRes, state, c)

	h.destroy(alertRes, state)
	if got := h.mustRead(alertRes, state); !got.IsNull() {
		t.Errorf("the archived notification is still in the state: %v", attrs(got))
	}
}

func TestAlertValidation(t *testing.T) {
	h := newHarness(t)
	wantError(t, h.validate(alertRes, with(lowBalance(), "alert_type", "low_credit")), "alert_type")
	wantError(t, h.validate(alertRes, with(lowBalance(), "customer_id", "acme")), "UUID")
	_, diags := h.apply(h.plan(alertRes, h.null(alertRes), with(lowBalance(), "alert_type", "usage_threshold_reached")))
	wantError(t, diags, "billable_metric_id is required")
	if _, diags := h.importState(alertRes, "13117714-3f05-48e5-a6e9-a66093f13b4d"); len(errorsOf(diags)) == 0 {
		t.Error("import is supported; the resource says it is not")
	}
}
