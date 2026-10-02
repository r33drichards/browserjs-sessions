package provider

import (
	"reflect"
	"testing"

	"github.com/r33drichards/browserjs-sessions/terraform-provider-metronome/internal/client"
	"github.com/r33drichards/browserjs-sessions/terraform-provider-metronome/internal/fakeapi"
)

const rateRes = "metronome_rate"

// pricing is a rate card and a usage product to put rates on.
func pricing(h *harness) (cardID, productID string) {
	h.t.Helper()
	cardID = str(h.mustApply(rateCardRes, h.null(rateCardRes), standardCard()), "id")
	productID = str(h.mustApply(productRes, h.null(productRes), awakeProduct(newMetric(h))), "id")
	return cardID, productID
}

func awakeRate(cardID, productID string) cfg {
	return cfg{
		"rate_card_id": cardID,
		"product_id":   productID,
		"starting_at":  "2026-10-01T00:00:00Z",
		"entitled":     true,
		"rate_type":    "FLAT",
		"price":        20,
	}
}

func TestRateAdd(t *testing.T) {
	h := newHarness(t)
	h.fake.Configure(func(s *fakeapi.Server) { s.Milliseconds = true })
	card, product := pricing(h)
	state := h.mustApply(rateRes, h.null(rateRes), awakeRate(card, product))

	want := map[string]any{
		"rate_card_id": card, "product_id": product, "starting_at": "2026-10-01T00:00:00Z",
		"entitled": true, "rate_type": "FLAT", "price": 20.0,
	}
	if sent := lastJSON(t, h, "/v1/contract-pricing/rate-cards/addRate"); !reflect.DeepEqual(sent, want) {
		t.Errorf("sent %v\nwant %v", sent, want)
	}
	if got, want := str(state, "id"), card+"/"+product+"/2026-10-01T00:00:00Z"; got != want {
		t.Errorf("id %q, want %q", got, want)
	}
	if got := str(state, "credit_type_id"); got != client.USDCreditTypeID {
		t.Errorf("credit_type_id %q, want USD", got)
	}
	state = h.mustRead(rateRes, state)
	h.unchanged(rateRes, state, awakeRate(card, product))
}

// A fraction of a cent survives the trip.
func TestRateFractionalPrice(t *testing.T) {
	h := newHarness(t)
	card, product := pricing(h)
	c := with(awakeRate(card, product), "price", 0.19178082)
	state := h.mustApply(rateRes, h.null(rateRes), c)
	state = h.mustRead(rateRes, state)
	h.unchanged(rateRes, state, c)
	if got := h.fake.Rates(card)[0].Rate.Price; got == nil || *got != 0.19178082 {
		t.Errorf("the fake holds price %v", got)
	}
}

// Rates are only ever added. A new price is a new rate, and the old one
// stays on the schedule: nothing in the API takes it away.
func TestRateChangeAddsAndDestroyLeavesItThere(t *testing.T) {
	h := newHarness(t)
	card, product := pricing(h)
	state := h.mustApply(rateRes, h.null(rateRes), awakeRate(card, product))
	before := len(h.fake.Requests())

	next := with(with(awakeRate(card, product), "price", 25), "starting_at", "2026-11-01T00:00:00Z")
	p := h.plan(rateRes, state, next)
	noErrors(t, "plan", p.diags)
	if len(p.replace) == 0 {
		t.Fatal("a new price does not replace the rate")
	}

	diags := h.destroy(rateRes, state)
	wantWarning(t, diags, "The rate stays in Metronome", "no call that removes a rate")
	if n := len(h.fake.Requests()) - before; n != 0 {
		t.Errorf("destroying a rate made %d requests, want none", n)
	}
	state = h.mustApply(rateRes, h.null(rateRes), next)

	rates := h.fake.Rates(card)
	if len(rates) != 2 || *rates[0].Rate.Price != 20 || *rates[1].Rate.Price != 25 {
		t.Fatalf("the schedule is %+v, want the 20 rate followed by the 25 rate", rates)
	}
	state = h.mustRead(rateRes, state)
	h.unchanged(rateRes, state, next)
}

