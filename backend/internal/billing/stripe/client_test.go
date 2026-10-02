package stripe_test

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/r33drichards/browserjs-sessions/backend/internal/billing"
	"github.com/r33drichards/browserjs-sessions/backend/internal/billing/stripe"
)

// A made-up key: it opens nothing. The requests go to a server in this
// process.
const fakeKey = "rk_test_madeUpForTests"

type request struct {
	method, path string
	form         url.Values
	header       http.Header
}

// fakeAPI stands where api.stripe.com would: it records each request and
// answers what the test says for "METHOD /path".
type fakeAPI struct {
	t       *testing.T
	mu      sync.Mutex
	answers map[string]func(form url.Values) (int, string)
	seen    []request
	url     string
}

func newFakeAPI(t *testing.T) *fakeAPI {
	f := &fakeAPI{t: t, answers: map[string]func(url.Values) (int, string){}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(raw))
		if r.Method == http.MethodGet {
			form = r.URL.Query()
		}
		f.mu.Lock()
		f.seen = append(f.seen, request{r.Method, r.URL.Path, form, r.Header.Clone()})
		answer := f.answers[r.Method+" "+r.URL.Path]
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if answer == nil {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","code":"resource_missing","message":"No such object"}}`))
			return
		}
		status, body := answer(form)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	f.url = srv.URL
	return f
}

func (f *fakeAPI) on(route string, status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers[route] = func(url.Values) (int, string) { return status, body }
}

// last is the last request, which must be route.
func (f *fakeAPI) last(route string) request {
	f.t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.seen) == 0 {
		f.t.Fatalf("no request; want %s", route)
	}
	r := f.seen[len(f.seen)-1]
	if got := r.method + " " + r.path; got != route {
		f.t.Fatalf("the last request was %s, want %s", got, route)
	}
	if got := r.header.Get("Authorization"); got != "Bearer "+fakeKey {
		f.t.Errorf("%s: not made with the key", route)
	}
	return r
}

// wantForm checks the fields of a request, and that it has no others.
func wantForm(t *testing.T, r request, want map[string]string) {
	t.Helper()
	for k, v := range want {
		if got := r.form.Get(k); got != v {
			t.Errorf("%s %s: %s = %q, want %q", r.method, r.path, k, got, v)
		}
	}
	for k := range r.form {
		if _, ok := want[k]; !ok {
			t.Errorf("%s %s: unexpected field %s = %q", r.method, r.path, k, r.form.Get(k))
		}
	}
}

