package metronome

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/r33drichards/computer-use/backend/internal/billing"
)

const token = "made-up-token"

// api is Metronome's API as this package takes it to be (metronome.md):
// enough of it to check what the client sends and that it reads back what
// it wrote. It is not evidence of what the real API accepts.
type api struct {
	t  *testing.T
	mu sync.Mutex
	n  int
	// What was made, by the path that lists it.
	lists    map[string][]map[string]any
	keys     map[string]bool // uniqueness keys used
	requests []string        // "METHOD path" of every request
	bodies   map[string][]map[string]any
	fail     int // answer this status to everything, if set
}

var listOf = map[string]string{
	"/v1/billable-metrics/create":             "/v1/billable-metrics",
	"/v1/contract-pricing/products/create":    "/v1/contract-pricing/products/list",
	"/v1/contract-pricing/rate-cards/create":  "/v1/contract-pricing/rate-cards/list",
	"/v1/alerts/create":                       "/v1/customer-alerts/list-all",
	"/v1/customers":                           "/v1/customers",
	"/v1/contracts/customerCredits/create":    "/v1/contracts/customerBalances/list",
	"/v1/contracts/create":                    "contracts",
	"/v1/contract-pricing/rate-cards/addRate": "rates",
	"/v1/customFields/addKey":                 "keys",
}

func newAPI(t *testing.T) (*api, *Client) {
	a := &api{t: t, lists: map[string][]map[string]any{}, keys: map[string]bool{}, bodies: map[string][]map[string]any{}}
	ts := httptest.NewServer(a)
	t.Cleanup(ts.Close)
	return a, New(ts.URL+"/", token)
}

func (a *api) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	path := r.URL.Path
	a.requests = append(a.requests, r.Method+" "+path)
	if got := r.Header.Get("Authorization"); got != "Bearer "+token {
		a.t.Errorf("%s: Authorization %q", path, got)
	}
	body := map[string]any{}
	raw, _ := io.ReadAll(r.Body)
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			a.t.Errorf("%s: body %q", path, raw)
		}
	}
	a.bodies[path] = append(a.bodies[path], body)
	answer := func(status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	if a.fail != 0 {
		answer(a.fail, map[string]any{"message": "no"})
		return
	}
	switch {
	case path == "/v1/usage/groups":
		answer(200, map[string]any{"data": []map[string]any{
			{"starting_on": "2026-10-02T00:00:00Z", "group_value": "s-aaaaaaaaab", "value": 3600},
			{"starting_on": "2026-10-03T00:00:00Z", "group_value": "s-aaaaaaaaab", "value": 60},
			{"starting_on": "2026-10-03T00:00:00Z", "group_value": "s-aaaaaaaaac", "value": nil},
		}})
	case path == "/v1/contracts/customerCredits/updateEndDate":
		for _, c := range a.lists["/v1/contracts/customerBalances/list"] {
			if c["id"] == body["credit_id"] {
				c["access_schedule"].(map[string]any)["schedule_items"].([]any)[0].(map[string]any)["ending_before"] = body["access_ending_before"]
			}
		}
		answer(200, map[string]any{"data": map[string]any{"id": body["credit_id"]}})
	case listOf[path] != "" && r.Method == "POST" && (strings.HasSuffix(path, "create") || strings.HasSuffix(path, "addRate") || strings.HasSuffix(path, "addKey") || path == "/v1/customers"):
		key, _ := body["uniqueness_key"].(string)
		if path == "/v1/customFields/addKey" {
			key = "field/" + body["key"].(string)
		}
		if key != "" && a.keys[key] {
			answer(409, map[string]any{"message": "uniqueness key already used"})
			return
		}
		if key != "" {
			a.keys[key] = true
		}
		a.n++
		body["id"] = "id-" + strconv.Itoa(a.n)
		if path == "/v1/contracts/customerCredits/create" {
			body["balance"] = body["access_schedule"].(map[string]any)["schedule_items"].([]any)[0].(map[string]any)["amount"]
		}
		a.lists[listOf[path]] = append(a.lists[listOf[path]], body)
		answer(200, map[string]any{"data": map[string]any{"id": body["id"]}})
	default: // a list
		var out []map[string]any
		for _, item := range a.lists[path] {
			if alias := r.URL.Query().Get("ingest_alias"); alias != "" && item["ingest_aliases"].([]any)[0] != alias {
				continue
			}
			if customer, ok := body["customer_id"]; ok && item["customer_id"] != customer {
				continue
			}
			out = append(out, item)
		}
		answer(200, map[string]any{"data": out})
	}
}

func (a *api) last(path string) map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	all := a.bodies[path]
	if len(all) == 0 {
		a.t.Fatalf("no request to %s", path)
	}
	return all[len(all)-1]
}

func (a *api) count(path string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.lists[path])
}

// defined is what OpenTofu (infra/billing) defines in Metronome, which the
// client finds by name.
func (a *api) defined() {
	a.lists["/v1/contract-pricing/products/list"] = []map[string]any{
		{"id": "prod-awake", "current": map[string]any{"name": ProductAwake}},
		{"id": "prod-credit", "current": map[string]any{"name": ProductCredit}},
	}
	a.lists["/v1/billable-metrics"] = []map[string]any{{"id": "bm-awake", "name": MetricAwake}, {"id": "bm-disk", "name": MetricDisk}}
}

