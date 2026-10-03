package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/r33drichards/computer-use/backend/internal/auth"
	"github.com/r33drichards/computer-use/backend/internal/billing"
	"github.com/r33drichards/computer-use/backend/internal/billing/billingtest"
	"github.com/r33drichards/computer-use/backend/internal/config"
	"github.com/r33drichards/computer-use/backend/internal/idle"
	"github.com/r33drichards/computer-use/backend/internal/proxy"
	"github.com/r33drichards/computer-use/backend/internal/sessions"
	"github.com/r33drichards/computer-use/backend/internal/sessions/sessionstest"
)

// billedServer is the route table as run() builds it with BILLING on: the
// real session store, and billing's accounts and ledger in memory.
func billedServer(t *testing.T, mode billing.Mode) (*server, *billed) {
	t.Helper()
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html>app</html>"), 0o644)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := auth.NewAssertionVerifier(func(*jwt.Token) (any, error) { return &key.PublicKey, nil }, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		WebDir: dir, PublicURL: sessionstest.PublicURL, SessionURLs: sessionstest.URLs(),
		SignOutURL: "/.pomerium/sign_out", ReadyTimeout: time.Second, MaxSessionsPerUser: 5,
		Billing: billing.Config{Mode: mode, PublicURL: sessionstest.PublicURL, ExemptEmails: []string{root}},
	}
	store, client := sessionstest.New(t)
	catalogue := billing.Catalogue{Rates: billing.Rates{AwakeMicrosPerHour: 200000, DiskMicrosPerGBHour: 384}, SessionDiskGB: 5,
		Payg: billing.Tier{Name: "Pay as you go", MaxSessions: 3, MaxAwake: 2}}
	clock := billingtest.NewClock(time.Now())
	accounts := billingtest.NewAccounts(clock)
	ledger := billing.NewLedger(billingtest.NewMetronome(clock, billingtest.NewSessions(clock), catalogue), accounts, clock, catalogue)
	enforcer := billing.NewEnforcer(cfg.Billing, accounts, ledger, billing.Store{Store: store}, clock, catalogue)
	parts := &billingParts{enforcer: enforcer, handlers: &billing.Handlers{Enforcer: enforcer}}

	s := &server{t: t, key: key, client: client}
	handler, px := newHandlerWith(cfg, verifier, store, idle.New(store, "test", 15*time.Minute, time.Now), parts)
	px.Target = func(sessions.Session, int) string { return "127.0.0.1:1" }
	s.handler = handler
	return s, &billed{clock: clock, accounts: accounts, ledger: ledger, enforcer: enforcer, proxy: px, store: store,
		replica: func(name string) (*server, *proxy.Proxy) {
			handler, px := newHandlerWith(cfg, verifier, store, idle.New(store, name, 15*time.Minute, time.Now), parts)
			return &server{t: t, key: key, client: client, handler: handler}, px
		}}
}

// billed is billing's parts behind a billedServer.
type billed struct {
	clock    *billingtest.Clock
	accounts *billingtest.Accounts
	ledger   billing.Ledger
	enforcer *billing.Enforcer
	proxy    *proxy.Proxy
	store    *sessions.Store
	// replica is another replica of the server: the same cluster and the
	// same billing, a route table and a proxy of its own.
	replica func(name string) (*server, *proxy.Proxy)
}