func TestAPICreateCheckout(t *testing.T) {
	f := newFakeAPI(t)
	api := stripe.NewAPI(fakeKey, "USD", f.url)
	f.on("POST /v1/checkout/sessions", 200, `{"id":"cs_test_1","object":"checkout.session","url":"https://checkout.stripe.com/c/pay/cs_test_1",
		"mode":"setup","status":"open","payment_status":"no_payment_required","customer":"cus_1","client_reference_id":"acct-1",
		"metadata":{"account":"acct-1","kind":"setup"},"created":1790942400}`)
	common := map[string]string{
		"customer": "cus_1", "client_reference_id": "acct-1", "metadata[account]": "acct-1",
		"success_url": "https://app.example.test/billing?checkout={CHECKOUT_SESSION_ID}", "cancel_url": "https://app.example.test/billing",
	}
	with := func(more map[string]string) map[string]string {
		out := map[string]string{}
		for _, m := range []map[string]string{common, more} {
			for k, v := range m {
				out[k] = v
			}
		}
		return out
	}
	p := billing.CheckoutParams{
		Customer: "cus_1", Account: "acct-1",
		SuccessURL: "https://app.example.test/billing?checkout={CHECKOUT_SESSION_ID}", CancelURL: "https://app.example.test/billing",
	}

	p.Mode, p.Kind = billing.ModeSetup, billing.KindSetup
	cs, err := api.CreateCheckout(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	equal(t, "the session", cs, billing.CheckoutSession{
		ID: "cs_test_1", URL: "https://checkout.stripe.com/c/pay/cs_test_1", Mode: "setup", Status: "open",
		PaymentStatus: "no_payment_required", Customer: "cus_1", ClientReferenceID: "acct-1",
		Metadata: map[string]string{"account": "acct-1", "kind": "setup"}, Created: time.Unix(1790942400, 0).UTC(),
	})
	wantForm(t, f.last("POST /v1/checkout/sessions"), with(map[string]string{
		"mode": "setup", "currency": "usd", "metadata[kind]": "setup",
		"setup_intent_data[metadata][account]": "acct-1", "setup_intent_data[metadata][kind]": "setup",
	}))

	p.Mode, p.Kind, p.Item, p.Price = billing.ModeSubscription, billing.KindPlan, starter, "price_1"
	if _, err := api.CreateCheckout(ctx, p); err != nil {
		t.Fatal(err)
	}
	wantForm(t, f.last("POST /v1/checkout/sessions"), with(map[string]string{
		"mode": "subscription", "metadata[kind]": "plan", "metadata[item]": starter,
		"line_items[0][price]": "price_1", "line_items[0][quantity]": "1",
		"subscription_data[metadata][account]": "acct-1", "subscription_data[metadata][kind]": "plan", "subscription_data[metadata][item]": starter,
	}))

	p.Mode, p.Kind, p.Item, p.Price = billing.ModePayment, billing.KindPurchase, pack20, "price_2"
	if _, err := api.CreateCheckout(ctx, p); err != nil {
		t.Fatal(err)
	}
	wantForm(t, f.last("POST /v1/checkout/sessions"), with(map[string]string{
		"mode": "payment", "metadata[kind]": "purchase", "metadata[item]": pack20,
		"line_items[0][price]": "price_2", "line_items[0][quantity]": "1",
		"payment_intent_data[setup_future_usage]": "off_session",
		"payment_intent_data[metadata][account]":  "acct-1", "payment_intent_data[metadata][kind]": "purchase", "payment_intent_data[metadata][item]": pack20,
	}))
}

func TestAPICreateCustomerAndPortal(t *testing.T) {
	f := newFakeAPI(t)
	api := stripe.NewAPI(fakeKey, "USD", f.url)
	f.on("POST /v1/customers", 200, `{"id":"cus_1","object":"customer"}`)
	id, err := api.CreateCustomer(ctx, billing.CustomerParams{Email: alice, Account: "acct-1", OwnerHash: "abc"})
	if err != nil || id != "cus_1" {
		t.Fatalf("id %q, err %v", id, err)
	}
	r := f.last("POST /v1/customers")
	wantForm(t, r, map[string]string{"email": alice, "metadata[account]": "acct-1", "metadata[owner_hash]": "abc"})
	if got := r.header.Get("Idempotency-Key"); got != "customer-abc" {
		t.Errorf("Idempotency-Key = %q", got)
	}

	f.on("POST /v1/billing_portal/sessions", 200, `{"id":"bps_1","object":"billing_portal.session","url":"https://billing.stripe.com/p/session/1"}`)
	link, err := api.CreatePortal(ctx, "cus_1", "https://app.example.test/billing")
	if err != nil || link != "https://billing.stripe.com/p/session/1" {
		t.Fatalf("url %q, err %v", link, err)
	}
	wantForm(t, f.last("POST /v1/billing_portal/sessions"), map[string]string{"customer": "cus_1", "return_url": "https://app.example.test/billing"})
}

func TestAPICreateRecharge(t *testing.T) {
	f := newFakeAPI(t)
	api := stripe.NewAPI(fakeKey, "USD", f.url)
	p := billing.RechargeParams{
		Customer: "cus_1", PaymentMethod: "pm_1", AmountCents: 2000, Currency: "usd", Account: "acct-1",
		Item: pack20, IdempotencyKey: "recharge-abc-2026-10-3",
	}
	f.on("POST /v1/payment_intents", 200, `{"id":"pi_1","object":"payment_intent","status":"succeeded","amount":2000,"customer":"cus_1",
		"metadata":{"account":"acct-1","kind":"recharge","item":"cu_credit_20_v1","month":"2026-10","seq":"3"},"created":1790942400}`)
	pi, err := api.CreateRecharge(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	equal(t, "the PaymentIntent", []any{pi.ID, pi.Status, pi.AmountCents, pi.Customer, pi.Metadata["seq"], pi.DeclineCode},
		[]any{"pi_1", "succeeded", 2000, "cus_1", "3", ""})
	r := f.last("POST /v1/payment_intents")
	wantForm(t, r, map[string]string{
		"amount": "2000", "currency": "usd", "customer": "cus_1", "payment_method": "pm_1", "off_session": "true", "confirm": "true",
		"metadata[account]": "acct-1", "metadata[kind]": "recharge", "metadata[item]": pack20, "metadata[month]": "2026-10", "metadata[seq]": "3",
	})
	if got := r.header.Get("Idempotency-Key"); got != "recharge-abc-2026-10-3" {
		t.Errorf("Idempotency-Key = %q", got)
	}

	// A card that wants its owner: Stripe answers 402 with the
	// PaymentIntent in the error. That is a failed charge, not an error.
	for code, body := range map[string]string{
		"authentication_required": `{"error":{"type":"card_error","code":"authentication_required","decline_code":"authentication_required","message":"needs authentication",
			"payment_intent":{"id":"pi_2","object":"payment_intent","status":"requires_payment_method","amount":2000,"customer":"cus_1","metadata":{"kind":"recharge"},
			"last_payment_error":{"type":"card_error","code":"authentication_required","decline_code":"authentication_required"}}}}`,
		"insufficient_funds": `{"error":{"type":"card_error","code":"card_declined","decline_code":"insufficient_funds","message":"declined",
			"payment_intent":{"id":"pi_2","object":"payment_intent","status":"requires_payment_method","amount":2000,"customer":"cus_1","metadata":{"kind":"recharge"}}}}`,
	} {
		f.on("POST /v1/payment_intents", 402, body)
		pi, err = api.CreateRecharge(ctx, p)
		if err != nil {
			t.Fatalf("%s: %v", code, err)
		}
		equal(t, code, []any{pi.ID, pi.Status, pi.DeclineCode}, []any{"pi_2", "requires_payment_method", code})
	}

	// Anything else is an error, and says nothing of the key.
	f.on("POST /v1/payment_intents", 401, `{"error":{"type":"invalid_request_error","message":"Invalid API Key provided: rk_test_***********ests"}}`)
	if _, err = api.CreateRecharge(ctx, p); err == nil || strings.Contains(err.Error(), fakeKey) {
		t.Errorf("err = %v", err)
	}
}

func TestAPIPaymentMethods(t *testing.T) {
	f := newFakeAPI(t)
	api := stripe.NewAPI(fakeKey, "USD", f.url)
	f.on("GET /v1/customers/cus_1", 200, `{"id":"cus_1","object":"customer","invoice_settings":{"default_payment_method":"pm_2"}}`)
	f.mu.Lock()
	// Two pages.
	f.answers["GET /v1/customers/cus_1/payment_methods"] = func(form url.Values) (int, string) {
		if form.Get("starting_after") == "" {
			return 200, `{"object":"list","has_more":true,"data":[{"id":"pm_2","object":"payment_method","type":"card","created":200,
				"card":{"fingerprint":"fp2","funding":"credit","brand":"visa","last4":"4242","exp_month":4,"exp_year":2030,"wallet":{"type":"apple_pay"}}}]}`
		}
		if form.Get("starting_after") != "pm_2" {
			return 400, `{"error":{"message":"bad cursor"}}`
		}
		return 200, `{"object":"list","has_more":false,"data":[
			{"id":"pm_1","object":"payment_method","type":"card","created":100,"card":{"fingerprint":"fp1","funding":"prepaid","brand":"mastercard","last4":"4444","exp_month":1,"exp_year":2029,"wallet":null}},
			{"id":"pm_0","object":"payment_method","type":"us_bank_account","created":50}]}`
	}
	f.mu.Unlock()
	methods, def, err := api.PaymentMethods(ctx, "cus_1")
	if err != nil {
		t.Fatal(err)
	}
	equal(t, "methods", []any{def, methods}, []any{"pm_2", []billing.PaymentMethod{
		{ID: "pm_2", Customer: "cus_1", Type: "card", Created: time.Unix(200, 0).UTC(),
			Fingerprint: "fp2", Funding: "credit", Wallet: "apple_pay", Brand: "visa", Last4: "4242", ExpMonth: 4, ExpYear: 2030},
		{ID: "pm_1", Customer: "cus_1", Type: "card", Created: time.Unix(100, 0).UTC(),
			Fingerprint: "fp1", Funding: "prepaid", Brand: "mastercard", Last4: "4444", ExpMonth: 1, ExpYear: 2029},
		{ID: "pm_0", Customer: "cus_1", Type: "us_bank_account", Created: time.Unix(50, 0).UTC()},
	}})

	// A customer Stripe does not have, or has deleted.
	if _, _, err := api.PaymentMethods(ctx, "cus_none"); !errors.Is(err, billing.ErrNotFound) {
		t.Errorf("no such customer: err = %v, want ErrNotFound", err)
	}
	f.on("GET /v1/customers/cus_gone", 200, `{"id":"cus_gone","object":"customer","deleted":true}`)
	if _, _, err := api.PaymentMethods(ctx, "cus_gone"); !errors.Is(err, billing.ErrNotFound) {
		t.Errorf("a deleted customer: err = %v, want ErrNotFound", err)
	}
}

func TestAPIReads(t *testing.T) {
	f := newFakeAPI(t)
	api := stripe.NewAPI(fakeKey, "USD", f.url)

	f.on("GET /v1/subscriptions/sub_1", 200, `{"id":"sub_1","object":"subscription","status":"active","customer":"cus_1","created":1000,
		"metadata":{"account":"acct-1"},"cancel_at":null,"cancel_at_period_end":true,
		"items":{"object":"list","data":[{"id":"si_1","current_period_start":2000,"current_period_end":3000,"price":{"id":"price_1","lookup_key":"cu_starter_monthly_v1"}}]},
		"latest_invoice":{"id":"in_1","object":"invoice","status":"paid"}}`)
	sub, err := api.Subscription(ctx, "sub_1")
	if err != nil {
		t.Fatal(err)
	}
	end := time.Unix(3000, 0).UTC()
	equal(t, "the subscription", sub, billing.Subscription{
		ID: "sub_1", Customer: "cus_1", Status: "active", Metadata: map[string]string{"account": "acct-1"}, PriceLookupKey: starter,
		CurrentPeriodStart: time.Unix(2000, 0).UTC(), CurrentPeriodEnd: end, CancelAt: &end, Created: time.Unix(1000, 0).UTC(),
		LatestInvoice: "in_1", LatestInvoiceStatus: "paid",
	})
	if got := f.last("GET /v1/subscriptions/sub_1").form["expand[0]"]; len(got) != 1 || got[0] != "latest_invoice" {
		t.Errorf("expand = %v, want latest_invoice", got)
	}
	// An invoice that is not paid.
	f.on("GET /v1/subscriptions/sub_1", 200, `{"id":"sub_1","object":"subscription","status":"past_due","customer":"cus_1",
		"items":{"data":[{"current_period_start":2000,"current_period_end":3000,"price":{"lookup_key":"cu_starter_monthly_v1"}}]},
		"latest_invoice":{"id":"in_2","object":"invoice","status":"open"}}`)
	if sub, _ = api.Subscription(ctx, "sub_1"); sub.LatestInvoiceStatus != "open" || sub.Status != "past_due" || sub.CancelAt != nil {
		t.Errorf("subscription = %+v", sub)
	}

	for name, call := range map[string]func() error{
		"subscription":   func() error { _, err := api.Subscription(ctx, "sub_none"); return err },
		"checkout":       func() error { _, err := api.Checkout(ctx, "cs_none"); return err },
		"payment intent": func() error { _, err := api.PaymentIntent(ctx, "pi_none"); return err },
		"cancel":         func() error { return api.CancelSubscription(ctx, "sub_none") },
		"detach":         func() error { return api.DetachPaymentMethod(ctx, "pm_none") },
	} {
		if err := call(); !errors.Is(err, billing.ErrNotFound) {
			t.Errorf("%s: err = %v, want ErrNotFound", name, err)
		}
	}

	f.on("GET /v1/checkout/sessions/cs_1", 200, `{"id":"cs_1","object":"checkout.session","mode":"payment","status":"complete","payment_status":"paid",
		"customer":"cus_1","client_reference_id":"acct-1","metadata":{"account":"acct-1","kind":"purchase","item":"cu_credit_20_v1"},
		"payment_intent":"pi_1","subscription":null,"created":5}`)
	cs, err := api.Checkout(ctx, "cs_1")
	if err != nil || cs.PaymentIntent != "pi_1" || cs.Subscription != "" || cs.PaymentStatus != "paid" || cs.Customer != "cus_1" {
		t.Errorf("checkout = %+v, err %v", cs, err)
	}

	f.on("GET /v1/prices", 200, `{"object":"list","has_more":false,"data":[{"id":"price_1","object":"price","lookup_key":"cu_starter_monthly_v1"}]}`)
	prices, err := api.Prices(ctx, []string{starter, pro})
	equal(t, "prices", []any{prices, err}, []any{map[string]string{starter: "price_1"}, nil})
	r := f.last("GET /v1/prices")
	equal(t, "the lookup keys asked for", []any{r.form["lookup_keys[0]"], r.form["lookup_keys[1]"], r.form.Get("active")},
		[]any{[]string{starter}, []string{pro}, "true"})

	f.on("GET /v1/checkout/sessions", 200, `{"object":"list","has_more":false,"data":[{"id":"cs_1","object":"checkout.session","mode":"setup","status":"open","customer":"cus_1","created":7}]}`)
	since := time.Unix(1790000000, 0)
	list, err := api.Checkouts(ctx, "cus_1", since)
	if err != nil || len(list) != 1 || list[0].Mode != "setup" {
		t.Errorf("checkouts = %+v, err %v", list, err)
	}
	r = f.last("GET /v1/checkout/sessions")
	equal(t, "the list's filter", []any{r.form.Get("customer"), r.form.Get("created[gte]")}, []any{"cus_1", "1790000000"})

	// The subscription period a payment paid for: an upgrade's invoice has
	// a line for the old period's unused time, and one for the new period.
	f.on("GET /v1/invoice_payments", 200, `{"object":"list","has_more":false,"data":[{"id":"inpay_1","object":"invoice_payment",
		"invoice":{"id":"in_1","object":"invoice","parent":{"type":"subscription_details","subscription_details":{"subscription":"sub_1"}},
		"lines":{"object":"list","data":[{"id":"il_1","period":{"start":2000,"end":3000}},{"id":"il_2","period":{"start":2500,"end":3500}}]}}}]}`)
	period, ok, err := api.SubscriptionPeriodOf(ctx, "pi_1")
	equal(t, "the period", []any{period, ok, err}, []any{stripe.PaidPeriod{Subscription: "sub_1", Invoice: "in_1", Start: time.Unix(2500, 0).UTC()}, true, nil})
	r = f.last("GET /v1/invoice_payments")
	equal(t, "the filter", []any{r.form.Get("payment[type]"), r.form.Get("payment[payment_intent]"), r.form.Get("expand[0]")},
		[]any{"payment_intent", "pi_1", "data.invoice"})
	f.on("GET /v1/invoice_payments", 200, `{"object":"list","has_more":false,"data":[]}`)
	if _, ok, err := api.SubscriptionPeriodOf(ctx, "pi_2"); ok || err != nil {
		t.Errorf("a payment that paid no invoice: ok %v, err %v", ok, err)
	}
}

func TestKeyMode(t *testing.T) {
	for key, want := range map[string]string{
		"sk_test_x": "test", "rk_test_x": "test", "sk_live_x": "live", "rk_live_x": "live",
		"pk_test_x": "", "whsec_x": "", "": "", "rk_prod_x": "",
	} {
		got, err := stripe.KeyMode(key)
		if got != want || (err == nil) != (want != "") {
			t.Errorf("KeyMode(%q) = %q, %v; want %q", key, got, err, want)
		}
		if err != nil && key != "" && strings.Contains(err.Error(), key) {
			t.Errorf("the error says the key: %v", err)
		}
	}
}
