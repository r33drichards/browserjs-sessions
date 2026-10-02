package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/dynamic"
	dynfake "k8s.io/client-go/dynamic/fake"

	"github.com/r33drichards/computer-use/backend/internal/api"
	"github.com/r33drichards/computer-use/backend/internal/auth"
	"github.com/r33drichards/computer-use/backend/internal/authz"
	"github.com/r33drichards/computer-use/backend/internal/billing"
	"github.com/r33drichards/computer-use/backend/internal/billing/billingtest"
	"github.com/r33drichards/computer-use/backend/internal/idle"
	"github.com/r33drichards/computer-use/backend/internal/proxy"
	"github.com/r33drichards/computer-use/backend/internal/sessions"
	"github.com/r33drichards/computer-use/backend/internal/sessions/sessionstest"
)

// The refusal of a create by a user whose account has no card, byte for
// byte (docs/contracts/billing/testing.md).
const noCard = `{"error":"Add a payment method to create or wake sessions.","code":"payment_method_required","billingUrl":"https://app.example.com/billing"}`

var billingStart = time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC)

// billed is the API with BILLING=enforce, over whichever session store the
// test brings.
type billed struct {
	t        *testing.T
	handler  http.Handler
	clock    *billingtest.Clock
	accounts *billingtest.Accounts
	ledger   billing.Ledger
}

// catalogue is enough of one for the gate.
var catalogue = billing.Catalogue{
	Rates: billing.Rates{AwakeMicrosPerHour: 200000, DiskMicrosPerGBHour: 384}, SessionDiskGB: 5,
	Payg: billing.Tier{Name: "Pay as you go", MaxSessions: 3, MaxAwake: 2},
}

func newBilled(t *testing.T, store api.Store, forBilling billing.Sessions) *billed {
	clock := billingtest.NewClock(billingStart)
	accounts := billingtest.NewAccounts(clock)
	// The real Ledger, over a fake Metronome that meters nothing here.
	ledger := billing.NewLedger(billingtest.NewMetronome(clock, billingtest.NewSessions(clock), catalogue), accounts, clock, catalogue)
	enforcer := billing.NewEnforcer(billing.Config{Mode: billing.Enforce, PublicURL: sessionstest.PublicURL, MaxAwakeSessions: 10, WakesPerHour: 30},
		accounts, ledger, forBilling, clock, catalogue)
	mux := http.NewServeMux()
	a := api.New(store, authz.NewOwners(store, 0), sessionstest.URLs(), 5)
	a.EnableBilling(enforcer)
	a.Register(mux)
	return &billed{t: t, handler: mux, clock: clock, accounts: accounts, ledger: ledger}
}

func (b *billed) do(user auth.User, method, path, body string) *httptest.ResponseRecorder {
	b.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(auth.WithUser(b.t.Context(), user))
	rec := httptest.NewRecorder()
	b.handler.ServeHTTP(rec, req)
	return rec
}

// withCard gives a user's account a saved card and credit, as the webhook
// and the operator would.
func (b *billed) withCard(owner string) {
	b.t.Helper()
	ctx := b.t.Context()
	acc, err := b.accounts.Ensure(ctx, owner)
	if err != nil {
		b.t.Fatal(err)
	}
	if _, err := b.accounts.Update(ctx, acc.Name, func(spec *billing.AccountSpec) error {
		spec.PaymentMethod = &billing.PaymentMethods{Present: true, IDs: []string{"pm_T1"}, ReadAt: b.clock.Now()}
		return nil
	}); err != nil {
		b.t.Fatal(err)
	}
	expires := b.clock.Now().AddDate(0, 0, 90)
	if _, _, err := b.ledger.EnsureGrant(ctx, billing.Grant{Account: acc.Name, Source: billing.SourceSignup, AmountMicros: 5000000,
		ValidFrom: b.clock.Now(), ExpiresAt: &expires, Key: "signup/" + owner}); err != nil {
		b.t.Fatal(err)
	}
}

