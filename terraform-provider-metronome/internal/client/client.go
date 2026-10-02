// Package client speaks the part of Metronome's API that configures pricing:
// billable metrics, products, rate cards, rates, threshold notifications and
// pricing units. The paths and bodies are those of
// https://docs.metronome.com/openapi.json, read on 2026-10-02.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// USDCreditTypeID is Metronome's built-in pricing unit "USD (cents)".
const USDCreditTypeID = "2714e483-4ff1-48e4-9e25-ac732e8f24f2"

// EventTypeFilter matches the event_type of a usage event.
type EventTypeFilter struct {
	InValues    []string `json:"in_values,omitempty"`
	NotInValues []string `json:"not_in_values,omitempty"`
}

// PropertyFilter is one rule on a property of a usage event.
type PropertyFilter struct {
	Name        string   `json:"name"`
	Exists      *bool    `json:"exists,omitempty"`
	InValues    []string `json:"in_values,omitempty"`
	NotInValues []string `json:"not_in_values,omitempty"`
}

// BillableMetric is a billable metric. Only Name can change after creation.
type BillableMetric struct {
	ID              string            `json:"id,omitempty"`
	Name            string            `json:"name"`
	EventTypeFilter *EventTypeFilter  `json:"event_type_filter,omitempty"`
	PropertyFilters []PropertyFilter  `json:"property_filters,omitempty"`
	AggregationType string            `json:"aggregation_type,omitempty"`
	AggregationKey  string            `json:"aggregation_key,omitempty"`
	GroupKeys       [][]string        `json:"group_keys,omitempty"`
	SQL             string            `json:"sql,omitempty"`
	CustomFields    map[string]string `json:"custom_fields,omitempty"`
	ArchivedAt      string            `json:"archived_at,omitempty"`
}

// QuantityConversion multiplies or divides a usage product's quantity
// before it is priced.
type QuantityConversion struct {
	Name             string  `json:"name,omitempty"`
	ConversionFactor float64 `json:"conversion_factor"`
	Operation        string  `json:"operation"`
}

// QuantityRounding rounds a usage product's quantity before it is priced.
type QuantityRounding struct {
	RoundingMethod string  `json:"rounding_method"`
	DecimalPlaces  float64 `json:"decimal_places"`
}

// ProductInput is the body of a product create.
type ProductInput struct {
	Name                 string              `json:"name"`
	Type                 string              `json:"type"`
	BillableMetricID     string              `json:"billable_metric_id,omitempty"`
	Tags                 []string            `json:"tags,omitempty"`
	QuantityConversion   *QuantityConversion `json:"quantity_conversion,omitempty"`
	QuantityRounding     *QuantityRounding   `json:"quantity_rounding,omitempty"`
	PricingGroupKey      []string            `json:"pricing_group_key,omitempty"`
	PresentationGroupKey []string            `json:"presentation_group_key,omitempty"`
}

// ProductUpdate is the body of a product update. An update is a new entry
// in the product's history that takes effect at StartingAt, which must be on
// an hour boundary. A nil field keeps the product's current value. For
// QuantityConversion and QuantityRounding, a pointer to nil sends null,
// which removes it.
type ProductUpdate struct {
	ProductID            string               `json:"product_id"`
	StartingAt           string               `json:"starting_at"`
	Name                 string               `json:"name,omitempty"`
	BillableMetricID     string               `json:"billable_metric_id,omitempty"`
	Tags                 *[]string            `json:"tags,omitempty"`
	QuantityConversion   **QuantityConversion `json:"quantity_conversion,omitempty"`
	QuantityRounding     **QuantityRounding   `json:"quantity_rounding,omitempty"`
	PricingGroupKey      *[]string            `json:"pricing_group_key,omitempty"`
	PresentationGroupKey *[]string            `json:"presentation_group_key,omitempty"`
}