func TestRateEveryAttributeReplaces(t *testing.T) {
	h := newHarness(t)
	card, product := pricing(h)
	state := h.mustApply(rateRes, h.null(rateRes), awakeRate(card, product))
	for attr, v := range map[string]any{
		"price": 30, "entitled": false, "starting_at": "2026-12-01T00:00:00Z", "ending_before": "2027-01-01T00:00:00Z",
		"product_id": "13117714-3f05-48e5-a6e9-a66093f13b4d", "rate_card_id": "13117714-3f05-48e5-a6e9-a66093f13b4d",
		"pricing_group_values": cfg{"region": "us"},
	} {
		p := h.plan(rateRes, state, with(awakeRate(card, product), attr, v))
		noErrors(t, "plan "+attr, p.diags)
		if len(p.replace) != 1 {
			t.Errorf("changing %s replaces because of %v, want that attribute", attr, p.replace)
		}
	}
}

// What the schedule says at the rate's own start is what the state shows.
func TestRateReadSeesTheSchedule(t *testing.T) {
	h := newHarness(t)
	card, product := pricing(h)
	state := h.mustApply(rateRes, h.null(rateRes), awakeRate(card, product))

	// A later rate does not disturb this one.
	later := h.mustApply(rateRes, h.null(rateRes), with(with(awakeRate(card, product), "price", 25), "starting_at", "2026-11-01T00:00:00Z"))
	h.unchanged(rateRes, h.mustRead(rateRes, state), awakeRate(card, product))
	h.unchanged(rateRes, h.mustRead(rateRes, later), with(with(awakeRate(card, product), "price", 25), "starting_at", "2026-11-01T00:00:00Z"))

	// A rate added behind Terraform's back for the same start shows as a
	// difference.
	h.mustApply(rateRes, h.null(rateRes), with(awakeRate(card, product), "price", 99))
	state = h.mustRead(rateRes, state)
	if got := attrs(state)["price"]; got != 99.0 {
		t.Errorf("price read as %v, want 99", got)
	}
	if p := h.plan(rateRes, state, awakeRate(card, product)); len(p.replace) == 0 {
		t.Error("the difference is not planned as a replacement")
	}

	// A rate the schedule does not have is not there to import, and one
	// whose rate card is gone leaves the state.
	other := str(h.mustApply(rateCardRes, h.null(rateCardRes), cfg{"name": "empty"}), "id")
	imported, diags := h.importState(rateRes, other+"/"+product+"/2026-10-01T00:00:00Z")
	noErrors(t, "import", diags)
	if !imported.IsNull() {
		t.Errorf("a rate that is not on the rate card was imported: %v", attrs(imported))
	}
	imported, diags = h.importState(rateRes, "00000000-0000-4000-8000-000000000000/"+product+"/2026-10-01T00:00:00Z")
	noErrors(t, "import", diags)
	if !imported.IsNull() {
		t.Errorf("a rate of a rate card that does not exist was imported: %v", attrs(imported))
	}
}

// The pricing of this repository: free at list price, 20 cents an hour
// against credit.
func TestRateWithACommitRate(t *testing.T) {
	h := newHarness(t)
	card, product := pricing(h)
	c := with(with(awakeRate(card, product), "price", 0), "commit_rate", cfg{"rate_type": "FLAT", "price": 20})
	state := h.mustApply(rateRes, h.null(rateRes), c)
	sent := lastJSON(t, h, "/v1/contract-pricing/rate-cards/addRate")
	if sent["price"] != 0.0 || !reflect.DeepEqual(sent["commit_rate"], map[string]any{"rate_type": "FLAT", "price": 20.0}) {
		t.Errorf("sent %v", sent)
	}
	state = h.mustRead(rateRes, state)
	h.unchanged(rateRes, state, c)

	imported, diags := h.importState(rateRes, str(state, "id"))
	noErrors(t, "import", diags)
	h.unchanged(rateRes, imported, c)

	next := with(c, "commit_rate", cfg{"rate_type": "FLAT", "price": 25})
	if p := h.plan(rateRes, state, next); len(p.replace) != 1 {
		t.Errorf("a new commit rate replaces because of %v, want commit_rate", p.replace)
	}
	wantError(t, h.validate(rateRes, with(c, "commit_rate", cfg{"rate_type": "TIERED", "price": 1})), "rate_type")
}

