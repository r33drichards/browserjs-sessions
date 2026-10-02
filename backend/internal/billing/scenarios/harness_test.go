package scenarios

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/r33drichards/computer-use/backend/internal/api"
	"github.com/r33drichards/computer-use/backend/internal/auth"
	"github.com/r33drichards/computer-use/backend/internal/authz"
	"github.com/r33drichards/computer-use/backend/internal/billing"
	"github.com/r33drichards/computer-use/backend/internal/billing/billingtest"
	"github.com/r33drichards/computer-use/backend/internal/billing/metronome"
	"github.com/r33drichards/computer-use/backend/internal/billing/stripe"
	"github.com/r33drichards/computer-use/backend/internal/idle"
	"github.com/r33drichards/computer-use/backend/internal/proxy"
	"github.com/r33drichards/computer-use/backend/internal/sessions"
)

const (
	publicURL    = "https://app.example.test"
	billingURL   = publicURL + "/billing"
	appHost      = "app.example.test"
	apiHost      = "api.example.test"
	sessionsHost = "sessions.example.test"

	user  = "u@example.com"
	other = "v@example.com"
	admin = "root@example.com" // an admin, and in BILLING_EXEMPT_EMAILS
)

// The catalogue is the contract's.
const cataloguePath = "../../../../docs/contracts/billing/catalogue.yaml"

// textVerifier takes the assertion's text for the user's email: Pomerium's
// part, played by the test.
type textVerifier struct{}

func (textVerifier) Verify(_ context.Context, raw, host string) (auth.User, error) {
	if raw == "" || host == "" {
		return auth.User{}, errors.New("bad assertion")
	}
	return auth.User{Subject: raw, Name: raw, Admin: raw == admin}, nil
}

// world is the backend with billing on, over the fakes.
type world struct {
	t         *testing.T
	clock     *billingtest.Clock
	accounts  *billingtest.Accounts
	sessions  *billingtest.Sessions
	metronome *billingtest.Metronome
	ledger    billing.Ledger // the real one, over the fake Metronome
	pass      *metronome.Pass
	stripe    *billingtest.Stripe
	calls     *billingtest.InFlight // what the test says is in flight
	enforcer  *billing.Enforcer
	proxy     *proxy.Proxy
	handler   http.Handler
	catalogue billing.Catalogue

	podCalls atomic.Int64 // requests that reached a session's pod
	// pod, when set, answers for the pod.
	pod atomic.Pointer[http.HandlerFunc]
}

// flights is the proxy's calls in flight plus those the test says there
// are; closing a session's streams is the proxy's, and is counted.
type flights struct {
	said  *billingtest.InFlight
	proxy *proxy.Proxy
}

func (f flights) Calls(id string) int { return f.said.Calls(id) + f.proxy.Calls(id) }
func (f flights) CloseStreams(id string) {
	f.said.CloseStreams(id)
	f.proxy.CloseStreams(id)
}

var start = time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC)