// ProductState is a product as of one moment of its history.
type ProductState struct {
	Name                 string              `json:"name"`
	BillableMetricID     string              `json:"billable_metric_id,omitempty"`
	Tags                 []string            `json:"tags,omitempty"`
	QuantityConversion   *QuantityConversion `json:"quantity_conversion,omitempty"`
	QuantityRounding     *QuantityRounding   `json:"quantity_rounding,omitempty"`
	PricingGroupKey      []string            `json:"pricing_group_key,omitempty"`
	PresentationGroupKey []string            `json:"presentation_group_key,omitempty"`
}

// Product is a product as the API shows it.
type Product struct {
	ID         string       `json:"id"`
	Type       string       `json:"type"`
	ArchivedAt string       `json:"archived_at,omitempty"`
	Current    ProductState `json:"current"`
}

// Alias is a name a contract can use in place of a rate card's ID.
type Alias struct {
	Name         string `json:"name"`
	StartingAt   string `json:"starting_at,omitempty"`
	EndingBefore string `json:"ending_before,omitempty"`
}

// CreditType is a pricing unit: a currency, or a custom unit.
type CreditType struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	IsCurrency bool   `json:"is_currency,omitempty"`
}

// RateCardInput is the body of a rate card create.
type RateCardInput struct {
	Name             string  `json:"name"`
	Description      string  `json:"description,omitempty"`
	FiatCreditTypeID string  `json:"fiat_credit_type_id,omitempty"`
	Aliases          []Alias `json:"aliases,omitempty"`
}

// RateCardUpdate is the body of a rate card update. Aliases replaces the
// whole list.
type RateCardUpdate struct {
	RateCardID  string  `json:"rate_card_id"`
	Name        string  `json:"name,omitempty"`
	Description *string `json:"description,omitempty"`
	Aliases     []Alias `json:"aliases"`
}

// RateCard is a rate card as the API shows it. The API does not say whether
// a rate card is archived.
type RateCard struct {
	ID             string      `json:"id"`
	Name           string      `json:"name"`
	Description    string      `json:"description,omitempty"`
	FiatCreditType *CreditType `json:"fiat_credit_type,omitempty"`
	Aliases        []Alias     `json:"aliases,omitempty"`
}

// Tier is one tier of a TIERED rate. A tier without a size is the last.
type Tier struct {
	Size  *float64 `json:"size,omitempty"`
	Price float64  `json:"price"`
}

// CommitRate is the price used in place of the list price while usage is
// paid for from a commit or a credit.
type CommitRate struct {
	RateType string   `json:"rate_type"`
	Price    *float64 `json:"price,omitempty"`
}

// RateInput is the body of addRate. Rates are append-only: a rate is never
// edited or removed, only followed by a later one.
type RateInput struct {
	RateCardID         string            `json:"rate_card_id"`
	ProductID          string            `json:"product_id"`
	StartingAt         string            `json:"starting_at"`
	EndingBefore       string            `json:"ending_before,omitempty"`
	Entitled           bool              `json:"entitled"`
	RateType           string            `json:"rate_type"`
	Price              *float64          `json:"price,omitempty"`
	CreditTypeID       string            `json:"credit_type_id,omitempty"`
	Tiers              []Tier            `json:"tiers,omitempty"`
	Quantity           *float64          `json:"quantity,omitempty"`
	IsProrated         *bool             `json:"is_prorated,omitempty"`
	BillingFrequency   string            `json:"billing_frequency,omitempty"`
	PricingGroupValues map[string]string `json:"pricing_group_values,omitempty"`
	CommitRate         *CommitRate       `json:"commit_rate,omitempty"`
}

// Rate is the priced part of a rate.
type Rate struct {
	RateType   string      `json:"rate_type"`
	Price      *float64    `json:"price,omitempty"`
	Tiers      []Tier      `json:"tiers,omitempty"`
	Quantity   *float64    `json:"quantity,omitempty"`
	IsProrated *bool       `json:"is_prorated,omitempty"`
	CreditType *CreditType `json:"credit_type,omitempty"`
}