// card gives owner's account a saved card and credit, as the webhook and
// the operator would.
func (b *billed) card(t *testing.T, owner string, present bool) {
	t.Helper()
	acc, err := b.accounts.Ensure(t.Context(), owner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.accounts.Update(t.Context(), acc.Name, func(spec *billing.AccountSpec) error {
		spec.PaymentMethod = &billing.PaymentMethods{Present: present, ReadAt: b.clock.Now()}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.ledger.EnsureGrant(t.Context(), billing.Grant{Account: acc.Name, Source: billing.SourceAdmin, AmountMicros: 5000000,
		ValidFrom: b.clock.Now().Add(-time.Hour), Key: "admin/" + owner}); err != nil {
		t.Fatal(err)
	}
}

// With BILLING=enforce the server's own route table has the gate at
// create, resume and wake, and the read routes; the exempt are let
// through.
func TestBillingIsWiredIn(t *testing.T) {
	s, b := billedServer(t, billing.Enforce)
	accounts := b.accounts

	rec := s.do("GET", appHost, "/api/billing", alice, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"state":"no_card"`) || !strings.Contains(rec.Body.String(), `"mode":"enforce"`) {
		t.Fatalf("GET /api/billing: %d %s", rec.Code, rec.Body)
	}
	if rec := s.do("GET", appHost, "/api/billing", "", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("GET /api/billing signed out: %d", rec.Code)
	}
	if rec := s.do("GET", appHost, "/api/billing/catalogue", alice, ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"awakeMicrosPerHour":200000`) {
		t.Errorf("catalogue: %d %s", rec.Code, rec.Body)
	}
	rec = s.do("POST", appHost, "/api/sessions", alice, `{}`)
	if rec.Code != http.StatusPaymentRequired || !strings.Contains(rec.Body.String(), `"code":"payment_method_required"`) ||
		!strings.Contains(rec.Body.String(), `"billingUrl":"https://app.example.com/billing"`) {
		t.Fatalf("create with no card: %d %s", rec.Code, rec.Body)
	}

	// An admin is exempt (BILLING_EXEMPT_EMAILS defaults to ADMIN_EMAILS)
	// and creates a session with no card. Blocked comes before exempt:
	// once the account is blocked, its session, asleep, cannot be woken by
	// a call or resumed.
	created := s.session(root)
	if _, err := accounts.Update(t.Context(), billing.AccountName(root), func(spec *billing.AccountSpec) error {
		spec.Blocked = &billing.Blocked{Reason: "admin"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.suspend(created.ID, sessions.StoppedByIdle); err != nil {
		t.Fatal(err)
	}
	rec = s.do("POST", sessionsHost, "/"+created.ID+"/mcp", root, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), `"code":-32002`) || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("wake by a blocked account: %d %s", rec.Code, rec.Body)
	}
	rec = s.do("PATCH", appHost, "/api/sessions/"+created.ID, root, `{"action":"resume"}`)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), `"code":"account_blocked"`) {
		t.Fatalf("resume by a blocked account: %d %s", rec.Code, rec.Body)
	}
}

// suspend puts a session to sleep in the cluster, as its controller would
// finish it.
func (s *server) suspend(id, by string) error {
	store, err := sessions.NewStore(s.client, sessionstest.Namespace, sessionstest.Blueprint, sessionstest.PublicURL, sessionstest.URLs())
	if err != nil {
		return err
	}
	if err := store.Suspend(s.t.Context(), id, by); err != nil {
		return err
	}
	sessionstest.SetStatus(s.t, s.client, id, sessionstest.Suspended())
	return nil
}

// A nil *billingParts is BILLING off: the server is what it was.
func TestBillingOffIsNotWiredIn(t *testing.T) {
	s := newServer(t)
	for _, path := range []string{"/api/billing", "/api/billing/usage", "/api/billing/catalogue"} {
		if rec := s.do("GET", appHost, path, alice, ""); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, rec.Code)
		}
	}
	if rec := s.do("POST", appHost, "/api/sessions", alice, `{}`); rec.Code != http.StatusCreated {
		t.Errorf("create: %d %s", rec.Code, rec.Body)
	}
	var none *billingParts
	none.run(t.Context(), nil)
	none.revokeTokensWith(nil)
	if h := none.withWebhooks(config.Config{APIURL: "https://api.example.com"}, s.handler); h == nil {
		t.Error("no handler")
	}
	if parts, err := newBilling(t.Context(), config.Config{Billing: billing.Config{Mode: billing.Off}}, nil, nil); parts != nil || err != nil {
		t.Errorf("newBilling with BILLING off = %v, %v", parts, err)
	}
}

