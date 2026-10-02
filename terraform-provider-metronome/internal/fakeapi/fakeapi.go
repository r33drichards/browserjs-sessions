// Package fakeapi is an in-memory stand-in for the part of Metronome's API
// the provider uses, written from https://docs.metronome.com/openapi.json
// (read 2026-10-02). It keeps Metronome's rules as documented there: a
// billable metric's definition cannot change, archived objects stay readable
// and cannot be used for anything new, rates are only ever added. Where the
// documentation does not say what the real API does, the fake's choice is
// marked ASSUMED.
package fakeapi

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/r33drichards/computer-use/terraform-provider-metronome/internal/client"
)

// Request is one request the fake received.
type Request struct {
	Method, Path, UserAgent, Body string
	Authorized                    bool
}

// Server is the fake. Its exported fields are settings; change them through
// Configure once it is serving.
type Server struct {
	mu sync.Mutex

	// Token is the one API token accepted.
	Token string
	// Now is the fake's clock.
	Now func() time.Time
	// RateLimited is how many of the next requests answer 429.
	RateLimited int
	// Milliseconds makes answers write timestamps with milliseconds, as the
	// real API's examples do, whatever form the request used.
	Milliseconds bool

	metrics   map[string]*client.BillableMetric
	products  map[string]*client.Product
	rateCards map[string]*rateCard
	alerts    map[string]*alert
	keys      map[string]string // uniqueness key -> alert ID
	fields    []client.CustomFieldKey
	requests  []Request
}

type rateCard struct {
	client.RateCard
	archived bool
	rates    []client.RateScheduleEntry
}

type alert struct {
	client.Alert
	customerID string
}

// New makes a fake that accepts token.
func New(token string) *Server {
	return &Server{
		Token:     token,
		Now:       time.Now,
		metrics:   map[string]*client.BillableMetric{},
		products:  map[string]*client.Product{},
		rateCards: map[string]*rateCard{},
		alerts:    map[string]*alert{},
		keys:      map[string]string{},
	}
}

// Configure changes settings while the fake is serving.
func (s *Server) Configure(f func(*Server)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(s)
}

// Requests is every request so far.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

// Count is how many requests had this method and path.
func (s *Server) Count(method, path string) int {
	n := 0
	for _, r := range s.Requests() {
		if r.Method == method && r.Path == path {
			n++
		}
	}
	return n
}

// Last is the body of the last request with this method and path.
func (s *Server) Last(method, path string) string {
	reqs := s.Requests()
	for i := len(reqs) - 1; i >= 0; i-- {
		if reqs[i].Method == method && reqs[i].Path == path {
			return reqs[i].Body
		}
	}
	return ""
}

// Metric, Product, RateCard and Alert look at what the fake holds.
func (s *Server) Metric(id string) (client.BillableMetric, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.metrics[id]
	if !ok {
		return client.BillableMetric{}, false
	}
	return *m, true
}

func (s *Server) Product(id string) (client.Product, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.products[id]
	if !ok {
		return client.Product{}, false
	}
	return *p, true
}

// RateCardArchived reports whether a rate card exists and is archived.
func (s *Server) RateCardArchived(id string) (archived, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rc, ok := s.rateCards[id]
	return ok && rc.archived, ok
}

// Rates is every rate ever added to a rate card, in the order added.
func (s *Server) Rates(rateCardID string) []client.RateScheduleEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	rc, ok := s.rateCards[rateCardID]
	if !ok {
		return nil
	}
	return append([]client.RateScheduleEntry(nil), rc.rates...)
}

// AlertStatus is `enabled`, `archived`, or "" when there is no such alert.
func (s *Server) AlertStatus(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.alerts[id]
	if !ok {
		return ""
	}
	return a.Status
}

// ArchiveOutOfBand archives a metric or a product behind the provider's
// back, as someone would in Metronome's app.
func (s *Server) ArchiveOutOfBand(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m, ok := s.metrics[id]; ok {
		m.ArchivedAt = s.stamp(s.Now())
	}
	if p, ok := s.products[id]; ok {
		p.ArchivedAt = s.stamp(s.Now())
	}
}

type bodyKey struct{}