// RateScheduleEntry is one rate of a rate card, in force from StartingAt.
type RateScheduleEntry struct {
	ProductID          string            `json:"product_id"`
	ProductName        string            `json:"product_name,omitempty"`
	PricingGroupValues map[string]string `json:"pricing_group_values,omitempty"`
	StartingAt         string            `json:"starting_at"`
	EndingBefore       string            `json:"ending_before,omitempty"`
	Entitled           bool              `json:"entitled"`
	Rate               Rate              `json:"rate"`
	CommitRate         *CommitRate       `json:"commit_rate,omitempty"`
	BillingFrequency   string            `json:"billing_frequency,omitempty"`
}

// CustomFieldKey allows a custom field on one kind of object.
type CustomFieldKey struct {
	Entity            string `json:"entity"`
	Key               string `json:"key"`
	EnforceUniqueness bool   `json:"enforce_uniqueness"`
}

// AlertInput is the body of a threshold notification create.
type AlertInput struct {
	AlertType              string   `json:"alert_type"`
	Name                   string   `json:"name"`
	Threshold              float64  `json:"threshold"`
	CreditTypeID           string   `json:"credit_type_id,omitempty"`
	CustomerID             string   `json:"customer_id,omitempty"`
	BillableMetricID       string   `json:"billable_metric_id,omitempty"`
	UniquenessKey          string   `json:"uniqueness_key,omitempty"`
	EvaluateOnCreate       *bool    `json:"evaluate_on_create,omitempty"`
	CreditGrantTypeFilters []string `json:"credit_grant_type_filters,omitempty"`
}

// Alert is a threshold notification as the API shows it for one customer.
type Alert struct {
	ID            string      `json:"id"`
	Name          string      `json:"name"`
	Type          string      `json:"type"`
	Status        string      `json:"status"`
	Threshold     float64     `json:"threshold"`
	UniquenessKey string      `json:"uniqueness_key,omitempty"`
	CreditType    *CreditType `json:"credit_type,omitempty"`
}

// APIError is any answer that is not a success.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("Metronome answered %d %s", e.Status, http.StatusText(e.Status))
	}
	return fmt.Sprintf("Metronome answered %d: %s", e.Status, e.Message)
}

// StatusOf is the HTTP status of err when it is an APIError, and 0 otherwise.
func StatusOf(err error) int {
	var e *APIError
	if errors.As(err, &e) {
		return e.Status
	}
	return 0
}

// IsNotFound reports a 404.
func IsNotFound(err error) bool { return StatusOf(err) == http.StatusNotFound }

// Client is an API client. The token is sent and never printed.
type Client struct {
	base      string
	token     string
	userAgent string
	http      *http.Client
	// retryWait is how long to wait before the nth retry of a 429 (from 0).
	retryWait func(n int) time.Duration
}

// maxRetries is how often a rate-limited request is sent again. Most of
// these endpoints allow 8 requests a second.
const maxRetries = 5

// New makes a client for the API at endpoint (https://api.metronome.com).
func New(endpoint, token, version string) (*Client, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, fmt.Errorf("endpoint must be an http(s) URL such as https://api.metronome.com")
	}
	if token == "" {
		return nil, errors.New("token is empty")
	}
	return &Client{
		base:      strings.TrimRight(endpoint, "/"),
		token:     token,
		userAgent: "terraform-provider-metronome/" + version,
		http:      &http.Client{Timeout: 60 * time.Second},
		retryWait: func(n int) time.Duration { return time.Second << min(n, 4) },
	}, nil
}

// SetRetryWait replaces the wait before a retry; tests make it short.
func (c *Client) SetRetryWait(f func(n int) time.Duration) { c.retryWait = f }

