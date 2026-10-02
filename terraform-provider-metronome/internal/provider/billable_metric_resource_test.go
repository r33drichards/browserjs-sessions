package provider

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/r33drichards/computer-use/terraform-provider-metronome/internal/fakeapi"
)

const metricRes = "metronome_billable_metric"

func awakeMetric() cfg {
	return cfg{
		"name":              "Awake seconds",
		"aggregation_type":  "SUM",
		"aggregation_key":   "seconds",
		"event_type_filter": cfg{"in_values": []any{"session.awake"}},
		"property_filters":  []any{cfg{"name": "seconds", "exists": true}},
		"group_keys":        []any{[]any{"session_id"}},
	}
}

// with is a copy of c with one attribute changed.
func with(c cfg, name string, v any) cfg {
	out := cfg{}
	for k, e := range c {
		out[k] = e
	}
	out[name] = v
	return out
}

func TestMetricCreate(t *testing.T) {
	h := newHarness(t)
	p := h.plan(metricRes, h.null(metricRes), awakeMetric())
	noErrors(t, "plan", p.diags)
	if attrs(p.state)["id"] != unknown {
		t.Errorf("id is planned as %v, want unknown", attrs(p.state)["id"])
	}
	state, diags := h.apply(p)
	noErrors(t, "apply", diags)
	consistent(t, p.state, state)

	var sent map[string]any
	if err := json.Unmarshal([]byte(h.fake.Last("POST", "/v1/billable-metrics/create")), &sent); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"name": "Awake seconds", "aggregation_type": "SUM", "aggregation_key": "seconds",
		"event_type_filter": map[string]any{"in_values": []any{"session.awake"}},
		"property_filters":  []any{map[string]any{"name": "seconds", "exists": true}},
		"group_keys":        []any{[]any{"session_id"}},
	}
	if !reflect.DeepEqual(sent, want) {
		t.Errorf("sent %v\nwant %v", sent, want)
	}
	if m, ok := h.fake.Metric(str(state, "id")); !ok || m.Name != "Awake seconds" {
		t.Errorf("the fake holds %v, %v", m, ok)
	}

	// A refresh and a second plan change nothing.
	state = h.mustRead(metricRes, state)
	h.unchanged(metricRes, state, awakeMetric())
}

func TestMetricRenameIsInPlace(t *testing.T) {
	h := newHarness(t)
	state := h.mustApply(metricRes, h.null(metricRes), awakeMetric())
	id := str(state, "id")

	state = h.inPlace(metricRes, state, with(awakeMetric(), "name", "Awake time (seconds)"))
	if str(state, "id") != id {
		t.Errorf("id changed from %s to %s", id, str(state, "id"))
	}
	if m, _ := h.fake.Metric(id); m.Name != "Awake time (seconds)" || m.ArchivedAt != "" {
		t.Errorf("the fake holds %+v", m)
	}
	if n := h.fake.Count("PUT", "/v1/billable-metrics/"+id); n != 1 {
		t.Errorf("%d renames, want 1", n)
	}
}

// Metronome fixes a metric's definition at creation: every part of it
// replaces the metric, and the old one is archived, not deleted.
func TestMetricDefinitionReplaces(t *testing.T) {
	changes := map[string]any{
		"aggregation_type":  "MAX",
		"aggregation_key":   "secs",
		"event_type_filter": cfg{"in_values": []any{"session.awake.v2"}},
		"property_filters":  []any{cfg{"name": "seconds", "exists": true}, cfg{"name": "region", "in_values": []any{"us"}}},
		"group_keys":        []any{[]any{"account_id"}},
		"custom_fields":     cfg{"team": "billing"},
	}
	for attr, v := range changes {
		t.Run(attr, func(t *testing.T) {
			h := newHarness(t)
			state := h.mustApply(metricRes, h.null(metricRes), awakeMetric())
			old := str(state, "id")
			next := with(awakeMetric(), attr, v)
			if attr == "aggregation_key" {
				next["property_filters"] = []any{cfg{"name": "secs", "exists": true}}
			}
			state = h.mustReplace(metricRes, state, next, attr)
			if str(state, "id") == old {
				t.Error("the replacement has the old ID")
			}
			if m, _ := h.fake.Metric(old); m.ArchivedAt == "" {
				t.Error("the old metric was not archived")
			}
		})
	}
}