func TestRateTieredAndSubscription(t *testing.T) {
	h := newHarness(t)
	card, product := pricing(h)
	tiered := cfg{
		"rate_card_id": card, "product_id": product, "starting_at": "2026-10-01T00:00:00Z", "entitled": true,
		"rate_type": "TIERED", "tiers": []any{cfg{"size": 100, "price": 20}, cfg{"price": 10}},
	}
	state := h.mustApply(rateRes, h.null(rateRes), tiered)
	state = h.mustRead(rateRes, state)
	h.unchanged(rateRes, state, tiered)

	seat := str(h.mustApply(productRes, h.null(productRes), cfg{"name": "Seat", "type": "SUBSCRIPTION"}), "id")
	sub := cfg{
		"rate_card_id": card, "product_id": seat, "starting_at": "2026-10-01T00:00:00Z", "entitled": true,
		"rate_type": "SUBSCRIPTION", "price": 500, "quantity": 1, "is_prorated": true, "billing_frequency": "MONTHLY",
	}
	state = h.mustApply(rateRes, h.null(rateRes), sub)
	state = h.mustRead(rateRes, state)
	h.unchanged(rateRes, state, sub)
}

func TestRateImport(t *testing.T) {
	h := newHarness(t)
	card, product := pricing(h)
	created := h.mustApply(rateRes, h.null(rateRes), awakeRate(card, product))
	state, diags := h.importState(rateRes, str(created, "id"))
	noErrors(t, "import", diags)
	if !reflect.DeepEqual(attrs(state), attrs(created)) {
		t.Errorf("imported %v\ncreated  %v", attrs(state), attrs(created))
	}
	h.unchanged(rateRes, state, awakeRate(card, product))

	for _, id := range []string{card, card + "/" + product, card + "/" + product + "/yesterday", "a/b/2026-10-01T00:00:00Z"} {
		_, diags := h.importState(rateRes, id)
		wantError(t, diags, "Not a rate ID")
	}
}

func TestRateValidation(t *testing.T) {
	h := newHarness(t)
	const id = "13117714-3f05-48e5-a6e9-a66093f13b4d"
	ok := awakeRate(id, id)
	noErrors(t, "flat", h.validate(rateRes, ok))
	wantError(t, h.validate(rateRes, with(ok, "price", nil)), "Missing price")
	wantError(t, h.validate(rateRes, with(ok, "price", -1)), "price")
	wantError(t, h.validate(rateRes, with(ok, "starting_at", "2026-10-01T00:30:00Z")), "hour boundary")
	wantError(t, h.validate(rateRes, with(ok, "rate_type", "TIERED")), "Missing tiers")
	wantError(t, h.validate(rateRes, with(ok, "tiers", []any{cfg{"price": 1}})), "Only for TIERED")
	wantError(t, h.validate(rateRes, with(ok, "rate_type", "SUBSCRIPTION")), "Missing billing_frequency")
	wantError(t, h.validate(rateRes, with(ok, "rate_type", "CUSTOM")), "rate_type")
}

// The API's refusals reach the user: here, a rate for an archived product.
func TestRateRefusedByTheAPI(t *testing.T) {
	h := newHarness(t)
	card, product := pricing(h)
	h.fake.ArchiveOutOfBand(product)
	_, diags := h.apply(h.plan(rateRes, h.null(rateRes), awakeRate(card, product)))
	wantError(t, diags, "add the rate", "the product is archived")
}