// do sends one request, again after a 429. out, when not nil, receives a 2xx
// body.
func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body []byte
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = b
	}
	for n := 0; ; n++ {
		status, retryAfter, err := c.once(ctx, method, path, body, out)
		if status != http.StatusTooManyRequests || n >= maxRetries {
			return err
		}
		wait := c.retryWait(n)
		if retryAfter > 0 {
			wait = retryAfter
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}

func (c *Client) once(ctx context.Context, method, path string, body []byte, out any) (int, time.Duration, error) {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, r)
	if err != nil {
		return 0, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// A *url.Error names the method and URL, never a header.
		return 0, 0, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return resp.StatusCode, 0, fmt.Errorf("%s %s: reading the answer: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(raw, &e)
		var after time.Duration
		if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
			after = time.Duration(s) * time.Second
		}
		return resp.StatusCode, after, &APIError{Status: resp.StatusCode, Message: e.Message}
	}
	if out != nil && len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, 0, fmt.Errorf("%s %s: the answer is not the JSON expected: %w", method, path, err)
		}
	}
	return resp.StatusCode, 0, nil
}

// data is the envelope of every answer.
type data[T any] struct {
	Data     T      `json:"data"`
	NextPage string `json:"next_page,omitempty"`
}

type idOnly struct {
	ID string `json:"id"`
}

// --- billable metrics ---

func (c *Client) CreateBillableMetric(ctx context.Context, in BillableMetric) (string, error) {
	var out data[idOnly]
	err := c.do(ctx, http.MethodPost, "/v1/billable-metrics/create", in, &out)
	return out.Data.ID, err
}

// GetBillableMetric returns a metric, archived or not.
func (c *Client) GetBillableMetric(ctx context.Context, id string) (*BillableMetric, error) {
	var out data[BillableMetric]
	err := c.do(ctx, http.MethodGet, "/v1/billable-metrics/"+url.PathEscape(id), nil, &out)
	return &out.Data, err
}

// RenameBillableMetric changes the one thing about a metric that can change.
func (c *Client) RenameBillableMetric(ctx context.Context, id, name string) error {
	return c.do(ctx, http.MethodPut, "/v1/billable-metrics/"+url.PathEscape(id), map[string]string{"name": name}, nil)
}

func (c *Client) ArchiveBillableMetric(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/v1/billable-metrics/archive", idOnly{ID: id}, nil)
}

// --- products ---

func (c *Client) CreateProduct(ctx context.Context, in ProductInput) (string, error) {
	var out data[idOnly]
	err := c.do(ctx, http.MethodPost, "/v1/contract-pricing/products/create", in, &out)
	return out.Data.ID, err
}

// GetProduct returns a product, archived or not.
func (c *Client) GetProduct(ctx context.Context, id string) (*Product, error) {
	var out data[Product]
	err := c.do(ctx, http.MethodPost, "/v1/contract-pricing/products/get", idOnly{ID: id}, &out)
	return &out.Data, err
}

func (c *Client) UpdateProduct(ctx context.Context, in ProductUpdate) error {
	return c.do(ctx, http.MethodPost, "/v1/contract-pricing/products/update", in, nil)
}

// ArchiveProduct cannot be undone.
func (c *Client) ArchiveProduct(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/v1/contract-pricing/products/archive", map[string]string{"product_id": id}, nil)
}

// --- rate cards ---

func (c *Client) CreateRateCard(ctx context.Context, in RateCardInput) (string, error) {
	var out data[idOnly]
	err := c.do(ctx, http.MethodPost, "/v1/contract-pricing/rate-cards/create", in, &out)
	return out.Data.ID, err
}

func (c *Client) GetRateCard(ctx context.Context, id string) (*RateCard, error) {
	var out data[RateCard]
	err := c.do(ctx, http.MethodPost, "/v1/contract-pricing/rate-cards/get", idOnly{ID: id}, &out)
	return &out.Data, err
}

func (c *Client) UpdateRateCard(ctx context.Context, in RateCardUpdate) error {
	if in.Aliases == nil {
		in.Aliases = []Alias{}
	}
	return c.do(ctx, http.MethodPost, "/v1/contract-pricing/rate-cards/update", in, nil)
}

// ArchiveRateCard cannot be undone.
func (c *Client) ArchiveRateCard(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/v1/contract-pricing/rate-cards/archive", idOnly{ID: id}, nil)
}

