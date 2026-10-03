package metronome

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/r33drichards/computer-use/backend/internal/billing"
)

// DefaultURL is Metronome's API. The token alone decides the environment
// (sandbox or production).
const DefaultURL = "https://api.metronome.com"

// The names of the objects OpenTofu (infra/billing) defines in Metronome
// (metronome.md, "Objects defined in OpenTofu"). The client finds them by
// these and takes no ID from anywhere else: a name here is a contract.
const (
	MetricAwake   = "cu_awake_seconds_v1"
	MetricDisk    = "cu_disk_gb_seconds_v1"
	ProductAwake  = "Awake time"
	ProductDisk   = "Disk"
	ProductCredit = "Credit"
)

// AwakeMetric is the metric of the awake seconds of sessions of a size:
// MetricAwake for small, and one of its own for every other size
// (metronome.md, "Sizes").
func AwakeMetric(size string) string {
	if size == "" || size == billing.SizeSmall {
		return MetricAwake
	}
	return "cu_awake_" + size + "_seconds_v1"
}

// errNotDefined is an object OpenTofu has not defined in this environment.
var errNotDefined = errors.New("not defined")

// Client is billing.Metronome over HTTP: one method per endpoint, no
// logic. The request and response shapes are from Metronome's
// documentation as metronome.md records it and have not been run against
// a sandbox (checks M1 to M11); docs/billing-enforcement.md lists each.
//
// It remembers the IDs of the objects OpenTofu defined (the Credit
// product, the two metrics), which never change. It remembers nothing of
// any customer.
type Client struct {
	base  string
	token string
	http  *http.Client

	// Sizes, if set, is the sizes of session other than small that are
	// charged (the catalogue's): the usage of each is read from its metric.
	Sizes func() []string

	mu  sync.Mutex
	ids map[string]string // "product/Credit", "metric/cu_awake_seconds_v1" -> ID
}

// New is a client for the API at base (DefaultURL if empty). The token is
// sent and never logged or put in an error.
func New(base, token string) *Client {
	if base == "" {
		base = DefaultURL
	}
	return &Client{base: strings.TrimRight(base, "/"), token: token, http: &http.Client{Timeout: 10 * time.Second}, ids: map[string]string{}}
}

// APIError is an answer that is not a success.
type APIError struct {
	Status int
	Path   string
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("metronome: %s answered %d: %s", e.Path, e.Status, e.Body)
}

// do sends one request. out, when not nil, receives a 2xx body.
func (c *Client) do(ctx context.Context, method, path string, in, out any) (int, error) {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return 0, err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// A *url.Error names the method and URL, never a header.
		return 0, fmt.Errorf("metronome: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return resp.StatusCode, fmt.Errorf("metronome: %s: reading the answer: %w", path, err)
	}
	if resp.StatusCode/100 != 2 {
		text := strings.TrimSpace(string(raw))
		if len(text) > 300 {
			text = text[:300]
		}
		return resp.StatusCode, &APIError{Status: resp.StatusCode, Path: path, Body: text}
	}
	if out != nil && len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, fmt.Errorf("metronome: %s: the answer is not the JSON expected: %w", path, err)
		}
	}
	return resp.StatusCode, nil
}

// conflict reports a 409: the uniqueness key was used.
func conflict(status int) bool { return status == http.StatusConflict }

type idData struct {
	Data struct {
		ID string `json:"id"`
	} `json:"data"`
}

func (c *Client) CreateCustomer(ctx context.Context, p billing.MetronomeCustomerParams) (string, error) {
	var out idData
	_, err := c.do(ctx, "POST", "/v1/customers", map[string]any{"name": p.Name, "ingest_aliases": []string{p.IngestAlias}}, &out)
	return out.Data.ID, err
}

func (c *Client) CustomerByAlias(ctx context.Context, alias string) (string, bool, error) {
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if _, err := c.do(ctx, "GET", "/v1/customers?ingest_alias="+url.QueryEscape(alias), nil, &out); err != nil {
		return "", false, err
	}
	if len(out.Data) == 0 {
		return "", false, nil
	}
	return out.Data[0].ID, true, nil
}

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func (c *Client) CreateContract(ctx context.Context, p billing.MetronomeContractParams) error {
	status, err := c.do(ctx, "POST", "/v1/contracts/create", map[string]any{
		"customer_id": p.Customer, "rate_card_alias": p.RateCardAlias, "starting_at": stamp(p.StartingAt),
		"usage_statement_schedule": map[string]any{"frequency": "MONTHLY"}, "uniqueness_key": p.UniquenessKey,
	}, nil)
	if conflict(status) {
		return nil
	}
	return err
}