// Handler serves the API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/billable-metrics/create", s.createMetric)
	mux.HandleFunc("GET /v1/billable-metrics/{id}", s.getMetric)
	mux.HandleFunc("PUT /v1/billable-metrics/{id}", s.renameMetric)
	mux.HandleFunc("POST /v1/billable-metrics/archive", s.archiveMetric)
	mux.HandleFunc("POST /v1/contract-pricing/products/create", s.createProduct)
	mux.HandleFunc("POST /v1/contract-pricing/products/get", s.getProduct)
	mux.HandleFunc("POST /v1/contract-pricing/products/update", s.updateProduct)
	mux.HandleFunc("POST /v1/contract-pricing/products/archive", s.archiveProduct)
	mux.HandleFunc("POST /v1/contract-pricing/rate-cards/create", s.createRateCard)
	mux.HandleFunc("POST /v1/contract-pricing/rate-cards/get", s.getRateCard)
	mux.HandleFunc("POST /v1/contract-pricing/rate-cards/update", s.updateRateCard)
	mux.HandleFunc("POST /v1/contract-pricing/rate-cards/archive", s.archiveRateCard)
	mux.HandleFunc("POST /v1/contract-pricing/rate-cards/addRate", s.addRate)
	mux.HandleFunc("POST /v1/contract-pricing/rate-cards/getRates", s.getRates)
	mux.HandleFunc("POST /v1/alerts/create", s.createAlert)
	mux.HandleFunc("POST /v1/alerts/archive", s.archiveAlert)
	mux.HandleFunc("POST /v1/customer-alerts/get", s.customerAlert)
	mux.HandleFunc("GET /v1/credit-types/list", s.creditTypes)
	mux.HandleFunc("POST /v1/customFields/addKey", s.addFieldKey)
	mux.HandleFunc("POST /v1/customFields/removeKey", s.removeFieldKey)
	mux.HandleFunc("POST /v1/customFields/listKeys", s.listFieldKeys)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 4<<20))
		ok := r.Header.Get("Authorization") == "Bearer "+s.Token
		s.mu.Lock()
		s.requests = append(s.requests, Request{Method: r.Method, Path: r.URL.Path, UserAgent: r.UserAgent(), Body: string(body), Authorized: ok})
		limited := s.RateLimited > 0
		if limited {
			s.RateLimited--
		}
		s.mu.Unlock()
		if limited {
			fail(w, http.StatusTooManyRequests, "Too Many Requests")
			return
		}
		if !ok {
			fail(w, http.StatusUnauthorized, "Unauthorized")
			return
		}
		mux.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), bodyKey{}, body)))
	})
}

func fail(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"message": message})
}

func answer(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": v})
}

func answerID(w http.ResponseWriter, id string) { answer(w, map[string]string{"id": id}) }

// decode reads the request body into v, refusing fields v does not have, so
// that a field the provider sends and the API does not know fails a test.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	b, _ := r.Context().Value(bodyKey{}).([]byte)
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		fail(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return false
	}
	return true
}

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// stamp writes a time as the API does.
func (s *Server) stamp(t time.Time) string {
	if s.Milliseconds {
		return t.UTC().Format("2006-01-02T15:04:05.000Z")
	}
	return t.UTC().Format(time.RFC3339)
}

// restamp rewrites a client's timestamp the way the API echoes it. An empty
// one stays empty.
func (s *Server) restamp(v string) string {
	if v == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return v
	}
	return s.stamp(t)
}

// --- billable metrics ---

var aggregations = map[string]bool{"COUNT": true, "LATEST": true, "MAX": true, "SUM": true, "UNIQUE": true}

