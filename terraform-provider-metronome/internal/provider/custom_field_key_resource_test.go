package provider

import (
	"reflect"
	"testing"

	"github.com/r33drichards/computer-use/terraform-provider-metronome/internal/client"
)

const fieldKeyRes = "metronome_custom_field_key"

func TestCustomFieldKey(t *testing.T) {
	h := newHarness(t)
	c := cfg{"entity": "contract_credit", "key": "grant_key", "enforce_uniqueness": false}
	state := h.mustApply(fieldKeyRes, h.null(fieldKeyRes), c)
	if got := str(state, "id"); got != "contract_credit/grant_key" {
		t.Errorf("id %q", got)
	}
	want := []client.CustomFieldKey{{Entity: "contract_credit", Key: "grant_key"}}
	if got := h.fake.FieldKeys(); !reflect.DeepEqual(got, want) {
		t.Errorf("the fake holds %+v", got)
	}
	state = h.mustRead(fieldKeyRes, state)
	h.unchanged(fieldKeyRes, state, c)

	// The same key on another kind of object is another key.
	other := h.mustApply(fieldKeyRes, h.null(fieldKeyRes), with(c, "entity", "commit"))
	if n := len(h.fake.FieldKeys()); n != 2 {
		t.Errorf("%d keys, want 2", n)
	}

	imported, diags := h.importState(fieldKeyRes, "contract_credit/grant_key")
	noErrors(t, "import", diags)
	if !reflect.DeepEqual(attrs(imported), attrs(state)) {
		t.Errorf("imported %v\ncreated  %v", attrs(imported), attrs(state))
	}
	_, diags = h.importState(fieldKeyRes, "grant_key")
	wantError(t, diags, "Not a custom field key ID")

	// Everything replaces; a destroy deletes, for once.
	state = h.mustReplace(fieldKeyRes, state, with(c, "enforce_uniqueness", true), "enforce_uniqueness")
	h.destroy(fieldKeyRes, state)
	if got := h.fake.FieldKeys(); len(got) != 1 || got[0].Entity != "commit" {
		t.Errorf("after destroy the fake holds %+v, want only the commit key", got)
	}
	if got := h.mustRead(fieldKeyRes, state); !got.IsNull() {
		t.Error("a key that was removed is still in the state")
	}
	h.destroy(fieldKeyRes, other)

	wantError(t, h.validate(fieldKeyRes, with(c, "entity", "credit")), "entity")
}
