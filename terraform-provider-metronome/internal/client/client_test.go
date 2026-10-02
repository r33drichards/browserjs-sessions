package client

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const token = "metronome-test-token-0123456789"

// serve answers every request with handler and returns a client for it.
func serve(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	c, err := New(ts.URL, token, "1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	c.SetRetryWait(func(int) time.Duration { return time.Millisecond })
	return c
}

func TestNewChecksItsArguments(t *testing.T) {
	for _, endpoint := range []string{"", "api.metronome.com", "ftp://api.metronome.com", "https://"} {
		if _, err := New(endpoint, token, "v"); err == nil {
			t.Errorf("endpoint %q was accepted", endpoint)
		}
	}
	if _, err := New("https://api.metronome.com", "", "v"); err == nil {
		t.Error("an empty token was accepted")
	}
}

func TestRequestsCarryTheTokenAndTheBody(t *testing.T) {
	var got *http.Request
	var body []byte
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		got = r
		body, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"data":{"id":"58fb0650-e54a-4d17-93cb-ba8e56c32c65"}}`))
	})
	id, err := c.CreateBillableMetric(context.Background(), BillableMetric{
		Name: "Awake seconds", AggregationType: "SUM", AggregationKey: "seconds",
		EventTypeFilter: &EventTypeFilter{InValues: []string{"session.awake"}},
	})
	if err != nil || id != "58fb0650-e54a-4d17-93cb-ba8e56c32c65" {
		t.Fatalf("id %q, err %v", id, err)
	}
	if got.Method != "POST" || got.URL.Path != "/v1/billable-metrics/create" {
		t.Errorf("%s %s", got.Method, got.URL.Path)
	}
	if got.Header.Get("Authorization") != "Bearer "+token || got.Header.Get("User-Agent") != "terraform-provider-metronome/1.2.3" || got.Header.Get("Content-Type") != "application/json" {
		t.Errorf("headers %v", got.Header)
	}
	// What is not set is not sent.
	want := `{"name":"Awake seconds","event_type_filter":{"in_values":["session.awake"]},"aggregation_type":"SUM","aggregation_key":"seconds"}`
	if string(body) != want {
		t.Errorf("body %s\nwant %s", body, want)
	}
}

func TestErrorsCarryTheStatusAndMessageAndNeverTheToken(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"message":"starting_at must be on an hour boundary"}`))
	})
	err := c.AddRate(context.Background(), RateInput{})
	if StatusOf(err) != 400 || !strings.Contains(err.Error(), "starting_at must be on an hour boundary") {
		t.Errorf("err %v", err)
	}
	if IsNotFound(err) {
		t.Error("a 400 is reported as not found")
	}

	// An answer that is not JSON still has its status.
	c = serve(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "<html>bad gateway</html>", http.StatusBadGateway)
	})
	_, err = c.GetProduct(context.Background(), "x")
	if StatusOf(err) != 502 || err.Error() != "Metronome answered 502 Bad Gateway" {
		t.Errorf("err %v", err)
	}

	// A host that does not answer names the path, not the token.
	c, _ = New("http://127.0.0.1:1", token, "v")
	_, err = c.GetRateCard(context.Background(), "x")
	if err == nil || strings.Contains(err.Error(), token) || !strings.Contains(err.Error(), "/v1/contract-pricing/rate-cards/get") {
		t.Errorf("err %v", err)
	}
	if StatusOf(err) != 0 {
		t.Errorf("status %d for a transport error", StatusOf(err))
	}
}

func TestRateLimitIsRetried(t *testing.T) {
	calls := 0
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 3 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`{"data":{"id":"p","type":"USAGE","current":{"name":"n"}}}`))
	})
	p, err := c.GetProduct(context.Background(), "p")
	if err != nil || p.Current.Name != "n" || calls != 3 {
		t.Errorf("product %+v, err %v, calls %d", p, err, calls)
	}

	// It gives up, and says 429.
	calls = 0
	c = serve(t, func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(http.StatusTooManyRequests) })
	if err := c.ArchiveProduct(context.Background(), "p"); StatusOf(err) != 429 || calls != maxRetries+1 {
		t.Errorf("err %v after %d calls", err, calls)
	}

	// A wait that outlasts the context ends with the context.
	c.SetRetryWait(func(int) time.Duration { return time.Hour })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := c.ArchiveProduct(ctx, "p"); err != context.DeadlineExceeded {
		t.Errorf("err %v, want the context's", err)
	}
}

// For the two nullable fields of a product update, "leave it" and "remove
// it" are different bodies.
func TestProductUpdateTellsAbsentFromNull(t *testing.T) {
	var bodies []string
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		_, _ = w.Write([]byte(`{"data":{"id":"p"}}`))
	})
	var none *QuantityConversion
	empty := []string{}
	for _, in := range []ProductUpdate{
		{ProductID: "p", StartingAt: "2026-10-02T15:00:00Z", Name: "n"},
		{ProductID: "p", StartingAt: "2026-10-02T15:00:00Z", QuantityConversion: &none, Tags: &empty},
	} {
		if err := c.UpdateProduct(context.Background(), in); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{
		`{"product_id":"p","starting_at":"2026-10-02T15:00:00Z","name":"n"}`,
		`{"product_id":"p","starting_at":"2026-10-02T15:00:00Z","tags":[],"quantity_conversion":null}`,
	}
	for i := range want {
		if bodies[i] != want[i] {
			t.Errorf("body %s\nwant %s", bodies[i], want[i])
		}
	}
}

func TestListsFollowPages(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		page := map[string]any{"data": []CreditType{{ID: "1", Name: "USD (cents)", IsCurrency: true}}, "next_page": "abc"}
		if r.URL.Query().Get("next_page") == "abc" {
			page = map[string]any{"data": []CreditType{{ID: "2", Name: "credits"}}, "next_page": nil}
		}
		_ = json.NewEncoder(w).Encode(page)
	})
	units, err := c.ListCreditTypes(context.Background())
	if err != nil || len(units) != 2 || units[1].Name != "credits" {
		t.Errorf("units %+v, err %v", units, err)
	}
}