func (s *Server) createMetric(w http.ResponseWriter, r *http.Request) {
	var in client.BillableMetric
	if !decode(w, r, &in) {
		return
	}
	switch {
	case in.Name == "":
		fail(w, http.StatusBadRequest, "name is required")
		return
	case in.SQL != "" && (in.AggregationType != "" || in.EventTypeFilter != nil || len(in.PropertyFilters) > 0 || in.AggregationKey != "" || len(in.GroupKeys) > 0):
		fail(w, http.StatusBadRequest, "sql is mutually exclusive with aggregation_type, event_type_filter, property_filters, aggregation_key and group_keys")
		return
	case in.SQL == "" && !aggregations[strings.ToUpper(in.AggregationType)]:
		fail(w, http.StatusBadRequest, "aggregation_type must be one of COUNT, LATEST, MAX, SUM, UNIQUE")
		return
	}
	in.AggregationType = strings.ToUpper(in.AggregationType)
	if in.AggregationType != "" && in.AggregationType != "COUNT" {
		named := false
		for _, f := range in.PropertyFilters {
			named = named || f.Name == in.AggregationKey
		}
		if !named {
			fail(w, http.StatusBadRequest, "aggregation_key must be one of the property filter names")
			return
		}
	}
	if len(in.GroupKeys) > 5 {
		fail(w, http.StatusBadRequest, "at most 5 group keys per billable metric")
		return
	}
	in.ID, in.ArchivedAt = newID(), ""
	s.mu.Lock()
	s.metrics[in.ID] = &in
	s.mu.Unlock()
	answerID(w, in.ID)
}

func (s *Server) getMetric(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.metrics[r.PathValue("id")]
	if !ok {
		fail(w, http.StatusNotFound, "billable metric not found")
		return
	}
	answer(w, m)
}

func (s *Server) renameMetric(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &in) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.metrics[r.PathValue("id")]
	if !ok {
		fail(w, http.StatusNotFound, "billable metric not found")
		return
	}
	if in.Name == "" {
		fail(w, http.StatusBadRequest, "name is required")
		return
	}
	m.Name = in.Name
	answerID(w, m.ID)
}

func (s *Server) archiveMetric(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID string `json:"id"`
	}
	if !decode(w, r, &in) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.metrics[in.ID]
	if !ok {
		fail(w, http.StatusNotFound, "billable metric not found")
		return
	}
	if m.ArchivedAt == "" {
		m.ArchivedAt = s.stamp(s.Now())
	}
	answerID(w, m.ID)
}

// --- products ---

var productTypes = map[string]bool{"FIXED": true, "USAGE": true, "COMPOSITE": true, "SUBSCRIPTION": true, "PRO_SERVICE": true}

func (s *Server) createProduct(w http.ResponseWriter, r *http.Request) {
	var in client.ProductInput
	if !decode(w, r, &in) {
		return
	}
	in.Type = strings.ToUpper(in.Type)
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case in.Name == "":
		fail(w, http.StatusBadRequest, "name is required")
		return
	case !productTypes[in.Type]:
		fail(w, http.StatusBadRequest, "type is not a product type")
		return
	case in.Type == "USAGE" && in.BillableMetricID == "":
		fail(w, http.StatusBadRequest, "billable_metric_id is required for USAGE products")
		return
	case in.Type != "USAGE" && (in.BillableMetricID != "" || in.QuantityConversion != nil || in.QuantityRounding != nil || len(in.PricingGroupKey) > 0 || len(in.PresentationGroupKey) > 0):
		fail(w, http.StatusBadRequest, "billable_metric_id, quantity_conversion, quantity_rounding and the group keys are only valid for USAGE products")
		return
	}
	if msg := s.usableMetric(in.BillableMetricID); msg != "" {
		fail(w, http.StatusBadRequest, msg)
		return
	}
	p := &client.Product{ID: newID(), Type: in.Type, Current: client.ProductState{
		Name: in.Name, BillableMetricID: in.BillableMetricID, Tags: in.Tags,
		QuantityConversion: in.QuantityConversion, QuantityRounding: in.QuantityRounding,
		PricingGroupKey: in.PricingGroupKey, PresentationGroupKey: in.PresentationGroupKey,
	}}
	s.products[p.ID] = p
	answerID(w, p.ID)
}

// usableMetric is why a metric cannot be given to a product, or "".
func (s *Server) usableMetric(id string) string {
	if id == "" {
		return ""
	}
	m, ok := s.metrics[id]
	if !ok {
		return "billable metric not found"
	}
	if m.ArchivedAt != "" {
		return "the billable metric is archived"
	}
	return ""
}

func (s *Server) getProduct(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID string `json:"id"`
	}
	if !decode(w, r, &in) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.products[in.ID]
	if !ok {
		fail(w, http.StatusNotFound, "product not found")
		return
	}
	out := *p
	answer(w, struct {
		client.Product
		Initial client.ProductState `json:"initial"`
		Updates []any               `json:"updates"`
	}{out, out.Current, []any{}})
}