// A user whose account has no card cannot create a session: 402
// payment_method_required, and the store's Create is never called.
func TestNoCardCannotCreate(t *testing.T) {
	store := billingtest.NewSessions(billingtest.NewClock(billingStart))
	b := newBilled(t, store, store)

	for _, body := range []string{`{}`, `{"name":"mine"}`, ``} {
		rec := b.do(alice, "POST", "/api/sessions", body)
		if rec.Code != http.StatusPaymentRequired || strings.TrimSpace(rec.Body.String()) != noCard {
			t.Fatalf("create %q: %d %s\nwant 402 %s", body, rec.Code, rec.Body, noCard)
		}
		if got := rec.Header().Get("Retry-After"); got != "" {
			t.Errorf("Retry-After = %q on a refusal that is not transient", got)
		}
	}
	if store.Creates() != 0 || store.Len() != 0 {
		t.Fatalf("Create was called %d times; %d sessions", store.Creates(), store.Len())
	}
	// The account was made on first sight, with no card.
	if acc, err := b.accounts.Get(t.Context(), billing.AccountName(alice.Subject)); err != nil || acc.Spec.PaymentMethod != nil {
		t.Fatalf("account %+v, %v", acc, err)
	}

	// With a card, create works; and only for the user who has it.
	b.withCard(alice.Subject)
	if rec := b.do(alice, "POST", "/api/sessions", `{}`); rec.Code != http.StatusCreated {
		t.Fatalf("create with a card: %d %s", rec.Code, rec.Body)
	}
	if rec := b.do(bob, "POST", "/api/sessions", `{}`); rec.Code != http.StatusPaymentRequired {
		t.Fatalf("bob, who has no card: %d %s", rec.Code, rec.Body)
	}
	if store.Creates() != 1 || store.Len() != 1 {
		t.Fatalf("Create was called %d times; %d sessions", store.Creates(), store.Len())
	}
}

// tokenFor is a token verifier with one token, alice's.
type tokenFor struct{ owner string }

func (v tokenFor) VerifyToken(_ context.Context, token string) (string, auth.TokenInfo, error) {
	if token != "bjs_good" {
		return "", auth.TokenInfo{}, auth.ErrInvalidToken
	}
	return v.owner, auth.TokenInfo{Name: "ci", Scopes: auth.Scopes}, nil
}

func (tokenFor) ExchangeToken(context.Context, string, string, []string) (string, auth.Grant, error) {
	return "", auth.Grant{}, auth.ErrInvalidToken
}

// The same through the API host, with an API token of that user: a token
// acts as its owner, and is judged on its owner's account.
func TestNoCardCannotCreateWithToken(t *testing.T) {
	store := billingtest.NewSessions(billingtest.NewClock(billingStart))
	b := newBilled(t, store, store)
	host := auth.NewAPIHost(auth.APIHostConfig{
		Tokens:         tokenFor{alice.Subject},
		Allowed:        auth.NewAllowList([]string{alice.Subject}),
		Limiter:        auth.NewFailureLimiter(10, time.Second, time.Now),
		App:            b.handler,
		ValidSessionID: sessions.ValidID,
		SessionBase:    sessionstest.URLs().Base,
	})
	post := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "https://api.example.com/v1/sessions", strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer bjs_good")
		rec := httptest.NewRecorder()
		host.ServeHTTP(rec, req)
		return rec
	}

	rec := post()
	if rec.Code != http.StatusPaymentRequired || strings.TrimSpace(rec.Body.String()) != noCard {
		t.Fatalf("POST /v1/sessions: %d %s\nwant 402 %s", rec.Code, rec.Body, noCard)
	}
	if store.Creates() != 0 {
		t.Fatalf("Create was called %d times", store.Creates())
	}
	// The account judged is the token's owner's.
	if _, err := b.accounts.Get(t.Context(), billing.AccountName(alice.Subject)); err != nil || b.accounts.Made() != 1 {
		t.Fatalf("accounts made: %d (%v)", b.accounts.Made(), err)
	}
	b.withCard(alice.Subject)
	if rec := post(); rec.Code != http.StatusCreated {
		t.Fatalf("with its owner's card: %d %s", rec.Code, rec.Body)
	}
}

// The same with the real store and a warm pool: the refusal comes before
// anything is made, so no SandboxClaim is created and no warm Sandbox is
// adopted.
func TestNoCardCannotCreateWarm(t *testing.T) {
	store, client := sessionstest.New(t)
	store.EnableWarmPool(sessionstest.WarmPoolName, time.Second)
	sessionstest.PlayClaimController(t, client, "s-bcdfg")
	b := newBilled(t, store, billing.Store{Store: store})

	rec := b.do(alice, "POST", "/api/sessions", `{}`)
	if rec.Code != http.StatusPaymentRequired || strings.TrimSpace(rec.Body.String()) != noCard {
		t.Fatalf("create: %d %s\nwant 402 %s", rec.Code, rec.Body, noCard)
	}
	if claims := sessionstest.Claims(t, client); len(claims) != 0 {
		t.Fatalf("%d SandboxClaims were created", len(claims))
	}
	if all, err := store.ListAll(t.Context()); err != nil || len(all) != 0 {
		t.Fatalf("sessions exist: %+v, %v", all, err)
	}
	for _, action := range client.(*dynfake.FakeDynamicClient).Actions() {
		if action.GetVerb() != "list" && action.GetVerb() != "watch" {
			t.Errorf("the cluster was written to: %s %s", action.GetVerb(), action.GetResource().Resource)
		}
	}

	// With a card the same request adopts the warm Sandbox.
	b.withCard(alice.Subject)
	rec = b.do(alice, "POST", "/api/sessions", `{}`)
	if rec.Code != http.StatusCreated || decode[session](t, rec).ID != "s-bcdfg" {
		t.Fatalf("with a card: %d %s, want the warm Sandbox s-bcdfg", rec.Code, rec.Body)
	}
	if claims := sessionstest.Claims(t, client); len(claims) != 1 {
		t.Fatalf("%d SandboxClaims, want one", len(claims))
	}
}

