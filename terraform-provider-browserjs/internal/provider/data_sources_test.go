package provider

import (
	"reflect"
	"testing"
)

func TestSessionDataSource(t *testing.T) {
	h := newHarness(t)
	a := h.fake.AddSession("research")
	h.fake.AddSession("twin")
	h.fake.AddSession("twin")

	byID, diags := h.data("browserjs_session", cfg{"id": a})
	noErrors(t, "by id", diags)
	byName, diags := h.data("browserjs_session", cfg{"name": "research"})
	noErrors(t, "by name", diags)
	want := map[string]any{
		"id": a, "name": "research", "state": "running", "owner": "dev@example.com",
		"mcp_url": "https://sessions.example.test/" + a + "/mcp",
	}
	if !reflect.DeepEqual(byID, want) || !reflect.DeepEqual(byName, want) {
		t.Errorf("by id %v\nby name %v\nwant %v", byID, byName, want)
	}

	_, diags = h.data("browserjs_session", cfg{"name": "twin"})
	wantError(t, diags, "name", "More than one session", "Use id instead")
	_, diags = h.data("browserjs_session", cfg{"name": "nobody"})
	wantError(t, diags, "name", "No such session")
	_, diags = h.data("browserjs_session", cfg{"id": "s-zzzzz"})
	wantError(t, diags, "id", "No such session")
	_, diags = h.data("browserjs_session", cfg{})
	wantError(t, diags, "id", "name")
	_, diags = h.data("browserjs_session", cfg{"id": a, "name": "research"})
	wantError(t, diags, "id", "name")
}

func TestSessionsDataSource(t *testing.T) {
	h := newHarness(t)
	got, diags := h.data("browserjs_sessions", cfg{})
	noErrors(t, "empty", diags)
	if l := got["sessions"].([]any); len(l) != 0 {
		t.Errorf("sessions %v", l)
	}
	a, b := h.fake.AddSession("one"), h.fake.AddSession("two")
	got, diags = h.data("browserjs_sessions", cfg{})
	noErrors(t, "two", diags)
	l := got["sessions"].([]any)
	if len(l) != 2 || l[0].(map[string]any)["id"] != a || l[1].(map[string]any)["id"] != b || l[1].(map[string]any)["name"] != "two" {
		t.Errorf("sessions %v", l)
	}
}
