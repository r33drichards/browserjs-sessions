package fakeapi_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/r33drichards/browserjs-sessions/terraform-provider-metronome/internal/client"
	"github.com/r33drichards/browserjs-sessions/terraform-provider-metronome/internal/fakeapi"
)

const token = "fake-token"

func start(t *testing.T) (*fakeapi.Server, *client.Client) {
	t.Helper()
	fake := fakeapi.New(token)
	ts := httptest.NewServer(fake.Handler())
	t.Cleanup(ts.Close)
	c, err := client.New(ts.URL, token, "test")
	if err != nil {
		t.Fatal(err)
	}
	c.SetRetryWait(func(int) time.Duration { return time.Millisecond })
	return fake, c
}

func refused(t *testing.T, err error, status int, part string) {
	t.Helper()
	if client.StatusOf(err) != status || !strings.Contains(err.Error(), part) {
		t.Errorf("err %v, want %d containing %q", err, status, part)
	}
}

func TestTheTokenIsChecked(t *testing.T) {
	fake, _ := start(t)
	ts := httptest.NewServer(fake.Handler())
	defer ts.Close()
	c, _ := client.New(ts.URL, "wrong", "test")
	_, err := c.ListCreditTypes(context.Background())
	refused(t, err, 401, "Unauthorized")
	if reqs := fake.Requests(); len(reqs) != 1 || reqs[0].Authorized {
		t.Errorf("requests %+v", reqs)
	}
}

// The rules the provider is built around: definitions are fixed, archived
// things stay readable and cannot be used again, rates only accumulate.
func TestMetronomesRules(t *testing.T) {
	ctx := context.Background()
	fake, c := start(t)

	metric := client.BillableMetric{
		Name: "Awake seconds", AggregationType: "sum", AggregationKey: "seconds",
		PropertyFilters: []client.PropertyFilter{{Name: "seconds"}},
	}
	id, err := c.CreateBillableMetric(ctx, metric)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := c.GetBillableMetric(ctx, id)
	if got.AggregationType != "SUM" || got.ArchivedAt != "" {
		t.Errorf("metric %+v", got)
	}
	_, err = c.CreateBillableMetric(ctx, client.BillableMetric{Name: "x", AggregationType: "SUM", AggregationKey: "missing"})
	refused(t, err, 400, "aggregation_key")
	_, err = c.CreateBillableMetric(ctx, client.BillableMetric{Name: "x", AggregationType: "SUM", SQL: "select 1"})
	refused(t, err, 400, "mutually exclusive")

	product, err := c.CreateProduct(ctx, client.ProductInput{Name: "Awake", Type: "USAGE", BillableMetricID: id})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.CreateProduct(ctx, client.ProductInput{Name: "Awake", Type: "USAGE"})
	refused(t, err, 400, "billable_metric_id is required")
	_, err = c.CreateProduct(ctx, client.ProductInput{Name: "Credit", Type: "FIXED", BillableMetricID: id})
	refused(t, err, 400, "only valid for USAGE")
	refused(t, c.UpdateProduct(ctx, client.ProductUpdate{ProductID: product, StartingAt: "2026-10-02T15:40:00Z"}), 400, "hour boundary")

	// An archived metric still reads, and no new product can have it.
	if err := c.ArchiveBillableMetric(ctx, id); err != nil {
		t.Fatal(err)
	}
	if got, err := c.GetBillableMetric(ctx, id); err != nil || got.ArchivedAt == "" {
		t.Errorf("archived metric %+v, %v", got, err)
	}
	_, err = c.CreateProduct(ctx, client.ProductInput{Name: "Awake 2", Type: "USAGE", BillableMetricID: id})
	refused(t, err, 400, "archived")

	card, err := c.CreateRateCard(ctx, client.RateCardInput{Name: "Standard"})
	if err != nil {
		t.Fatal(err)
	}
	price := 20.0
	rate := client.RateInput{RateCardID: card, ProductID: product, StartingAt: "2026-10-01T00:00:00Z", Entitled: true, RateType: "FLAT", Price: &price}
	if err := c.AddRate(ctx, rate); err != nil {
		t.Fatal(err)
	}
	later, newPrice := rate, 25.0
	later.StartingAt, later.Price = "2026-11-01T00:00:00Z", &newPrice
	if err := c.AddRate(ctx, later); err != nil {
		t.Fatal(err)
	}
	for at, want := range map[string]float64{"2026-10-15T00:00:00Z": 20, "2026-11-01T00:00:00Z": 25, "2027-01-01T00:00:00Z": 25} {
		rates, err := c.RatesAt(ctx, card, product, at)
		if err != nil || len(rates) != 1 || *rates[0].Rate.Price != want {
			t.Errorf("at %s: %+v, %v; want one rate of %v", at, rates, err, want)
		}
	}
	if rates, _ := c.RatesAt(ctx, card, product, "2026-09-01T00:00:00Z"); len(rates) != 0 {
		t.Errorf("before any rate starts: %+v", rates)
	}
	if n := len(fake.Rates(card)); n != 2 {
		t.Errorf("%d rates on the schedule, want both", n)
	}

	// An archived product keeps its rates and takes no new one; the same
	// for an archived rate card.
	if err := c.ArchiveProduct(ctx, product); err != nil {
		t.Fatal(err)
	}
	refused(t, c.AddRate(ctx, rate), 400, "the product is archived")
	if rates, _ := c.RatesAt(ctx, card, product, "2026-10-15T00:00:00Z"); len(rates) != 1 {
		t.Error("the archived product's rate is gone")
	}
	credit, _ := c.CreateProduct(ctx, client.ProductInput{Name: "Credit", Type: "FIXED"})
	if err := c.ArchiveRateCard(ctx, card); err != nil {
		t.Fatal(err)
	}
	rate.ProductID = credit
	refused(t, c.AddRate(ctx, rate), 400, "the rate card is archived")
	if _, err := c.GetRateCard(ctx, card); err != nil {
		t.Errorf("an archived rate card does not read: %v", err)
	}
}