// named finds the ID of an object OpenTofu defined, by its name, and
// remembers it.
func (c *Client) named(ctx context.Context, kind, listPath, method, name string) (string, error) {
	c.mu.Lock()
	id, ok := c.ids[kind+"/"+name]
	c.mu.Unlock()
	if ok {
		return id, nil
	}
	found, err := c.find(ctx, method, listPath, name)
	if err != nil {
		return "", err
	}
	if found == "" {
		return "", fmt.Errorf("metronome: no %s named %q; apply infra/billing to this environment: %w", kind, name, errNotDefined)
	}
	c.mu.Lock()
	c.ids[kind+"/"+name] = found
	c.mu.Unlock()
	return found, nil
}

// listed is an entry of any of Metronome's lists, as far as its name goes.
type listed struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Current *struct {
		Name string `json:"name"`
	} `json:"current"`
	Aliases []struct {
		Name string `json:"name"`
	} `json:"aliases"`
}

func (l listed) is(name string) bool {
	if l.Name == name || (l.Current != nil && l.Current.Name == name) {
		return true
	}
	for _, a := range l.Aliases {
		if a.Name == name {
			return true
		}
	}
	return false
}

// find pages through a list for an entry with the name ("" for none).
func (c *Client) find(ctx context.Context, method, path, name string) (string, error) {
	page := ""
	for {
		var out struct {
			Data     []listed `json:"data"`
			NextPage string   `json:"next_page"`
		}
		at := path
		if page != "" {
			sep := "?"
			if strings.Contains(path, "?") {
				sep = "&"
			}
			at += sep + "next_page=" + url.QueryEscape(page)
		}
		var in any
		if method == "POST" {
			in = map[string]any{}
		}
		if _, err := c.do(ctx, method, at, in, &out); err != nil {
			return "", err
		}
		for _, item := range out.Data {
			if item.is(name) {
				return item.ID, nil
			}
		}
		if out.NextPage == "" {
			return "", nil
		}
		page = out.NextPage
	}
}

func (c *Client) product(ctx context.Context, name string) (string, error) {
	return c.named(ctx, "product", "/v1/contract-pricing/products/list", "POST", name)
}

func (c *Client) metric(ctx context.Context, name string) (string, error) {
	return c.named(ctx, "metric", "/v1/billable-metrics", "GET", name)
}

// cents is micro-dollars as US cents, which may be fractional.
func cents(micros int64) float64 { return float64(micros) / 10000 }

// micros is US cents as micro-dollars, rounded down.
func micros(cents float64) int64 { return int64(math.Floor(cents*10000 + 1e-6)) }

func (c *Client) CreateCredit(ctx context.Context, p billing.MetronomeCreditParams) (string, bool, error) {
	product, err := c.product(ctx, ProductCredit)
	if err != nil {
		return "", false, err
	}
	var out idData
	status, err := c.do(ctx, "POST", "/v1/contracts/customerCredits/create", map[string]any{
		"customer_id": p.Customer, "product_id": product, "name": p.Name, "uniqueness_key": p.UniquenessKey,
		"priority": p.Priority, "rate_type": "COMMIT_RATE",
		"access_schedule": map[string]any{"schedule_items": []map[string]any{{
			"amount": cents(p.AmountMicros), "starting_at": stamp(p.StartingAt), "ending_before": stamp(p.EndingBefore),
		}}},
		"custom_fields": p.CustomFields,
	}, &out)
	if conflict(status) {
		return "", true, nil
	}
	return out.Data.ID, false, err
}