// With billing enforced: a resume is judged, the session's view carries
// billing's fields, and stopping and deleting are never refused.
func TestResumeIsJudgedAndTheViewSaysWhy(t *testing.T) {
	clock := billingtest.NewClock(billingStart)
	store := billingtest.NewSessions(clock)
	b := newBilled(t, store, store)
	b.withCard(alice.Subject)
	created := decode[session](t, b.do(alice, "POST", "/api/sessions", `{"name":"one"}`))
	if err := store.Sleep(t.Context(), created.ID, sessions.StoppedByCredit, nil); err != nil {
		t.Fatal(err)
	}
	rec := b.do(alice, "GET", "/api/sessions/"+created.ID, "")
	if !strings.Contains(rec.Body.String(), `"state":"asleep"`) || !strings.Contains(rec.Body.String(), `"stoppedBy":"credit"`) {
		t.Fatalf("GET: %s", rec.Body)
	}
	if rec := b.do(alice, "GET", "/api/sessions", ""); !strings.Contains(rec.Body.String(), `"stoppedBy":"credit"`) {
		t.Fatalf("list: %s", rec.Body)
	}
	// The card goes: resume is refused, and the session stays asleep.
	if _, err := b.accounts.Update(t.Context(), billing.AccountName(alice.Subject), func(spec *billing.AccountSpec) error {
		spec.PaymentMethod.Present = false
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	rec = b.do(alice, "PATCH", "/api/sessions/"+created.ID, `{"action":"resume","name":"renamed"}`)
	if rec.Code != http.StatusPaymentRequired || strings.TrimSpace(rec.Body.String()) != noCard {
		t.Fatalf("resume: %d %s", rec.Code, rec.Body)
	}
	if s, _ := store.Get(t.Context(), created.ID); s.State != sessions.Asleep || s.Name != "one" {
		t.Fatalf("a refused resume changed the session: %+v", s)
	}
	// An admin's resume is judged on the owner's account too.
	if rec := b.do(root, "PATCH", "/api/sessions/"+created.ID, `{"action":"resume"}`); rec.Code != http.StatusPaymentRequired {
		t.Fatalf("an admin's resume: %d %s", rec.Code, rec.Body)
	}
	// Renaming, stopping and deleting are not billing's to refuse.
	for _, body := range []string{`{"name":"renamed"}`, `{"action":"stop"}`} {
		if rec := b.do(alice, "PATCH", "/api/sessions/"+created.ID, body); rec.Code != http.StatusOK {
			t.Fatalf("PATCH %s: %d %s", body, rec.Code, rec.Body)
		}
	}
	if rec := b.do(alice, "DELETE", "/api/sessions/"+created.ID, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
}

var (
	sessionIDs = regexp.MustCompile(`s-[a-z2-7]{10}`)
	timestamps = regexp.MustCompile(`"created":"[^"]*"`)
)

// shape is a response body with what differs from run to run taken out.
func shape(body string) string {
	body = sessionIDs.ReplaceAllString(strings.TrimSpace(body), "s-ID")
	return timestamps.ReplaceAllString(body, `"created":"T"`)
}

// With BILLING unset the API and the proxy are what they were: the
// responses of create, resume and wake are byte for byte today's, and no
// Account is made. The bodies below are those of the commit before
// billing.
func TestBillingOffIsToday(t *testing.T) {
	store, client := sessionstest.New(t)
	owners := authz.NewOwners(store, 0)
	urls := sessionstest.URLs()
	px := &proxy.Proxy{
		Verifier: assertion{}, Authz: owners,
		Waker: &proxy.Waker{Store: store, Timeout: 100 * time.Millisecond, Poll: 5 * time.Millisecond},
		Idle:  idle.New(store, "test", 15*time.Minute, time.Now), URLs: urls,
	}
	mux := http.NewServeMux()
	a := api.New(store, owners, urls, 2) // no EnableBilling: BILLING is unset
	a.SetPetName(func() string { return "brave-otter" })
	a.Register(mux)
	px.RegisterApp(mux)
	app := http.NewServeMux()
	app.Handle("/api/", auth.Middleware(assertion{})(mux))
	handler := px.Handler(app)
	do := func(method, host, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "https://"+host+path, strings.NewReader(body))
		req.Host = host
		req.Header.Set(auth.AssertionHeader, alice.Subject)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	const app0, sessions0 = "app.example.com", "sessions.example.com"
	same := func(what string, rec *httptest.ResponseRecorder, status int, body string) {
		t.Helper()
		if rec.Code != status || shape(rec.Body.String()) != body {
			t.Fatalf("%s: %d %s\nwant today's %d %s", what, rec.Code, shape(rec.Body.String()), status, body)
		}
	}

	// Create: a user with no account of any kind creates a session.
	const starting = `{"id":"s-ID","name":"brave-otter","owner":"alice@example.com","state":"starting","created":"T","mcp_url":"https://sessions.example.com/s-ID/mcp"}`
	rec := do("POST", app0, "/api/sessions", `{}`)
	same("create", rec, http.StatusCreated, starting)
	var created session
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	id := created.ID
	// The cap is the API's own.
	do("POST", app0, "/api/sessions", `{}`)
	same("over the cap", do("POST", app0, "/api/sessions", `{}`), http.StatusConflict, `{"error":"session limit reached; delete one first"}`)

	// Resume: of a session its user stopped.
	if err := store.Suspend(t.Context(), id, sessions.StoppedByUser); err != nil {
		t.Fatal(err)
	}
	sessionstest.SetStatus(t, client, id, sessionstest.Suspended())
	same("a stopped session", do("GET", app0, "/api/sessions/"+id, ""), http.StatusOK,
		`{"id":"s-ID","name":"brave-otter","owner":"alice@example.com","state":"stopped","created":"T","mcp_url":"https://sessions.example.com/s-ID/mcp"}`)
	// The fake cluster leaves the old Suspended condition behind, so a
	// resumed session reads as starting, with the condition's message.
	rec = do("PATCH", app0, "/api/sessions/"+id, `{"action":"resume"}`)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "stoppedBy") || !strings.Contains(rec.Body.String(), `"state":"starting"`) {
		t.Fatalf("resume: %d %s", rec.Code, rec.Body)
	}

	// Wake: an MCP call to a session asleep for being idle wakes it and
	// waits for it; one its user stopped is left stopped.
	if err := store.Suspend(t.Context(), id, sessions.StoppedByIdle); err != nil {
		t.Fatal(err)
	}
	sessionstest.SetStatus(t, client, id, sessionstest.Suspended())
	same("an asleep session", do("GET", app0, "/api/sessions/"+id, ""), http.StatusOK,
		`{"id":"s-ID","name":"brave-otter","owner":"alice@example.com","state":"asleep","created":"T","mcp_url":"https://sessions.example.com/s-ID/mcp"}`)
	rec = do("POST", sessions0, "/"+id+"/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	same("wake", rec, http.StatusGatewayTimeout, "session is waking up; retry shortly")
	if rec.Header().Get("Retry-After") != "10" {
		t.Errorf("wake: Retry-After = %q, want today's 10", rec.Header().Get("Retry-After"))
	}
	if s, _ := store.Get(t.Context(), id); s.State == sessions.Asleep || s.State == sessions.Stopped {
		t.Fatalf("the call did not wake the session: %s", s.State)
	}
	if err := store.Suspend(t.Context(), id, sessions.StoppedByUser); err != nil {
		t.Fatal(err)
	}
	sessionstest.SetStatus(t, client, id, sessionstest.Suspended())
	same("a call to a stopped session", do("POST", sessions0, "/"+id+"/mcp", `{}`), http.StatusConflict, "session is stopped; resume it first")

	// Nothing of billing exists: no route, no Account, nothing read or
	// written but Sandboxes.
	for _, path := range []string{"/api/billing", "/api/billing/usage", "/api/billing/catalogue"} {
		if rec := do("GET", app0, path, ""); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404: the route does not exist", path, rec.Code)
		}
	}
	if rec := do("DELETE", app0, "/api/account", `{"confirm":"alice@example.com"}`); rec.Code != http.StatusNotFound {
		t.Errorf("DELETE /api/account = %d, want 404", rec.Code)
	}
	for _, action := range client.(*dynfake.FakeDynamicClient).Actions() {
		if got := action.GetResource().Resource; got != sessions.SandboxGVR.Resource {
			t.Errorf("billing off, and yet: %s %s", action.GetVerb(), got)
		}
	}
}

// assertion takes the assertion's text for the user's email.
type assertion struct{}

func (assertion) Verify(_ context.Context, raw, _ string) (auth.User, error) {
	return auth.User{Subject: raw, Name: "Alice"}, nil
}

var _ dynamic.Interface = (*dynfake.FakeDynamicClient)(nil)
