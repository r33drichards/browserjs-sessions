package provider

import (
	"reflect"
	"testing"

	"github.com/r33drichards/computer-use/terraform-provider-metronome/internal/client"
	"github.com/r33drichards/computer-use/terraform-provider-metronome/internal/fakeapi"
)

const rateCardRes = "metronome_rate_card"

func standardCard() cfg {
	return cfg{
		"name":        "Computer Use standard",
		"description": "List prices",
		"aliases":     []any{cfg{"name": "computeruse-standard", "starting_at": "2026-10-01T00:00:00Z"}},
	}
}

func TestRateCardCreate(t *testing.T) {
	h := newHarness(t)
	// The real API echoes timestamps with milliseconds; the state keeps
	// what the configuration wrote.
	h.fake.Configure(func(s *fakeapi.Server) { s.Milliseconds = true })
	p := h.plan(rateCardRes, h.null(rateCardRes), standardCard())
	noErrors(t, "plan", p.diags)
	if attrs(p.state)["fiat_credit_type_id"] != unknown {
		t.Errorf("fiat_credit_type_id is planned as %v, want unknown", attrs(p.state)["fiat_credit_type_id"])
	}
	state, diags := h.apply(p)
	noErrors(t, "apply", diags)
	consistent(t, p.state, state)
	if got := str(state, "fiat_credit_type_id"); got != client.USDCreditTypeID {
		t.Errorf("fiat_credit_type_id %q, want USD", got)
	}
	want := map[string]any{
		"name": "Computer Use standard", "description": "List prices",
		"aliases": []any{map[string]any{"name": "computeruse-standard", "starting_at": "2026-10-01T00:00:00Z"}},
	}
	if sent := lastJSON(t, h, "/v1/contract-pricing/rate-cards/create"); !reflect.DeepEqual(sent, want) {
		t.Errorf("sent %v\nwant %v", sent, want)
	}
	state = h.mustRead(rateCardRes, state)
	h.unchanged(rateCardRes, state, standardCard())

	// The least there is.
	bare := h.mustApply(rateCardRes, h.null(rateCardRes), cfg{"name": "bare"})
	bare = h.mustRead(rateCardRes, bare)
	h.unchanged(rateCardRes, bare, cfg{"name": "bare"})
}

func TestRateCardUpdatesInPlace(t *testing.T) {
	h := newHarness(t)
	state := h.mustApply(rateCardRes, h.null(rateCardRes), standardCard())
	id := str(state, "id")

	next := cfg{"name": "Computer Use 2026", "aliases": []any{
		cfg{"name": "computeruse-standard", "starting_at": "2026-10-01T00:00:00Z", "ending_before": "2027-01-01T00:00:00Z"},
	}}
	state = h.inPlace(rateCardRes, state, next)
	want := map[string]any{
		"rate_card_id": id, "name": "Computer Use 2026", "description": "",
		"aliases": []any{map[string]any{"name": "computeruse-standard", "starting_at": "2026-10-01T00:00:00Z", "ending_before": "2027-01-01T00:00:00Z"}},
	}
	if sent := lastJSON(t, h, "/v1/contract-pricing/rate-cards/update"); !reflect.DeepEqual(sent, want) {
		t.Errorf("sent %v\nwant %v", sent, want)
	}
	if str(state, "id") != id {
		t.Error("the rate card was replaced")
	}
	state = h.mustRead(rateCardRes, state)
	h.unchanged(rateCardRes, state, next)

	// Dropping the aliases sends an empty list.
	state = h.inPlace(rateCardRes, state, cfg{"name": "Computer Use 2026"})
	if sent := lastJSON(t, h, "/v1/contract-pricing/rate-cards/update"); !reflect.DeepEqual(sent["aliases"], []any{}) {
		t.Errorf("aliases sent as %v, want []", sent["aliases"])
	}
	h.unchanged(rateCardRes, h.mustRead(rateCardRes, state), cfg{"name": "Computer Use 2026"})
}

func TestRateCardCurrencyReplacesAndDestroyArchives(t *testing.T) {
	h := newHarness(t)
	state := h.mustApply(rateCardRes, h.null(rateCardRes), standardCard())
	id := str(state, "id")

	p := h.plan(rateCardRes, state, with(standardCard(), "fiat_credit_type_id", "fa2f1b3d-9d52-4951-a099-25991fd394d6"))
	if len(p.replace) != 1 {
		t.Errorf("changing the currency replaces %v, want fiat_credit_type_id", p.replace)
	}

	h.destroy(rateCardRes, state)
	if archived, ok := h.fake.RateCardArchived(id); !ok || !archived {
		t.Errorf("after destroy: archived %v, exists %v", archived, ok)
	}
}

func TestRateCardImportAndValidation(t *testing.T) {
	h := newHarness(t)
	created := h.mustApply(rateCardRes, h.null(rateCardRes), standardCard())
	state, diags := h.importState(rateCardRes, str(created, "id"))
	noErrors(t, "import", diags)
	if !reflect.DeepEqual(attrs(state), attrs(created)) {
		t.Errorf("imported %v\ncreated  %v", attrs(state), attrs(created))
	}
	_, diags = h.importState(rateCardRes, "00000000-0000-4000-8000-000000000000")
	if len(errorsOf(diags)) != 0 {
		t.Errorf("importing a rate card that does not exist: %v", errorsOf(diags))
	}

	bad := cfg{"name": "x", "aliases": []any{cfg{"name": "a", "starting_at": "2026-10-01"}}}
	wantError(t, h.validate(rateCardRes, bad), "RFC 3339")
}