func (s *Server) updateProduct(w http.ResponseWriter, r *http.Request) {
	// Decoded as raw fields: for the two nullable ones, absent and null
	// differ.
	var in map[string]json.RawMessage
	if !decode(w, r, &in) {
		return
	}
	var typed client.ProductUpdate
	if !decode(w, r, &typed) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.products[typed.ProductID]
	if !ok {
		fail(w, http.StatusNotFound, "product not found")
		return
	}
	at, err := time.Parse(time.RFC3339, typed.StartingAt)
	switch {
	case err != nil:
		fail(w, http.StatusBadRequest, "starting_at is required")
		return
	case !at.Equal(at.Truncate(time.Hour)):
		fail(w, http.StatusBadRequest, "starting_at must be on an hour boundary")
		return
	case p.ArchivedAt != "":
		// ASSUMED: the documentation does not say what updating an
		// archived product answers.
		fail(w, http.StatusBadRequest, "the product is archived")
		return
	}
	usageOnly := typed.BillableMetricID != "" || typed.PricingGroupKey != nil || typed.PresentationGroupKey != nil
	for _, k := range []string{"quantity_conversion", "quantity_rounding"} {
		_, sent := in[k]
		usageOnly = usageOnly || sent
	}
	if usageOnly && p.Type != "USAGE" {
		fail(w, http.StatusBadRequest, "billable_metric_id, quantity_conversion, quantity_rounding and the group keys are only valid for USAGE products")
		return
	}
	if msg := s.usableMetric(typed.BillableMetricID); msg != "" {
		fail(w, http.StatusBadRequest, msg)
		return
	}
	cur := &p.Current
	if typed.Name != "" {
		cur.Name = typed.Name
	}
	if typed.BillableMetricID != "" {
		cur.BillableMetricID = typed.BillableMetricID
	}
	if typed.Tags != nil {
		cur.Tags = *typed.Tags
	}
	// A null decodes to a nil outer pointer, like an absent field: the raw
	// fields tell them apart.
	if raw, sent := in["quantity_conversion"]; sent {
		cur.QuantityConversion = nil
		if string(raw) != "null" {
			cur.QuantityConversion = *typed.QuantityConversion
		}
	}
	if raw, sent := in["quantity_rounding"]; sent {
		cur.QuantityRounding = nil
		if string(raw) != "null" {
			cur.QuantityRounding = *typed.QuantityRounding
		}
	}
	if typed.PricingGroupKey != nil {
		cur.PricingGroupKey = *typed.PricingGroupKey
	}
	if typed.PresentationGroupKey != nil {
		cur.PresentationGroupKey = *typed.PresentationGroupKey
	}
	answerID(w, p.ID)
}

func (s *Server) archiveProduct(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ProductID string `json:"product_id"`
	}
	if !decode(w, r, &in) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.products[in.ProductID]
	if !ok {
		fail(w, http.StatusNotFound, "product not found")
		return
	}
	if p.ArchivedAt == "" {
		p.ArchivedAt = s.stamp(s.Now())
	}
	answerID(w, p.ID)
}

// --- rate cards ---

func (s *Server) createRateCard(w http.ResponseWriter, r *http.Request) {
	var in client.RateCardInput
	if !decode(w, r, &in) {
		return
	}
	if in.Name == "" {
		fail(w, http.StatusBadRequest, "name is required")
		return
	}
	if in.FiatCreditTypeID == "" {
		in.FiatCreditTypeID = client.USDCreditTypeID
	}
	if in.FiatCreditTypeID != client.USDCreditTypeID {
		fail(w, http.StatusBadRequest, "fiat_credit_type_id is not a fiat pricing unit")
		return
	}
	rc := &rateCard{RateCard: client.RateCard{
		ID: newID(), Name: in.Name, Description: in.Description,
		FiatCreditType: &client.CreditType{ID: client.USDCreditTypeID, Name: "USD (cents)"},
		Aliases:        s.aliases(in.Aliases),
	}}
	s.mu.Lock()
	s.rateCards[rc.ID] = rc
	s.mu.Unlock()
	answerID(w, rc.ID)
}