// newWorld is a backend with BILLING=enforce and the deploy.md defaults,
// changed by with.
func newWorld(t *testing.T, with func(*billing.Config)) *world {
	t.Helper()
	raw, err := os.ReadFile(cataloguePath)
	if err != nil {
		t.Fatal(err)
	}
	catalogue, err := billing.ParseCatalogue(raw)
	if err != nil {
		t.Fatal(err)
	}
	cfg := billing.Config{
		Mode: billing.Enforce, PublicURL: publicURL,
		Grace: 5 * time.Minute, DrainTimeout: 10 * time.Minute,
		ExemptEmails: []string{admin}, MaxAwakeSessions: 10, WakesPerHour: 30,
		SignupCredit: true, Payments: "test",
	}
	if with != nil {
		with(&cfg)
	}
	w := &world{t: t, clock: billingtest.NewClock(start), calls: billingtest.NewInFlight(), catalogue: catalogue}
	w.accounts = billingtest.NewAccounts(w.clock)
	w.sessions = billingtest.NewSessions(w.clock)
	w.metronome = billingtest.NewMetronome(w.clock, w.sessions, catalogue)
	w.ledger = billing.NewLedger(w.metronome, w.accounts, w.clock, catalogue)
	w.pass = &metronome.Pass{Accounts: w.accounts, Ledger: w.ledger, Sessions: w.sessions, Clock: w.clock}
	w.stripe = billingtest.NewStripe(w.clock)
	w.enforcer = billing.NewEnforcer(cfg, w.accounts, w.ledger, w.sessions, w.clock, catalogue)

	pod := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w.podCalls.Add(1)
		if h := w.pod.Load(); h != nil {
			(*h)(rw, r)
			return
		}
		rw.Header().Set("Content-Type", "application/json")
		_, _ = rw.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	t.Cleanup(pod.Close)

	urls, err := sessions.ParseURLTemplate("https://" + sessionsHost + "/{id}")
	if err != nil {
		t.Fatal(err)
	}
	// The route table, as cmd/server builds it.
	owners := authz.NewOwners(w.sessions, 0)
	w.proxy = &proxy.Proxy{
		Verifier: textVerifier{}, Authz: owners,
		Waker:   &proxy.Waker{Store: w.sessions, Timeout: 5 * time.Second, Poll: 5 * time.Millisecond, RunningTTL: 2 * time.Second},
		Idle:    idle.New(15*time.Minute, w.clock.Now),
		URLs:    urls,
		Billing: w.enforcer,
		Target:  func(sessions.Session, int) string { return pod.Listener.Addr().String() },
	}
	// The Stripe side is the real one (backend/internal/billing/stripe):
	// its checkout route and its webhook handler, over the fake Stripe.
	payments, err := stripe.New(w.accounts, w.ledger, w.stripe, w.clock, stripe.Options{
		Mode: "test", WebhookSecret: billingtest.WebhookSecret, PublicURL: publicURL,
		NoSignupCredit: !cfg.SignupCredit, Catalogue: catalogue,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := payments.RefreshPrices(t.Context()); err != nil {
		t.Fatal(err)
	}

	apiMux := http.NewServeMux()
	sessionAPI := api.New(w.sessions, owners, urls, 5)
	sessionAPI.EnableBilling(w.enforcer)
	(&billing.Handlers{Enforcer: w.enforcer, Stripe: w.stripe}).Register(apiMux)
	payments.Register(apiMux)
	sessionAPI.Register(apiMux)
	w.proxy.RegisterApp(apiMux)

	app := http.NewServeMux()
	app.Handle("/api/", auth.Middleware(textVerifier{})(apiMux))
	// Stripe's webhook: nobody signs in, the signature is the credential.
	app.Handle(stripe.WebhookPath, payments.Webhook())
	app.Handle("/metronome/webhook", &metronome.Webhook{Accounts: w.accounts, Ledger: w.ledger, Clock: w.clock, Secret: billingtest.MetronomeSecret})
	w.handler = w.proxy.Handler(app)
	return w
}

type response struct {
	*httptest.ResponseRecorder
}

func (r response) body() string { return strings.TrimSpace(r.Body.String()) }

func (r response) json(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(r.Body.Bytes(), v); err != nil {
		t.Fatalf("bad JSON %q: %v", r.Body.String(), err)
	}
}

func (w *world) request(as, method, host, path, body string) *http.Request {
	req := httptest.NewRequest(method, "https://"+host+path, strings.NewReader(body))
	req.Host = host
	if as != "" {
		req.Header.Set(auth.AssertionHeader, as)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	return req
}

// api is a request to the app's API as a signed-in user.
func (w *world) api(as, method, path, body string) response {
	w.t.Helper()
	rec := httptest.NewRecorder()
	w.handler.ServeHTTP(rec, w.request(as, method, appHost, path, body))
	return response{rec}
}

// mcp is an MCP call to a session through the proxy.
func (w *world) mcp(as, id string) response {
	w.t.Helper()
	rec := httptest.NewRecorder()
	w.handler.ServeHTTP(rec, w.request(as, "POST", sessionsHost, "/"+id+"/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	return response{rec}
}

// webhook posts an event to Stripe's webhook, correctly signed. The
// handler verifies with Stripe's SDK, which reads the machine's clock for
// the signature's age, so that is the time it is signed at.
func (w *world) webhook(e billingtest.Event) response {
	w.t.Helper()
	body := e.Body()
	req := httptest.NewRequest("POST", "https://"+apiHost+"/stripe/webhook", bytes.NewReader(body))
	req.Host = apiHost
	req.Header.Set("Stripe-Signature", billingtest.Sign(billingtest.WebhookSecret, body, time.Now()))
	rec := httptest.NewRecorder()
	w.handler.ServeHTTP(rec, req)
	return response{rec}
}

func (w *world) sweep() {
	w.t.Helper()
	if err := w.enforcer.Sweep(w.t.Context(), flights{w.calls, w.proxy}); err != nil {
		w.t.Fatalf("sweep: %v", err)
	}
}

// alert posts one of Metronome's notifications to its webhook, correctly
// signed.
func (w *world) alert(e billingtest.MetronomeEvent) response {
	w.t.Helper()
	body := e.Body()
	date := w.clock.Now().UTC().Format(time.RFC3339)
	req := httptest.NewRequest("POST", "https://"+apiHost+"/metronome/webhook", bytes.NewReader(body))
	req.Host = apiHost
	req.Header.Set("X-Metronome-Date", date)
	req.Header.Set("Metronome-Webhook-Signature", billingtest.SignMetronome(billingtest.MetronomeSecret, date, body))
	rec := httptest.NewRecorder()
	w.handler.ServeHTTP(rec, req)
	return response{rec}
}

// tick is "Metronome.Tick" of the contract: the fake's Tick at now, and
// each alert it returns posted, signed, to the webhook.
func (w *world) tick() {
	w.t.Helper()
	for _, e := range w.metronome.Tick(w.clock.Now()) {
		if res := w.alert(e); res.Code != http.StatusOK {
			w.t.Fatalf("metronome webhook = %d %s", res.Code, res.body())
		}
	}
}

// balancePass runs the balance pass once.
func (w *world) balancePass() {
	w.t.Helper()
	if err := w.pass.Run(w.t.Context()); err != nil {
		w.t.Fatalf("balance pass: %v", err)
	}
}

// credits is every credit in the fake Metronome that was not archived.
func (w *world) credits() []billing.MetronomeCredit {
	var out []billing.MetronomeCredit
	for _, c := range w.metronome.AllCredits() {
		if !c.Archived {
			out = append(out, c)
		}
	}
	return out
}

func (w *world) account(owner string) billing.Account {
	w.t.Helper()
	acc, err := w.accounts.Get(w.t.Context(), billing.AccountName(owner))
	if err != nil {
		w.t.Fatalf("account of %s: %v", owner, err)
	}
	return acc
}

func (w *world) session(id string) sessions.Session {
	w.t.Helper()
	s, err := w.sessions.Get(w.t.Context(), id)
	if err != nil {
		w.t.Fatalf("session %s: %v", id, err)
	}
	return s
}

// billingOf is GET /api/billing as owner.
func (w *world) billingOf(owner string) billing.BillingView {
	w.t.Helper()
	res := w.api(owner, "GET", "/api/billing", "")
	if res.Code != http.StatusOK {
		w.t.Fatalf("GET /api/billing = %d %s", res.Code, res.body())
	}
	var v billing.BillingView
	res.json(w.t, &v)
	return v
}

// saveCard takes owner through the gate: a setup Checkout, completed with
// the card, and its signed event posted to the webhook. It returns the
// saved payment method's ID.
func (w *world) saveCard(owner string, card billingtest.Card) string {
	w.t.Helper()
	if res := w.api(owner, "POST", "/api/billing/checkout", `{}`); res.Code != http.StatusOK {
		w.t.Fatalf("checkout = %d %s", res.Code, res.body())
	}
	all := w.stripe.CheckoutSessions()
	if res := w.webhook(w.stripe.CompleteCheckout(all[len(all)-1].ID, &card)); res.Code != http.StatusOK {
		w.t.Fatalf("webhook = %d %s", res.Code, res.body())
	}
	ids := w.account(owner).Spec.PaymentMethod.IDs
	return ids[len(ids)-1]
}

// buy is owner buying a pack: a payment Checkout, paid, and its signed
// event posted.
func (w *world) buy(owner, pack string) {
	w.t.Helper()
	if res := w.api(owner, "POST", "/api/billing/checkout", `{"item":"`+pack+`"}`); res.Code != http.StatusOK {
		w.t.Fatalf("checkout = %d %s", res.Code, res.body())
	}
	all := w.stripe.CheckoutSessions()
	if res := w.webhook(w.stripe.CompleteCheckout(all[len(all)-1].ID, nil)); res.Code != http.StatusOK {
		w.t.Fatalf("webhook = %d %s", res.Code, res.body())
	}
}

// grant gives owner's account credit by hand (an admin Grant).
func (w *world) grant(owner string, micros int64) {
	w.t.Helper()
	acc, err := w.accounts.Ensure(w.t.Context(), owner)
	if err != nil {
		w.t.Fatal(err)
	}
	created, _, err := w.ledger.EnsureGrant(w.t.Context(), billing.Grant{Account: acc.Name, Source: billing.SourceAdmin,
		AmountMicros: micros, ValidFrom: w.clock.Now(), Key: "admin/" + owner + "/" + w.clock.Now().Format(time.RFC3339)})
	if err != nil || !created {
		w.t.Fatalf("grant: created %v, %v", created, err)
	}
}

// create is POST /api/sessions as owner, which must succeed.
func (w *world) create(owner string) string {
	w.t.Helper()
	res := w.api(owner, "POST", "/api/sessions", `{}`)
	if res.Code != http.StatusCreated {
		w.t.Fatalf("create = %d %s", res.Code, res.body())
	}
	var s struct {
		ID string `json:"id"`
	}
	res.json(w.t, &s)
	return s.ID
}

// sessionView is a session as the API shows it with billing on.
type sessionView struct {
	ID          string     `json:"id"`
	Owner       string     `json:"owner"`
	State       string     `json:"state"`
	StoppedBy   string     `json:"stoppedBy"`
	Draining    string     `json:"draining"`
	DeleteAfter *time.Time `json:"deleteAfter"`
}

func (w *world) view(as, id string) sessionView {
	w.t.Helper()
	res := w.api(as, "GET", "/api/sessions/"+id, "")
	if res.Code != http.StatusOK {
		w.t.Fatalf("GET session = %d %s", res.Code, res.body())
	}
	var v sessionView
	res.json(w.t, &v)
	return v
}

// refusedWith checks an HTTP refusal: the status, the code, the message
// and the billing URL, and that it does not look like something to retry.
func refusedWith(t *testing.T, step string, res response, status int, code string) billing.ErrorBody {
	t.Helper()
	var body billing.ErrorBody
	if res.Code != status {
		t.Fatalf("%s: %d %s, want %d %s", step, res.Code, res.body(), status, code)
	}
	res.json(t, &body)
	if body.Code != code || body.Error == "" || body.BillingURL != billingURL {
		t.Errorf("%s: body %s, want code %s and the billing URL", step, res.body(), code)
	}
	if got := res.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("%s: Content-Type %q", step, got)
	}
	if status == http.StatusPaymentRequired && res.Header().Get("Retry-After") != "" {
		t.Errorf("%s: a 402 carries Retry-After %q; it is not something to retry", step, res.Header().Get("Retry-After"))
	}
	return body
}

// mcpRefused checks the MCP form of a refusal: the status, JSON, and a
// JSON-RPC error -32002 whose message has what a model is to relay.
func mcpRefused(t *testing.T, step string, res response, status int, says string) {
	t.Helper()
	if res.Code != status {
		t.Fatalf("%s: %d %s, want %d", step, res.Code, res.body(), status)
	}
	if got := res.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("%s: Content-Type %q, want application/json", step, got)
	}
	if got := res.Header().Get("Retry-After"); got != "" {
		t.Errorf("%s: Retry-After %q on a refusal that is not transient", step, got)
	}
	var rpc struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	res.json(t, &rpc)
	if rpc.JSONRPC != "2.0" || rpc.Error.Code != -32002 {
		t.Errorf("%s: not a JSON-RPC error -32002: %s", step, res.body())
	}
	if !strings.Contains(rpc.Error.Message, says) || !strings.Contains(rpc.Error.Message, billingURL) {
		t.Errorf("%s: message %q, want %q and %s in it", step, rpc.Error.Message, says, billingURL)
	}
}

// logs captures what is logged while the test runs.
type logs struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logs) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logs) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func captureLogs(t *testing.T) *logs {
	l := &logs{}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(l, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	return l
}
