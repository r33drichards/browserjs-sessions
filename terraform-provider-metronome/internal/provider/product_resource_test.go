package provider

import (
	"encoding/json"
	"reflect"
	"testing"
)

const productRes = "metronome_product"

// newMetric creates a billable metric and returns its ID.
func newMetric(h *harness) string {
	h.t.Helper()
	return str(h.mustApply(metricRes, h.null(metricRes), awakeMetric()), "id")
}

func awakeProduct(metricID string) cfg {
	return cfg{
		"name":               "Awake session time",
		"type":               "USAGE",
		"billable_metric_id": metricID,
		"tags":               []any{"usage"},
		"quantity_conversion": cfg{
			"name": "seconds to hours", "conversion_factor": 3600, "operation": "DIVIDE",
		},
	}
}

func lastJSON(t *testing.T, h *harness, path string) map[string]any {
	t.Helper()
	var sent map[string]any
	if err := json.Unmarshal([]byte(h.fake.Last("POST", path)), &sent); err != nil {
		t.Fatalf("the body sent to %s: %v", path, err)
	}
	return sent
}

func TestProductCreate(t *testing.T) {
	h := newHarness(t)
	metric := newMetric(h)
	state := h.mustApply(productRes, h.null(productRes), awakeProduct(metric))

	want := map[string]any{
		"name": "Awake session time", "type": "USAGE", "billable_metric_id": metric, "tags": []any{"usage"},
		"quantity_conversion": map[string]any{"name": "seconds to hours", "conversion_factor": 3600.0, "operation": "DIVIDE"},
	}
	if sent := lastJSON(t, h, "/v1/contract-pricing/products/create"); !reflect.DeepEqual(sent, want) {
		t.Errorf("sent %v\nwant %v", sent, want)
	}
	state = h.mustRead(productRes, state)
	h.unchanged(productRes, state, awakeProduct(metric))

	// A product a commit is sold as has nothing but a name.
	fixed := cfg{"name": "Computer Use credit", "type": "FIXED"}
	state = h.mustApply(productRes, h.null(productRes), fixed)
	state = h.mustRead(productRes, state)
	h.unchanged(productRes, state, fixed)
}

// An update is an entry in the product's history, from the start of the
// current hour, carrying only what changed.
func TestProductUpdatesInPlace(t *testing.T) {
	h := newHarness(t)
	metric := newMetric(h)
	state := h.mustApply(productRes, h.null(productRes), awakeProduct(metric))
	id := str(state, "id")
	const update = "/v1/contract-pricing/products/update"

	state = h.inPlace(productRes, state, with(awakeProduct(metric), "name", "Awake time"))
	want := map[string]any{"product_id": id, "starting_at": "2026-10-02T15:00:00Z", "name": "Awake time"}
	if sent := lastJSON(t, h, update); !reflect.DeepEqual(sent, want) {
		t.Errorf("sent %v\nwant %v", sent, want)
	}

	// Another metric: the product stays, and meters with it from now on.
	second := str(h.mustApply(metricRes, h.null(metricRes), with(awakeMetric(), "name", "Awake seconds v2")), "id")
	next := with(with(awakeProduct(second), "name", "Awake time"), "tags", nil)
	state = h.inPlace(productRes, state, next)
	want = map[string]any{"product_id": id, "starting_at": "2026-10-02T15:00:00Z", "name": "Awake time", "billable_metric_id": second, "tags": []any{}}
	if sent := lastJSON(t, h, update); !reflect.DeepEqual(sent, want) {
		t.Errorf("sent %v\nwant %v", sent, want)
	}

	// Taking the conversion away sends null, not nothing.
	next = with(next, "quantity_conversion", nil)
	next["quantity_rounding"] = cfg{"rounding_method": "ROUND_UP", "decimal_places": 2}
	state = h.inPlace(productRes, state, next)
	sent := lastJSON(t, h, update)
	if v, ok := sent["quantity_conversion"]; !ok || v != nil {
		t.Errorf("quantity_conversion was sent as %v (present: %v), want null", v, ok)
	}
	p, _ := h.fake.Product(id)
	if p.Current.QuantityConversion != nil || p.Current.QuantityRounding == nil || p.Current.BillableMetricID != second || len(p.Current.Tags) != 0 {
		t.Errorf("the fake holds %+v", p.Current)
	}
	if str(state, "id") != id {
		t.Error("the product was replaced")
	}
	state = h.mustRead(productRes, state)
	h.unchanged(productRes, state, next)
}

func TestProductTypeReplaces(t *testing.T) {
	h := newHarness(t)
	state := h.mustApply(productRes, h.null(productRes), cfg{"name": "Credit", "type": "FIXED"})
	old := str(state, "id")
	state = h.mustReplace(productRes, state, cfg{"name": "Credit", "type": "SUBSCRIPTION"}, "type")
	if p, _ := h.fake.Product(old); p.ArchivedAt == "" {
		t.Error("the old product was not archived")
	}
	if p, _ := h.fake.Product(str(state, "id")); p.Type != "SUBSCRIPTION" || p.ArchivedAt != "" {
		t.Errorf("the new product is %+v", p)
	}
}

func TestProductDestroyArchivesAndArchivedLeavesTheState(t *testing.T) {
	h := newHarness(t)
	state := h.mustApply(productRes, h.null(productRes), cfg{"name": "Credit", "type": "FIXED"})
	h.fake.ArchiveOutOfBand(str(state, "id"))
	if got := h.mustRead(productRes, state); !got.IsNull() {
		t.Errorf("the archived product is still in the state: %v", attrs(got))
	}

	state = h.mustApply(productRes, h.null(productRes), cfg{"name": "Credit", "type": "FIXED"})
	h.destroy(productRes, state)
	if p, ok := h.fake.Product(str(state, "id")); !ok || p.ArchivedAt == "" {
		t.Errorf("after destroy the fake holds %+v, %v: want it archived", p, ok)
	}
}

func TestProductImport(t *testing.T) {
	h := newHarness(t)
	metric := newMetric(h)
	created := h.mustApply(productRes, h.null(productRes), awakeProduct(metric))
	state, diags := h.importState(productRes, str(created, "id"))
	noErrors(t, "import", diags)
	if !reflect.DeepEqual(attrs(state), attrs(created)) {
		t.Errorf("imported %v\ncreated  %v", attrs(state), attrs(created))
	}
	h.unchanged(productRes, state, awakeProduct(metric))
}

func TestProductValidation(t *testing.T) {
	h := newHarness(t)
	const metric = "13117714-3f05-48e5-a6e9-a66093f13b4d"
	wantError(t, h.validate(productRes, cfg{"name": "x", "type": "USAGE"}), "Missing billable_metric_id")
	wantError(t, h.validate(productRes, cfg{"name": "x", "type": "FIXED", "billable_metric_id": metric}), "Only for USAGE products")
	wantError(t, h.validate(productRes, cfg{"name": "x", "type": "FIXED", "pricing_group_key": []any{"region"}}), "Only for USAGE products")
	wantError(t, h.validate(productRes, cfg{"name": "x", "type": "COMPOSITE"}), "type")
	wantError(t, h.validate(productRes, cfg{"name": "x", "type": "USAGE", "billable_metric_id": "awake"}), "UUID")
	bad := awakeProduct(metric)
	bad["quantity_conversion"] = cfg{"conversion_factor": 3600, "operation": "divide"}
	wantError(t, h.validate(productRes, bad), "operation")
	// The metric's ID is not known until it is created: that validates.
	noErrors(t, "unknown metric", h.validate(productRes, cfg{"name": "x", "type": "USAGE", "billable_metric_id": unknown}))
}