// --- rates ---

func (c *Client) AddRate(ctx context.Context, in RateInput) error {
	return c.do(ctx, http.MethodPost, "/v1/contract-pricing/rate-cards/addRate", in, nil)
}

// RatesAt returns the rates of a product that are in force at a moment (RFC
// 3339): one for each set of pricing group values.
func (c *Client) RatesAt(ctx context.Context, rateCardID, productID, at string) ([]RateScheduleEntry, error) {
	in := map[string]any{
		"rate_card_id": rateCardID,
		"at":           at,
		"selectors":    []map[string]string{{"product_id": productID}},
	}
	var all []RateScheduleEntry
	for page := ""; ; {
		path := "/v1/contract-pricing/rate-cards/getRates?limit=100"
		if page != "" {
			path += "&next_page=" + url.QueryEscape(page)
		}
		var out data[[]RateScheduleEntry]
		if err := c.do(ctx, http.MethodPost, path, in, &out); err != nil {
			return nil, err
		}
		all = append(all, out.Data...)
		if out.NextPage == "" {
			return all, nil
		}
		page = out.NextPage
	}
}

// --- threshold notifications ---

func (c *Client) CreateAlert(ctx context.Context, in AlertInput) (string, error) {
	var out data[idOnly]
	err := c.do(ctx, http.MethodPost, "/v1/alerts/create", in, &out)
	return out.Data.ID, err
}

// ArchiveAlert cannot be undone. releaseKey lets the uniqueness key be used
// again.
func (c *Client) ArchiveAlert(ctx context.Context, id string, releaseKey bool) error {
	return c.do(ctx, http.MethodPost, "/v1/alerts/archive", map[string]any{"id": id, "release_uniqueness_key": releaseKey}, nil)
}

// CustomerAlert reads a threshold notification as it stands for one
// customer: the only way the API offers to read one.
func (c *Client) CustomerAlert(ctx context.Context, customerID, alertID string) (*Alert, error) {
	var out data[struct {
		Alert Alert `json:"alert"`
	}]
	err := c.do(ctx, http.MethodPost, "/v1/customer-alerts/get", map[string]string{"customer_id": customerID, "alert_id": alertID}, &out)
	return &out.Data.Alert, err
}

// --- custom field keys ---

func (c *Client) AddCustomFieldKey(ctx context.Context, k CustomFieldKey) error {
	return c.do(ctx, http.MethodPost, "/v1/customFields/addKey", k, nil)
}

// RemoveCustomFieldKey also makes every value stored under the key
// unreadable.
func (c *Client) RemoveCustomFieldKey(ctx context.Context, entity, key string) error {
	return c.do(ctx, http.MethodPost, "/v1/customFields/removeKey", map[string]string{"entity": entity, "key": key}, nil)
}

// CustomFieldKeys lists the keys of one kind of object.
func (c *Client) CustomFieldKeys(ctx context.Context, entity string) ([]CustomFieldKey, error) {
	in := map[string][]string{"entities": {entity}}
	var all []CustomFieldKey
	for page := ""; ; {
		path := "/v1/customFields/listKeys"
		if page != "" {
			path += "?next_page=" + url.QueryEscape(page)
		}
		var out data[[]CustomFieldKey]
		if err := c.do(ctx, http.MethodPost, path, in, &out); err != nil {
			return nil, err
		}
		all = append(all, out.Data...)
		if out.NextPage == "" {
			return all, nil
		}
		page = out.NextPage
	}
}

// --- pricing units ---

func (c *Client) ListCreditTypes(ctx context.Context) ([]CreditType, error) {
	var all []CreditType
	for page := ""; ; {
		path := "/v1/credit-types/list?limit=100"
		if page != "" {
			path += "&next_page=" + url.QueryEscape(page)
		}
		var out data[[]CreditType]
		if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
			return nil, err
		}
		all = append(all, out.Data...)
		if out.NextPage == "" {
			return all, nil
		}
		page = out.NextPage
	}
}