func (s *Server) aliases(in []client.Alias) []client.Alias {
	out := make([]client.Alias, len(in))
	for i, a := range in {
		out[i] = client.Alias{Name: a.Name, StartingAt: s.restamp(a.StartingAt), EndingBefore: s.restamp(a.EndingBefore)}
	}
	return out
}

func (s *Server) getRateCard(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID string `json:"id"`
	}
	if !decode(w, r, &in) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rc, ok := s.rateCards[in.ID]
	if !ok {
		fail(w, http.StatusNotFound, "rate card not found")
		return
	}
	// As the real API: an archived rate card reads like any other.
	answer(w, rc.RateCard)
}

func (s *Server) updateRateCard(w http.ResponseWriter, r *http.Request) {
	var in client.RateCardUpdate
	if !decode(w, r, &in) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rc, ok := s.rateCards[in.RateCardID]
	if !ok {
		fail(w, http.StatusNotFound, "rate card not found")
		return
	}
	if rc.archived {
		// ASSUMED.
		fail(w, http.StatusBadRequest, "the rate card is archived")
		return
	}
	if in.Name != "" {
		rc.Name = in.Name
	}
	if in.Description != nil {
		rc.Description = *in.Description
	}
	rc.Aliases = s.aliases(in.Aliases)
	answerID(w, rc.ID)
}

func (s *Server) archiveRateCard(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID string `json:"id"`
	}
	if !decode(w, r, &in) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rc, ok := s.rateCards[in.ID]
	if !ok {
		fail(w, http.StatusNotFound, "rate card not found")
		return
	}
	rc.archived = true
	answerID(w, rc.ID)
}

// --- rates ---

var rateTypes = map[string]bool{"FLAT": true, "PERCENTAGE": true, "SUBSCRIPTION": true, "TIERED": true, "TIERED_PERCENTAGE": true, "CUSTOM": true}

func (s *Server) addRate(w http.ResponseWriter, r *http.Request) {
	var in client.RateInput
	if !decode(w, r, &in) {
		return
	}
	in.RateType = strings.ToUpper(in.RateType)
	s.mu.Lock()
	defer s.mu.Unlock()
	rc, ok := s.rateCards[in.RateCardID]
	if !ok {
		fail(w, http.StatusNotFound, "rate card not found")
		return
	}
	p, ok := s.products[in.ProductID]
	if !ok {
		fail(w, http.StatusNotFound, "product not found")
		return
	}
	at, err := time.Parse(time.RFC3339, in.StartingAt)
	switch {
	case rc.archived:
		fail(w, http.StatusBadRequest, "the rate card is archived")
		return
	case p.ArchivedAt != "":
		fail(w, http.StatusBadRequest, "the product is archived")
		return
	case err != nil:
		fail(w, http.StatusBadRequest, "starting_at is required")
		return
	case !at.Equal(at.Truncate(time.Hour)):
		// ASSUMED from the rule for product updates and contracts.
		fail(w, http.StatusBadRequest, "starting_at must be on an hour boundary")
		return
	case !rateTypes[in.RateType]:
		fail(w, http.StatusBadRequest, "rate_type is not a rate type")
		return
	case (in.RateType == "FLAT" || in.RateType == "SUBSCRIPTION" || in.RateType == "PERCENTAGE") && (in.Price == nil || *in.Price < 0):
		fail(w, http.StatusBadRequest, "price must be >= 0")
		return
	case in.RateType == "TIERED" && len(in.Tiers) == 0:
		fail(w, http.StatusBadRequest, "tiers are required for a TIERED rate")
		return
	case in.RateType == "SUBSCRIPTION" && in.BillingFrequency == "":
		fail(w, http.StatusBadRequest, "billing_frequency is required for a SUBSCRIPTION rate")
		return
	case in.CommitRate != nil && (strings.ToUpper(in.CommitRate.RateType) != "FLAT" || in.CommitRate.Price == nil || *in.CommitRate.Price < 0):
		// The fake knows FLAT commit rates only.
		fail(w, http.StatusBadRequest, "commit_rate must be FLAT with a price >= 0")
		return
	}
	if in.CreditTypeID == "" {
		in.CreditTypeID = client.USDCreditTypeID
	}
	if in.CreditTypeID != client.USDCreditTypeID {
		fail(w, http.StatusBadRequest, "the rate card has no conversion for that pricing unit")
		return
	}
	entry := client.RateScheduleEntry{
		ProductID: p.ID, ProductName: p.Current.Name, PricingGroupValues: in.PricingGroupValues,
		StartingAt: s.restamp(in.StartingAt), EndingBefore: s.restamp(in.EndingBefore), Entitled: in.Entitled,
		BillingFrequency: strings.ToUpper(in.BillingFrequency), CommitRate: in.CommitRate,
		Rate: client.Rate{
			RateType: in.RateType, Price: in.Price, Tiers: in.Tiers, Quantity: in.Quantity, IsProrated: in.IsProrated,
			CreditType: &client.CreditType{ID: client.USDCreditTypeID, Name: "USD (cents)"},
		},
	}
	rc.rates = append(rc.rates, entry)
	answer(w, entry.Rate)
}