func TestAlertKeys(t *testing.T) {
	ctx := context.Background()
	fake, c := start(t)
	in := client.AlertInput{AlertType: "spend_threshold_reached", Name: "Spend", Threshold: 1000, UniquenessKey: "k"}
	id, err := c.CreateAlert(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.CreateAlert(ctx, in)
	refused(t, err, 409, "uniqueness key")

	// Archived without releasing, the key stays taken.
	if err := c.ArchiveAlert(ctx, id, false); err != nil {
		t.Fatal(err)
	}
	_, err = c.CreateAlert(ctx, in)
	refused(t, err, 409, "uniqueness key")
	if err := c.ArchiveAlert(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateAlert(ctx, in); err != nil {
		t.Errorf("after release: %v", err)
	}
	if fake.AlertStatus(id) != "archived" {
		t.Errorf("status %q", fake.AlertStatus(id))
	}
	got, err := c.CustomerAlert(ctx, "9b85c1c1-5238-4f2a-a409-61412905e1e1", id)
	if err != nil || got.Status != "archived" || got.Threshold != 1000 {
		t.Errorf("read %+v, %v", got, err)
	}
}

// A field the API does not have is refused, so that a provider sending one
// fails its tests instead of passing against a lenient fake.
func TestUnknownFieldsAreRefused(t *testing.T) {
	fake, _ := start(t)
	ts := httptest.NewServer(fake.Handler())
	defer ts.Close()
	req := httptest.NewRequest("POST", "/v1/contract-pricing/rate-cards/create", strings.NewReader(`{"name":"x","currency":"usd"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	fake.Handler().ServeHTTP(w, req)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "unknown field") {
		t.Errorf("%d %s", w.Code, w.Body.String())
	}
}