func (c *Client) Credits(ctx context.Context, customer string) ([]billing.MetronomeCredit, error) {
	var credits []billing.MetronomeCredit
	page := ""
	for {
		var out struct {
			Data []struct {
				ID             string            `json:"id"`
				Name           string            `json:"name"`
				Priority       float64           `json:"priority"`
				Balance        float64           `json:"balance"`
				ArchivedAt     string            `json:"archived_at"`
				CustomFields   map[string]string `json:"custom_fields"`
				AccessSchedule struct {
					ScheduleItems []struct {
						Amount       float64   `json:"amount"`
						StartingAt   time.Time `json:"starting_at"`
						EndingBefore time.Time `json:"ending_before"`
					} `json:"schedule_items"`
				} `json:"access_schedule"`
			} `json:"data"`
			NextPage string `json:"next_page"`
		}
		in := map[string]any{"customer_id": customer, "include_balance": true, "include_archived": true}
		if page != "" {
			in["next_page"] = page
		}
		if _, err := c.do(ctx, "POST", "/v1/contracts/customerBalances/list", in, &out); err != nil {
			return nil, err
		}
		for _, d := range out.Data {
			credit := billing.MetronomeCredit{ID: d.ID, Name: d.Name, Priority: int(d.Priority), BalanceMicros: micros(d.Balance),
				Archived: d.ArchivedAt != "", CustomFields: d.CustomFields}
			for i, item := range d.AccessSchedule.ScheduleItems {
				credit.AmountMicros += micros(item.Amount)
				if i == 0 || item.StartingAt.Before(credit.StartingAt) {
					credit.StartingAt = item.StartingAt
				}
				if item.EndingBefore.After(credit.EndingBefore) {
					credit.EndingBefore = item.EndingBefore
				}
			}
			credits = append(credits, credit)
		}
		if out.NextPage == "" {
			return credits, nil
		}
		page = out.NextPage
	}
}

// ArchiveCredit ends a credit's access now: it counts for nothing from
// here on, and what was used of it stays used.
func (c *Client) ArchiveCredit(ctx context.Context, customer, id string) error {
	_, err := c.do(ctx, "POST", "/v1/contracts/customerCredits/updateEndDate", map[string]any{
		"customer_id": customer, "credit_id": id, "access_ending_before": stamp(time.Now().UTC().Truncate(time.Hour)),
	}, nil)
	return err
}

// Usage reads the customer's metrics by session and UTC day: awake seconds
// (one metric for each size) and GB-seconds of disk.
func (c *Client) Usage(ctx context.Context, customer string, from, to time.Time) (billing.MetronomeUsage, error) {
	rows := map[[2]string]*billing.MetronomeUsageRow{}
	var order [][2]string
	type metric struct {
		name     string
		optional bool // a size's: absent until infra/billing is applied with it
		add      func(*billing.MetronomeUsageRow, int64)
	}
	metrics := []metric{
		{MetricAwake, false, func(r *billing.MetronomeUsageRow, n int64) { r.AwakeSeconds += n }},
		{MetricDisk, false, func(r *billing.MetronomeUsageRow, n int64) { r.DiskGBSeconds += n }},
	}
	if c.Sizes != nil {
		for _, size := range c.Sizes() {
			metrics = append(metrics, metric{AwakeMetric(size), true, func(r *billing.MetronomeUsageRow, n int64) {
				if r.AwakeBySize == nil {
					r.AwakeBySize = map[string]int64{}
				}
				r.AwakeSeconds += n
				r.AwakeBySize[size] += n
			}})
		}
	}
	for _, m := range metrics {
		metric, err := c.metric(ctx, m.name)
		if m.optional && errors.Is(err, errNotDefined) {
			continue
		}
		if err != nil {
			return billing.MetronomeUsage{}, err
		}
		page := ""
		for {
			var out struct {
				Data []struct {
					StartingOn time.Time `json:"starting_on"`
					GroupValue string    `json:"group_value"`
					Value      *float64  `json:"value"`
				} `json:"data"`
				NextPage string `json:"next_page"`
			}
			path := "/v1/usage/groups"
			if page != "" {
				path += "?next_page=" + url.QueryEscape(page)
			}
			_, err := c.do(ctx, "POST", path, map[string]any{
				"customer_id": customer, "billable_metric_id": metric, "window_size": "DAY",
				"group_by":    map[string]any{"key": "session_id"},
				"starting_on": stamp(from.UTC().Truncate(24 * time.Hour)), "ending_before": stamp(to.UTC().Add(24*time.Hour - time.Nanosecond).Truncate(24 * time.Hour)),
			}, &out)
			if err != nil {
				return billing.MetronomeUsage{}, err
			}
			for _, d := range out.Data {
				if d.Value == nil || d.GroupValue == "" {
					continue
				}
				key := [2]string{d.GroupValue, d.StartingOn.UTC().Format("2006-01-02")}
				if rows[key] == nil {
					rows[key] = &billing.MetronomeUsageRow{SessionID: key[0], Day: key[1]}
					order = append(order, key)
				}
				m.add(rows[key], int64(math.Round(*d.Value)))
			}
			if out.NextPage == "" {
				break
			}
			page = out.NextPage
		}
	}
	var usage billing.MetronomeUsage
	for _, key := range order {
		usage.Rows = append(usage.Rows, *rows[key])
	}
	return usage, nil
}