func (s *Server) getRates(w http.ResponseWriter, r *http.Request) {
	var in struct {
		RateCardID string `json:"rate_card_id"`
		At         string `json:"at"`
		Selectors  []struct {
			ProductID string `json:"product_id"`
		} `json:"selectors"`
	}
	if !decode(w, r, &in) {
		return
	}
	at, err := time.Parse(time.RFC3339, in.At)
	if err != nil {
		fail(w, http.StatusBadRequest, "at is required")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rc, ok := s.rateCards[in.RateCardID]
	if !ok {
		fail(w, http.StatusNotFound, "rate card not found")
		return
	}
	// For each product and set of pricing group values, the rate in force
	// at `at`. ASSUMED: of two rates that both cover the moment, the one
	// added later wins; the documentation says only that a rate added later
	// with a later starting_at takes over from then.
	inForce := map[string]client.RateScheduleEntry{}
	for _, e := range rc.rates {
		wanted := len(in.Selectors) == 0
		for _, sel := range in.Selectors {
			wanted = wanted || sel.ProductID == e.ProductID
		}
		start, _ := time.Parse(time.RFC3339, e.StartingAt)
		if !wanted || start.After(at) {
			continue
		}
		if e.EndingBefore != "" {
			if end, _ := time.Parse(time.RFC3339, e.EndingBefore); !end.After(at) {
				continue
			}
		}
		groups, _ := json.Marshal(e.PricingGroupValues)
		key := e.ProductID + string(groups)
		if prev, ok := inForce[key]; ok {
			prevStart, _ := time.Parse(time.RFC3339, prev.StartingAt)
			if prevStart.After(start) {
				continue
			}
		}
		inForce[key] = e
	}
	keys := make([]string, 0, len(inForce))
	for k := range inForce {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := []client.RateScheduleEntry{}
	for _, k := range keys {
		out = append(out, inForce[k])
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": out, "next_page": nil})
}

// --- threshold notifications ---

var alertTypes = map[string]bool{
	"spend_threshold_reached": true, "monthly_invoice_total_spend_threshold_reached": true, "usage_threshold_reached": true,
	"low_remaining_days_for_commit_segment_reached": true, "low_remaining_commit_balance_reached": true,
	"low_remaining_commit_percentage_reached": true, "low_remaining_days_for_contract_credit_segment_reached": true,
	"low_remaining_contract_credit_balance_reached": true, "low_remaining_contract_credit_percentage_reached": true,
	"low_remaining_contract_credit_and_commit_balance_reached": true, "low_remaining_contract_credit_and_commit_percentage_reached": true,
	"invoice_total_reached": true, "low_remaining_seat_balance_reached": true,
}

func (s *Server) createAlert(w http.ResponseWriter, r *http.Request) {
	var in client.AlertInput
	if !decode(w, r, &in) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case in.Name == "":
		fail(w, http.StatusBadRequest, "name is required")
		return
	case !alertTypes[in.AlertType]:
		fail(w, http.StatusBadRequest, "alert_type is not a threshold notification type")
		return
	case in.AlertType == "usage_threshold_reached" && in.BillableMetricID == "":
		fail(w, http.StatusBadRequest, "billable_metric_id is required for usage_threshold_reached")
		return
	}
	if in.UniquenessKey != "" {
		if _, taken := s.keys[in.UniquenessKey]; taken {
			fail(w, http.StatusConflict, "a threshold notification with this uniqueness key already exists")
			return
		}
	}
	a := &alert{customerID: in.CustomerID, Alert: client.Alert{
		ID: newID(), Name: in.Name, Type: in.AlertType, Status: "enabled", Threshold: in.Threshold, UniquenessKey: in.UniquenessKey,
	}}
	if in.CreditTypeID != "" {
		a.CreditType = &client.CreditType{ID: in.CreditTypeID, Name: "USD (cents)"}
	}
	s.alerts[a.ID] = a
	if in.UniquenessKey != "" {
		s.keys[in.UniquenessKey] = a.ID
	}
	answerID(w, a.ID)
}

func (s *Server) archiveAlert(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID                   string `json:"id"`
		ReleaseUniquenessKey bool   `json:"release_uniqueness_key"`
	}
	if !decode(w, r, &in) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.alerts[in.ID]
	if !ok {
		fail(w, http.StatusNotFound, "threshold notification not found")
		return
	}
	a.Status = "archived"
	if in.ReleaseUniquenessKey && s.keys[a.UniquenessKey] == a.ID {
		delete(s.keys, a.UniquenessKey)
	}
	answerID(w, a.ID)
}

func (s *Server) customerAlert(w http.ResponseWriter, r *http.Request) {
	var in struct {
		CustomerID string `json:"customer_id"`
		AlertID    string `json:"alert_id"`
	}
	if !decode(w, r, &in) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.alerts[in.AlertID]
	if !ok || in.CustomerID == "" || (a.customerID != "" && a.customerID != in.CustomerID) {
		fail(w, http.StatusNotFound, "threshold notification not found")
		return
	}
	var status any = "ok"
	if a.Status == "archived" {
		status = nil
	}
	answer(w, map[string]any{"customer_status": status, "alert": a.Alert})
}

// --- pricing units ---

func (s *Server) creditTypes(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"data": []client.CreditType{
			{ID: client.USDCreditTypeID, Name: "USD (cents)", IsCurrency: true},
			{ID: "fa2f1b3d-9d52-4951-a099-25991fd394d6", Name: "cloud consumption units"},
		},
		"next_page": nil,
	})
}