// The client says to Metronome what metronome.md says it says, and reads
// back what it wrote.
func TestClientCalls(t *testing.T) {
	a, c := newAPI(t)
	ctx := t.Context()
	// Before OpenTofu has made the objects, the client says what is missing.
	if _, _, err := c.CreateCredit(ctx, billing.MetronomeCreditParams{Customer: "x"}); err == nil || !strings.Contains(err.Error(), ProductCredit) {
		t.Fatalf("with no Credit product: %v", err)
	}
	a.defined()

	if _, found, err := c.CustomerByAlias(ctx, "acct-abc"); err != nil || found {
		t.Fatalf("an alias nobody has: %v, %v", found, err)
	}
	id, err := c.CreateCustomer(ctx, billing.MetronomeCustomerParams{Name: "abc", IngestAlias: "acct-abc"})
	if err != nil || id == "" {
		t.Fatal(id, err)
	}
	if body := a.last("/v1/customers"); body["name"] != "abc" || body["ingest_aliases"].([]any)[0] != "acct-abc" {
		t.Errorf("customer %v", body)
	}
	if got, found, err := c.CustomerByAlias(ctx, "acct-abc"); err != nil || !found || got != id {
		t.Fatalf("by alias: %q, %v, %v", got, found, err)
	}

	contract := billing.MetronomeContractParams{Customer: id, RateCardAlias: billing.RateCard, StartingAt: time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC), UniquenessKey: "contract/acct-abc"}
	for range 2 { // the second is a 409, which is success
		if err := c.CreateContract(ctx, contract); err != nil {
			t.Fatal(err)
		}
	}
	if body := a.last("/v1/contracts/create"); body["rate_card_alias"] != "cu-standard-v1" || body["starting_at"] != "2026-10-02T10:00:00Z" ||
		body["usage_statement_schedule"].(map[string]any)["frequency"] != "MONTHLY" || a.count("contracts") != 1 {
		t.Errorf("contract %v (%d made)", body, a.count("contracts"))
	}

	credit := billing.MetronomeCreditParams{Customer: id, Name: "Sign-up credit", UniquenessKey: "signup/fpA", Priority: 20, AmountMicros: 5_000_000,
		StartingAt: time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC), EndingBefore: time.Date(2026, 12, 31, 10, 0, 0, 0, time.UTC),
		CustomFields: map[string]string{"grant_key": "signup/fpA", "source": "signup"}}
	creditID, conflict, err := c.CreateCredit(ctx, credit)
	if err != nil || conflict || creditID == "" {
		t.Fatal(creditID, conflict, err)
	}
	body := a.last("/v1/contracts/customerCredits/create")
	item := body["access_schedule"].(map[string]any)["schedule_items"].([]any)[0].(map[string]any)
	if body["rate_type"] != "COMMIT_RATE" || body["priority"] != float64(20) || body["uniqueness_key"] != "signup/fpA" || body["product_id"] != "prod-credit" ||
		item["amount"] != float64(500) || item["ending_before"] != "2026-12-31T10:00:00Z" {
		t.Errorf("credit %v", body)
	}
	if _, conflict, err := c.CreateCredit(ctx, credit); err != nil || !conflict {
		t.Fatalf("the same key again: conflict %v, %v", conflict, err)
	}

	credits, err := c.Credits(ctx, id)
	if err != nil || len(credits) != 1 {
		t.Fatal(credits, err)
	}
	if got := credits[0]; got.ID != creditID || got.AmountMicros != 5_000_000 || got.BalanceMicros != 5_000_000 || got.Priority != 20 ||
		got.CustomFields["grant_key"] != "signup/fpA" || !got.EndingBefore.Equal(credit.EndingBefore) || got.Archived {
		t.Errorf("read back %+v", got)
	}
	if other, err := c.Credits(ctx, "someone-else"); err != nil || len(other) != 0 {
		t.Errorf("another customer's credits: %v, %v", other, err)
	}
	if err := c.ArchiveCredit(ctx, id, creditID); err != nil {
		t.Fatal(err)
	}
	if credits, _ := c.Credits(ctx, id); credits[0].EndingBefore.After(time.Now()) {
		t.Errorf("after the archive the credit still ends at %v", credits[0].EndingBefore)
	}

	usage, err := c.Usage(ctx, id, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC))
	if err != nil || len(usage.Rows) != 2 {
		t.Fatal(usage, err)
	}
	if r := usage.Rows[0]; r.SessionID != "s-aaaaaaaaab" || r.Day != "2026-10-02" || r.AwakeSeconds != 3600 || r.DiskGBSeconds != 3600 {
		t.Errorf("usage %+v", usage.Rows)
	}
	if body := a.last("/v1/usage/groups"); body["billable_metric_id"] != "bm-disk" || body["window_size"] != "DAY" || body["group_by"].(map[string]any)["key"] != "session_id" ||
		body["starting_on"] != "2026-10-01T00:00:00Z" || body["ending_before"] != "2026-10-04T00:00:00Z" {
		t.Errorf("usage request %v", body)
	}
}

// An answer that is not a success is an error that names the path and
// never the token; an unreachable Metronome likewise.
func TestErrorsHideTheToken(t *testing.T) {
	a, c := newAPI(t)
	a.fail = http.StatusInternalServerError
	_, err := c.Credits(t.Context(), "x")
	if err == nil || !strings.Contains(err.Error(), "500") || strings.Contains(err.Error(), token) {
		t.Fatalf("err = %v", err)
	}
	gone := New("http://127.0.0.1:1", token)
	if _, err := gone.CreateCustomer(t.Context(), billing.MetronomeCustomerParams{Name: "a", IngestAlias: "b"}); err == nil || strings.Contains(err.Error(), token) {
		t.Fatalf("err = %v", err)
	}
	if cents(5_000_000) != 500 || micros(0.3) != 3000 || micros(500) != 5_000_000 {
		t.Errorf("cents %v, micros %v %v", cents(5_000_000), micros(0.3), micros(500))
	}
}