// The sweep over the real session store, with the real proxy as its count
// of calls in flight: a session whose owner's card is gone is marked,
// then put to sleep with the reason, and is asleep to the API.
func TestSweepOverTheRealStore(t *testing.T) {
	s, b := billedServer(t, billing.Enforce)
	b.card(t, alice, true)
	created := s.session(alice)
	sweep := func() {
		t.Helper()
		if err := b.enforcer.Sweep(t.Context(), b.proxy); err != nil {
			t.Fatal(err)
		}
	}
	raw := func() map[string]string {
		t.Helper()
		obj, err := s.client.Resource(sessions.SandboxGVR).Namespace(sessionstest.Namespace).Get(t.Context(), created.ID, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		mode, _ := obj.Object["spec"].(map[string]any)["operatingMode"].(string)
		ann := obj.GetAnnotations()
		return map[string]string{"mode": mode, "stoppedBy": ann[sessions.AnnStoppedBy], "draining": ann[sessions.AnnDraining]}
	}

	// With a card and credit the sweep leaves it alone.
	sweep()
	if got := raw(); got["mode"] != "Running" || got["draining"] != "" {
		t.Fatalf("a paid-up account's session: %v", got)
	}
	b.card(t, alice, false)
	sweep()
	if got := raw(); got["mode"] != "Suspended" || got["stoppedBy"] != sessions.StoppedByPaymentMethod || got["draining"] != "" {
		t.Fatalf("after the sweep: %v, want Suspended, payment-method, no mark", got)
	}
	sessionstest.SetStatus(t, s.client, created.ID, sessionstest.Suspended())
	rec := s.do("GET", appHost, "/api/sessions/"+created.ID, alice, "")
	if !strings.Contains(rec.Body.String(), `"state":"asleep"`) || !strings.Contains(rec.Body.String(), `"stoppedBy":"payment-method"`) {
		t.Fatalf("GET: %s", rec.Body)
	}
	// And it is not woken by a call while there is no card.
	rec = s.do("POST", sessionsHost, "/"+created.ID+"/mcp", alice, `{}`)
	if rec.Code != http.StatusPaymentRequired || !strings.Contains(rec.Body.String(), "no payment method") {
		t.Fatalf("wake: %d %s", rec.Code, rec.Body)
	}
	if got := raw(); got["mode"] != "Suspended" {
		t.Fatalf("the call woke it: %v", got)
	}
	// The card back, the same call wakes it.
	b.card(t, alice, true)
	s.do("POST", sessionsHost, "/"+created.ID+"/mcp", alice, `{}`)
	if got := raw(); got["mode"] != "Running" || got["stoppedBy"] != "" {
		t.Fatalf("with the card back: %v", got)
	}
}

// The drain with two replicas. The one that sweeps has no call of its own
// to the session; the other is proxying one. The call is written on the
// session, the sweep reads it there, and the session is not put to sleep
// until it is over, or the drain times out.
func TestDrainWaitsForACallAtAnotherReplica(t *testing.T) {
	type world struct {
		s, other *server
		b        *billed
		id       string
		arrived  chan struct{}
		finish   func()
		answered chan int
	}
	// setup: a session of an account whose card is gone, with a call in
	// flight at the replica that does not sweep.
	setup := func(t *testing.T) *world {
		s, b := billedServer(t, billing.Enforce)
		other, otherProxy := b.replica("other")
		w := &world{s: s, other: other, b: b, arrived: make(chan struct{}), answered: make(chan int, 1)}
		release := make(chan struct{})
		var once sync.Once
		w.finish = func() { once.Do(func() { close(release) }) }
		pod := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
			// The backend's start of a new session's browser is not the call.
			if r.URL.Path == "/browser/start" {
				rw.WriteHeader(http.StatusAccepted)
				return
			}
			close(w.arrived)
			<-release
			_, _ = rw.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
		}))
		t.Cleanup(pod.Close)
		t.Cleanup(w.finish) // first: the pod answers before it is closed
		for _, px := range []*proxy.Proxy{b.proxy, otherProxy} {
			px.Target = func(sessions.Session, int) string { return pod.Listener.Addr().String() }
		}
		b.card(t, alice, true)
		w.id = s.session(alice).ID
		go func() { w.answered <- other.do("POST", sessionsHost, "/"+w.id+"/mcp", alice, `{}`).Code }()
		<-w.arrived
		if got := otherProxy.Calls(w.id); got != 1 || b.proxy.Calls(w.id) != 0 {
			t.Fatalf("calls in flight: %d at the other replica, %d at the one that sweeps; want 1 and 0", got, b.proxy.Calls(w.id))
		}
		b.card(t, alice, false)
		return w
	}
	sweep := func(t *testing.T, w *world) sessions.Session {
		t.Helper()
		if err := w.b.enforcer.Sweep(t.Context(), w.b.proxy); err != nil {
			t.Fatal(err)
		}
		got, err := w.b.store.Get(t.Context(), w.id)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	suspended := func(s sessions.Session) bool { return s.State != sessions.Running }

	t.Run("the call finishes, then the session sleeps", func(t *testing.T) {
		w := setup(t)
		if got := sweep(t, w); suspended(got) || got.Draining != sessions.StoppedByPaymentMethod {
			t.Fatalf("with a call in flight at the other replica: %s, draining %q; want running, marked", got.State, got.Draining)
		}
		// New calls are refused, where the mark has been seen.
		rec := w.s.do("POST", sessionsHost, "/"+w.id+"/mcp", alice, `{}`)
		if rec.Code != http.StatusPaymentRequired {
			t.Fatalf("a new call during the drain: %d %s", rec.Code, rec.Body)
		}
		if got := sweep(t, w); suspended(got) {
			t.Fatalf("put to sleep at the second sweep, the call still in flight: %s", got.State)
		}

		w.finish()
		if code := <-w.answered; code != http.StatusOK {
			t.Fatalf("the call in flight did not finish: %d", code)
		}
		// The other replica vouched for the call until its mark runs out:
		// it does not write again to say the call is over.
		if got := sweep(t, w); suspended(got) {
			t.Fatalf("put to sleep while the other replica's mark still stands: %s", got.State)
		}
		w.b.clock.Advance(46 * time.Second)
		if got := sweep(t, w); !suspended(got) || got.Draining != "" {
			t.Fatalf("once the mark ran out: %s, draining %q; want suspended, no mark", got.State, got.Draining)
		}
		obj, err := w.s.client.Resource(sessions.SandboxGVR).Namespace(sessionstest.Namespace).Get(t.Context(), w.id, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if by := obj.GetAnnotations()[sessions.AnnStoppedBy]; by != sessions.StoppedByPaymentMethod {
			t.Fatalf("stopped by %q, want payment-method", by)
		}
		for key := range obj.GetAnnotations() {
			if key == sessions.AnnLastActive || strings.HasPrefix(key, sessions.AnnInFlightPrefix) {
				t.Errorf("%s is still on the sleeping session", key)
			}
		}
	})

	t.Run("the call never ends; the drain times out", func(t *testing.T) {
		w := setup(t)
		sweep(t, w)
		// The other replica renews its mark for as long as the call runs.
		renew := func() {
			t.Helper()
			if _, err := w.b.store.Mark(t.Context(), w.id, sessions.Activity{Replica: "other", InFlightUntil: w.b.clock.Now().Add(45 * time.Second)}); err != nil {
				t.Fatal(err)
			}
		}
		w.b.clock.Advance(9 * time.Minute)
		renew()
		if got := sweep(t, w); suspended(got) {
			t.Fatalf("put to sleep before the timeout: %s", got.State)
		}
		w.b.clock.Advance(time.Minute + time.Second) // BILLING_DRAIN_TIMEOUT since the mark
		renew()
		if got := sweep(t, w); !suspended(got) {
			t.Fatalf("still %s after BILLING_DRAIN_TIMEOUT", got.State)
		}
	})

	// The race: the session is draining with nothing in flight anywhere,
	// and between the sweep's read for the suspend and its write another
	// replica, which had not seen the mark yet, takes a call in. It wrote
	// that on the session first: the suspend does not go through.
	t.Run("a call taken in as the session is suspended", func(t *testing.T) {
		s, b := billedServer(t, billing.Enforce)
		b.card(t, alice, true)
		id := s.session(alice).ID
		b.card(t, alice, false)
		if err := b.store.SetDraining(t.Context(), id, sessions.StoppedByPaymentMethod); err != nil {
			t.Fatal(err)
		}
		sessionstest.RaceNextGet(t, s.client, id, func(obj *unstructured.Unstructured) {
			ann := obj.GetAnnotations()
			ann[sessions.AnnInFlightPrefix+"other"] = b.clock.Now().Add(45 * time.Second).UTC().Format(time.RFC3339Nano)
			obj.SetAnnotations(ann)
		})
		if err := b.enforcer.Sweep(t.Context(), b.proxy); err != nil {
			t.Fatal(err)
		}
		got, err := b.store.Get(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if got.State != sessions.Running || got.Draining != sessions.StoppedByPaymentMethod {
			t.Fatalf("%s, draining %q; want running and still draining: the call is waited for", got.State, got.Draining)
		}
		b.clock.Advance(46 * time.Second)
		if err := b.enforcer.Sweep(t.Context(), b.proxy); err != nil {
			t.Fatal(err)
		}
		if got, _ := b.store.Get(t.Context(), id); got.State == sessions.Running {
			t.Fatal("never put to sleep once the call's mark ran out")
		}
	})
}

// Metronome's webhook is on the API host, in front of everything, and
// nowhere else.
func TestMetronomeWebhookIsOnTheAPIHost(t *testing.T) {
	s, b := billedServer(t, billing.Enforce)
	hits := 0
	parts := &billingParts{enforcer: b.enforcer, webhook: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hits++ })}
	s.handler = parts.withWebhooks(config.Config{APIURL: "https://api.example.com"}, s.handler)

	if rec := s.do("POST", "api.example.com", "/metronome/webhook", "", `{}`); rec.Code != http.StatusOK || hits != 1 {
		t.Fatalf("on the API host: %d, %d hits", rec.Code, hits)
	}
	for _, host := range []string{appHost, sessionsHost} {
		if rec := s.do("POST", host, "/metronome/webhook", "", `{}`); hits != 1 {
			t.Errorf("on %s the webhook was reached (%d)", host, rec.Code)
		}
	}
	// With no secret there is no webhook, and the path is nobody's.
	parts.webhook = nil
	if h := parts.withWebhooks(config.Config{APIURL: "https://api.example.com"}, http.NotFoundHandler()); h == nil {
		t.Fatal("no handler")
	}
}
