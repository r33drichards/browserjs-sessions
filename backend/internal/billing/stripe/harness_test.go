package stripe_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/r33drichards/browserjs-sessions/backend/internal/auth"
	"github.com/r33drichards/browserjs-sessions/backend/internal/billing"
	"github.com/r33drichards/browserjs-sessions/backend/internal/billing/billingtest"
	"github.com/r33drichards/browserjs-sessions/backend/internal/billing/stripe"
	"github.com/r33drichards/browserjs-sessions/backend/internal/billing/stripe/stripetest"
)

// A made-up secret: no test has, or needs, a real one.
const whsec = "whsec_test"

const (
	publicURL = "https://app.example.test"
	alice     = "u@example.com"
	bob       = "v@example.com"

	starter = "cu_starter_monthly_v1"
	pro     = "cu_pro_monthly_v1"
	pack20  = "cu_credit_20_v1"
)

var cardA = stripetest.Card{Fingerprint: "fpA", Funding: "credit", Brand: "visa", Last4: "4242"}

// world is the Service on the fakes, with its routes behind a stand-in for
// the sign-in.
type world struct {
	t        *testing.T
	clock    *billingtest.Clock
	accounts *billingtest.Accounts
	// accountsErr, when set, is what every call to the account store
	// answers.
	accountsErr error
	// metronome is where credit is kept; ledger is the real one over it,
	// and remembers what it was asked.
	metronome *billingtest.Metronome
	ledger    *ledger
	stripe    *stripetest.Stripe
	svc       *stripe.Service
	mux       *http.ServeMux
	webhook   http.Handler
	// balances is what the balance pass would have just read, by owner.
	balances map[string]int64
}

func contractCatalogue(t *testing.T) billing.Catalogue {
	t.Helper()
	data, err := os.ReadFile("../../../../docs/contracts/billing/catalogue.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cat, err := billing.ParseCatalogue(data)
	if err != nil {
		t.Fatal(err)
	}
	return cat
}

func newWorld(t *testing.T, change ...func(*stripe.Options)) *world {
	t.Helper()
	clock := billingtest.NewClock(time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC))
	cat := contractCatalogue(t)
	w := &world{
		t: t, clock: clock, accounts: billingtest.NewAccounts(clock),
		metronome: billingtest.NewMetronome(clock, billingtest.NewSessions(clock), cat),
		stripe:    stripetest.NewStripe(clock), mux: http.NewServeMux(),
		balances: map[string]int64{},
	}
	accounts := &flakyAccounts{Accounts: w.accounts, err: &w.accountsErr}
	w.ledger = &ledger{Ledger: billing.NewLedger(w.metronome, accounts, clock, cat), asked: map[string]billing.Grant{}}
	w.stripe.SetPrices(stripe.LookupKeys(cat)...)
	opt := stripe.Options{
		Mode: "test", WebhookSecret: whsec, PublicURL: publicURL, AutoRecharge: true,
		Catalogue: cat,
	}
	for _, c := range change {
		c(&opt)
	}
	var err error
	if w.svc, err = stripe.New(accounts, w.ledger, w.stripe, clock, opt); err != nil {
		t.Fatal(err)
	}
	if err := w.svc.RefreshPrices(context.Background()); err != nil {
		t.Fatal(err)
	}
	w.svc.Register(w.mux)
	w.webhook = w.svc.Webhook()
	return w
}

// as makes a request to the API as a signed-in user and returns the status
// and the decoded body.
func (w *world) as(u auth.User, method, path, body string) (int, map[string]any) {
	w.t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	w.mux.ServeHTTP(rec, r.WithContext(auth.WithUser(r.Context(), u)))
	var out map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			w.t.Fatalf("%s %s: body is not JSON: %q", method, path, rec.Body.String())
		}
	}
	return rec.Code, out
}

func (w *world) call(owner, method, path, body string) (int, map[string]any) {
	w.t.Helper()
	return w.as(auth.User{Subject: owner}, method, path, body)
}

// post signs an event with the made-up secret and posts it to the webhook.
func (w *world) post(ev stripetest.Event) int {
	w.t.Helper()
	return w.postSigned(ev.Payload, stripetest.Sign(ev.Payload, whsec, time.Now()))
}

func (w *world) postSigned(payload []byte, signature string) int {
	w.t.Helper()
	r := httptest.NewRequest(http.MethodPost, stripe.WebhookPath, bytes.NewReader(payload))
	r.Header.Set("Stripe-Signature", signature)
	rec := httptest.NewRecorder()
	w.webhook.ServeHTTP(rec, r)
	if rec.Body.Len() != 0 {
		w.t.Errorf("the webhook answered a body: %q", rec.Body.String())
	}
	return rec.Code
}

// ok posts an event and requires a 200.
func (w *world) ok(ev stripetest.Event) {
	w.t.Helper()
	if code := w.post(ev); code != http.StatusOK {
		w.t.Fatalf("%s: status %d, want 200", ev.Type, code)
	}
}