func TestMetricSQL(t *testing.T) {
	h := newHarness(t)
	c := cfg{"name": "Kept hours", "sql": "select count(*) as value from events"}
	state := h.mustApply(metricRes, h.null(metricRes), c)
	state = h.mustRead(metricRes, state)
	h.unchanged(metricRes, state, c)
	h.mustReplace(metricRes, state, with(c, "sql", "select 1 as value"), "sql")
}

func TestMetricDestroyArchives(t *testing.T) {
	h := newHarness(t)
	state := h.mustApply(metricRes, h.null(metricRes), awakeMetric())
	id := str(state, "id")
	h.destroy(metricRes, state)
	m, ok := h.fake.Metric(id)
	if !ok || m.ArchivedAt == "" {
		t.Errorf("after destroy the fake holds %+v, %v: want it archived, not gone", m, ok)
	}
	// Destroying what is already archived, or gone, is not an error.
	h.destroy(metricRes, state)
}

// A metric archived in Metronome's app cannot be restored: it leaves the
// state, so the next apply creates another.
func TestMetricArchivedOutOfBandLeavesTheState(t *testing.T) {
	h := newHarness(t)
	state := h.mustApply(metricRes, h.null(metricRes), awakeMetric())
	h.fake.ArchiveOutOfBand(str(state, "id"))
	if state = h.mustRead(metricRes, state); !state.IsNull() {
		t.Errorf("the archived metric is still in the state: %v", attrs(state))
	}
}

func TestMetricImport(t *testing.T) {
	h := newHarness(t)
	h.fake.Configure(func(s *fakeapi.Server) { s.Milliseconds = true })
	created := h.mustApply(metricRes, h.null(metricRes), awakeMetric())

	state, diags := h.importState(metricRes, str(created, "id"))
	noErrors(t, "import", diags)
	if !reflect.DeepEqual(attrs(state), attrs(created)) {
		t.Errorf("imported %v\ncreated  %v", attrs(state), attrs(created))
	}
	h.unchanged(metricRes, state, awakeMetric())

	_, diags = h.importState(metricRes, "awake-seconds")
	wantError(t, diags, "Not a Metronome ID")

	h.fake.ArchiveOutOfBand(str(created, "id"))
	if state, _ := h.importState(metricRes, str(created, "id")); !state.IsNull() {
		t.Error("an archived metric was imported")
	}
}

func TestMetricValidation(t *testing.T) {
	h := newHarness(t)
	wantError(t, h.validate(metricRes, cfg{"name": "x"}), "no definition")
	wantError(t, h.validate(metricRes, with(awakeMetric(), "sql", "select 1")), "two definitions")
	wantError(t, h.validate(metricRes, cfg{"name": "x", "aggregation_type": "SUM"}), "aggregation_key")
	wantError(t, h.validate(metricRes, with(awakeMetric(), "aggregation_type", "sum")), "aggregation_type")
	noErrors(t, "count", h.validate(metricRes, cfg{"name": "x", "aggregation_type": "COUNT"}))
	if n := len(h.fake.Requests()); n != 0 {
		t.Errorf("validation made %d requests", n)
	}
}

func TestMetricAPIErrors(t *testing.T) {
	h := newHarness(t)
	// The API's own check: the key must be a property filter.
	p := h.plan(metricRes, h.null(metricRes), with(awakeMetric(), "aggregation_key", "minutes"))
	_, diags := h.apply(p)
	wantError(t, diags, "create the billable metric", "400", "aggregation_key must be one of the property filter names")

	// A rate limit is waited out.
	h.fake.Configure(func(s *fakeapi.Server) { s.RateLimited = 2 })
	state := h.mustApply(metricRes, h.null(metricRes), awakeMetric())
	if n := h.fake.Count("POST", "/v1/billable-metrics/create"); n != 4 {
		t.Errorf("%d creates, want 4 (one refused, two rate limited, one accepted)", n)
	}

	// One that lasts is reported.
	h.fake.Configure(func(s *fakeapi.Server) { s.RateLimited = 100 })
	_, diags = h.read(metricRes, state)
	wantError(t, diags, "429", "rate limit")
	h.fake.Configure(func(s *fakeapi.Server) { s.RateLimited = 0 })

	// A wrong token says what to do, and never shows the token.
	h.fake.Configure(func(s *fakeapi.Server) { s.Token = "another" })
	_, diags = h.read(metricRes, state)
	wantError(t, diags, "401", envToken)
	for _, e := range errorsOf(diags) {
		if strings.Contains(e, testToken) {
			t.Errorf("the token is in an error: %s", e)
		}
	}
}