// --- custom field keys ---

var fieldEntities = map[string]bool{
	"alert": true, "billable_metric": true, "charge": true, "commit": true, "contract_credit": true, "contract_product": true,
	"contract": true, "customer": true, "discount": true, "invoice": true, "professional_service": true, "product": true,
	"rate_card": true, "scheduled_charge": true, "subscription": true, "package_commit": true, "package_credit": true,
	"package_subscription": true, "package_scheduled_charge": true,
}

func (s *Server) addFieldKey(w http.ResponseWriter, r *http.Request) {
	var in client.CustomFieldKey
	if !decode(w, r, &in) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !fieldEntities[in.Entity] || in.Key == "" {
		fail(w, http.StatusBadRequest, "entity and key are required")
		return
	}
	for _, k := range s.fields {
		if k.Entity == in.Entity && k.Key == in.Key {
			// ASSUMED: the documentation does not say what adding a key
			// twice answers.
			fail(w, http.StatusBadRequest, "the custom field key already exists")
			return
		}
	}
	s.fields = append(s.fields, in)
	w.WriteHeader(http.StatusOK)
}

func (s *Server) removeFieldKey(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Entity string `json:"entity"`
		Key    string `json:"key"`
	}
	if !decode(w, r, &in) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, k := range s.fields {
		if k.Entity == in.Entity && k.Key == in.Key {
			s.fields = append(s.fields[:i], s.fields[i+1:]...)
			break
		}
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) listFieldKeys(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Entities []string `json:"entities"`
	}
	if !decode(w, r, &in) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []client.CustomFieldKey{}
	for _, k := range s.fields {
		wanted := len(in.Entities) == 0
		for _, e := range in.Entities {
			wanted = wanted || e == k.Entity
		}
		if wanted {
			out = append(out, k)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": out, "next_page": nil})
}

// FieldKeys is the custom field keys the fake holds.
func (s *Server) FieldKeys() []client.CustomFieldKey {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]client.CustomFieldKey(nil), s.fields...)
}