// startCheckout starts a Checkout as owner and returns its session's ID.
func (w *world) startCheckout(owner, item string) string {
	w.t.Helper()
	body := "{}"
	if item != "" {
		body = `{"item":"` + item + `"}`
	}
	code, out := w.call(owner, "POST", "/api/billing/checkout", body)
	if code != http.StatusOK {
		w.t.Fatalf("checkout: status %d: %v", code, out)
	}
	url, _ := out["url"].(string)
	return url[strings.LastIndex(url, "/")+1:]
}

// saveCard is the first-run path: a setup Checkout completed with card, and
// its event handled.
func (w *world) saveCard(owner string, card stripetest.Card) billing.Account {
	w.t.Helper()
	w.ok(w.stripe.CompleteCheckout(w.startCheckout(owner, ""), card))
	return w.account(owner)
}

func (w *world) account(owner string) billing.Account {
	w.t.Helper()
	acct, err := w.accounts.Ensure(context.Background(), owner)
	if err != nil {
		w.t.Fatal(err)
	}
	return acct
}

// state is everything the Service writes, for comparing before and after:
// the Accounts, and the credit in Metronome.
func (w *world) state() string {
	w.t.Helper()
	data, err := json.MarshalIndent(map[string]any{"accounts": w.accounts.All(), "credits": w.metronome.AllCredits()}, "", " ")
	if err != nil {
		w.t.Fatal(err)
	}
	return string(data)
}

// live is the keys of the credits Metronome has that are not archived.
func (w *world) live() []string {
	var keys []string
	for _, c := range w.metronome.AllCredits() {
		if !c.Archived {
			keys = append(keys, c.CustomFields[billing.FieldGrantKey])
		}
	}
	sort.Strings(keys)
	return keys
}

// grant is the Grant made for key, as the ledger was asked for it, revoked
// if Metronome has it archived.
func (w *world) grant(key string) billing.Grant {
	w.t.Helper()
	g, ok := w.ledger.grant(key)
	if !ok {
		w.t.Fatalf("no Grant %q; there are %v", key, w.live())
	}
	archived := true
	for _, live := range w.live() {
		archived = archived && live != key
	}
	if archived != (g.Revoked != nil) {
		w.t.Fatalf("Grant %q: archived in Metronome %v, revoked as asked %+v", key, archived, g.Revoked)
	}
	return g
}

// ledger is the real ledger, and what was asked of it: the Grants as they
// were given, and why each was revoked. Metronome keeps neither a Grant's
// references nor the reason.
type ledger struct {
	billing.Ledger
	mu    sync.Mutex
	asked map[string]billing.Grant // by key
}

func (l *ledger) EnsureGrant(ctx context.Context, g billing.Grant) (bool, billing.Grant, error) {
	created, existing, err := l.Ledger.EnsureGrant(ctx, g)
	if created {
		l.mu.Lock()
		g.Name = billing.GrantName(g.Key)
		l.asked[g.Key] = g
		l.mu.Unlock()
	}
	return created, existing, err
}

func (l *ledger) Revoke(ctx context.Context, sel billing.GrantSelector, reason string) error {
	if err := l.Ledger.Revoke(ctx, sel, reason); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for key, g := range l.asked {
		if sel.Matches(g) {
			g.Revoked = &billing.Revoked{Reason: reason}
			l.asked[key] = g
		}
	}
	return nil
}

func (l *ledger) grant(key string) (billing.Grant, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	g, ok := l.asked[key]
	return g, ok
}

// flakyAccounts is the account store, failing when the test says so.
type flakyAccounts struct {
	billing.Accounts
	err *error
}

func (a *flakyAccounts) Ensure(ctx context.Context, owner string) (billing.Account, error) {
	if *a.err != nil {
		return billing.Account{}, *a.err
	}
	return a.Accounts.Ensure(ctx, owner)
}

func (a *flakyAccounts) Get(ctx context.Context, name string) (billing.Account, error) {
	if *a.err != nil {
		return billing.Account{}, *a.err
	}
	return a.Accounts.Get(ctx, name)
}

func (a *flakyAccounts) ByCustomer(ctx context.Context, id string) (billing.Account, error) {
	if *a.err != nil {
		return billing.Account{}, *a.err
	}
	return a.Accounts.ByCustomer(ctx, id)
}

func (a *flakyAccounts) ByPaymentMethod(ctx context.Context, id string) (billing.Account, error) {
	if *a.err != nil {
		return billing.Account{}, *a.err
	}
	return a.Accounts.ByPaymentMethod(ctx, id)
}

func (a *flakyAccounts) WithCustomer(ctx context.Context) ([]billing.Account, error) {
	if *a.err != nil {
		return nil, *a.err
	}
	return a.Accounts.WithCustomer(ctx)
}

func (a *flakyAccounts) Update(ctx context.Context, name string, change func(*billing.AccountSpec) error) (billing.Account, error) {
	if *a.err != nil {
		return billing.Account{}, *a.err
	}
	return a.Accounts.Update(ctx, name, change)
}

func equal(t *testing.T, what string, got, want any) {
	t.Helper()
	g, _ := json.Marshal(got)
	w, _ := json.Marshal(want)
	if string(g) != string(w) {
		t.Errorf("%s: got %s, want %s", what, g, w)
	}
}

func newRequest(method, path string) (*httptest.ResponseRecorder, *http.Request) {
	return httptest.NewRecorder(), httptest.NewRequest(method, path, nil)
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
