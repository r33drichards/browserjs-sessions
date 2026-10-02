package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	dynfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/r33drichards/computer-use/backend/internal/api"
	"github.com/r33drichards/computer-use/backend/internal/auth"
	"github.com/r33drichards/computer-use/backend/internal/authz"
	"github.com/r33drichards/computer-use/backend/internal/sessions"
	"github.com/r33drichards/computer-use/backend/internal/sessions/sessionstest"
)

type fixture struct {
	t       *testing.T
	handler http.Handler
	api     *api.API
	store   *sessions.Store
	client  dynamic.Interface
	faults  *faulty
}

// faulty is a checker that can be made to fail, and counts what it is asked.
type faulty struct {
	authz.Checker
	mu       sync.Mutex
	checkErr error
	checks   int
}

func (f *faulty) fail(with error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checkErr = with
}

func (f *faulty) Allowed(ctx context.Context, u auth.User, sessionID string) (bool, error) {
	f.mu.Lock()
	f.checks++
	err := f.checkErr
	f.mu.Unlock()
	if err != nil {
		return false, err
	}
	return f.Checker.Allowed(ctx, u, sessionID)
}

func (f *faulty) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.checks
}

var errDown = errors.New("authorization is down")

func newFixture(t *testing.T) *fixture {
	store, client := sessionstest.New(t)
	az := &faulty{Checker: authz.NewOwners(store, 0)}
	mux := http.NewServeMux()
	a := api.New(store, az, sessionstest.URLs(), 2)
	a.Register(mux)
	return &fixture{t: t, handler: mux, api: a, store: store, client: client, faults: az}
}

// do performs a request as user, bypassing the verification of who it is.
func (f *fixture) do(user auth.User, method, path, body string) *httptest.ResponseRecorder {
	f.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(auth.WithUser(f.t.Context(), user))
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("bad JSON %q: %v", rec.Body.String(), err)
	}
	return v
}

// session is a session as the API shows it.
type session struct {
	sessions.Session
	MCPURL string `json:"mcp_url"`
}

var (
	alice = auth.User{Subject: "alice@example.com", Name: "Alice"}
	bob   = auth.User{Subject: "bob@example.com"}
	root  = auth.User{Subject: "root@example.com", Name: "Root", Admin: true}
)

func TestMe(t *testing.T) {
	f := newFixture(t)
	for user, want := range map[auth.User]string{
		alice: `{"admin":false,"email":"alice@example.com","name":"Alice"}`,
		bob:   `{"admin":false,"email":"bob@example.com","name":""}`,
		root:  `{"admin":true,"email":"root@example.com","name":"Root"}`,
	} {
		rec := f.do(user, "GET", "/api/me", "")
		if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != want {
			t.Errorf("%s: %d %s, want %s", user.Subject, rec.Code, rec.Body, want)
		}
		if got := rec.Header().Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q", got)
		}
	}
}

func TestCreateListAndCap(t *testing.T) {
	f := newFixture(t)

	rec := f.do(alice, "POST", "/api/sessions", `{"name":"one"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	created := decode[session](t, rec)
	if created.Name != "one" || created.Owner != "alice@example.com" {
		t.Errorf("created = %+v", created)
	}
	if want := "https://sessions.example.com/" + created.ID + "/mcp"; created.MCPURL != want {
		t.Errorf("mcp_url = %q, want %q", created.MCPURL, want)
	}
	// Creating is all it takes to own the session.
	if rec := f.do(alice, "GET", "/api/sessions/"+created.ID, ""); rec.Code != http.StatusOK {
		t.Errorf("owner GET after create: %d", rec.Code)
	}

	if rec := f.do(alice, "POST", "/api/sessions", `{"name":"`+strings.Repeat("x", 64)+`"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("long name: %d", rec.Code)
	}
	if rec := f.do(alice, "POST", "/api/sessions", `{"name":"two"}`); rec.Code != http.StatusCreated {
		t.Fatalf("second create: %d %s", rec.Code, rec.Body)
	}
	if rec := f.do(alice, "POST", "/api/sessions", `{"name":"three"}`); rec.Code != http.StatusConflict {
		t.Errorf("over cap: %d, want 409", rec.Code)
	}

	if rec := f.do(bob, "POST", "/api/sessions", `{"name":"bobs"}`); rec.Code != http.StatusCreated {
		t.Fatalf("bob's create: %d %s", rec.Code, rec.Body)
	}
	mine := decode[[]session](t, f.do(alice, "GET", "/api/sessions", ""))
	if len(mine) != 2 {
		t.Errorf("alice sees %d sessions, want 2", len(mine))
	}
	for _, s := range mine {
		if s.MCPURL != "https://sessions.example.com/"+s.ID+"/mcp" || s.Owner != "alice@example.com" {
			t.Errorf("listed session = %+v", s)
		}
	}
	// all=1 is honoured for admins only.
	if got := decode[[]session](t, f.do(alice, "GET", "/api/sessions?all=1", "")); len(got) != 2 {
		t.Errorf("non-admin all=1 returned %d", len(got))
	}
	if got := decode[[]session](t, f.do(root, "GET", "/api/sessions?all=1", "")); len(got) != 3 {
		t.Errorf("admin all=1 returned %d, want 3", len(got))
	}
	// Someone with no sessions gets an empty list, not null.
	if rec := f.do(root, "GET", "/api/sessions", ""); strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Errorf("empty list = %s", rec.Body)
	}
}

// The fields the UI reads, by the names it reads them by.
func TestSessionJSON(t *testing.T) {
	f := newFixture(t)
	rec := f.do(alice, "POST", "/api/sessions", `{"name":"one"}`)
	got := decode[map[string]any](t, rec)
	id, _ := got["id"].(string)
	for field, want := range map[string]any{
		"name": "one", "owner": "alice@example.com", "state": "starting",
		"mcp_url": "https://sessions.example.com/" + id + "/mcp",
	} {
		if got[field] != want {
			t.Errorf("%s = %v, want %v (body %s)", field, got[field], want, rec.Body)
		}
	}
	if _, ok := got["created"]; !ok || !sessions.ValidID(id) {
		t.Errorf("body = %s", rec.Body)
	}
	for _, c := range []struct{ method, body string }{{"GET", ""}, {"PATCH", `{"name":"two"}`}} {
		got := decode[map[string]any](t, f.do(alice, c.method, "/api/sessions/"+id, c.body))
		if got["mcp_url"] != "https://sessions.example.com/"+id+"/mcp" {
			t.Errorf("%s: mcp_url = %v", c.method, got["mcp_url"])
		}
	}
}

// The routes on one session, each with a request that would change or show
// something.
var sessionRoutes = []struct{ method, body string }{
	{"GET", ""}, {"PATCH", `{"name":"x"}`}, {"PATCH", `{"action":"stop"}`}, {"DELETE", ""},
}

func TestOwnershipIsEnforcedAs404(t *testing.T) {
	f := newFixture(t)
	id := decode[session](t, f.do(alice, "POST", "/api/sessions", `{"name":"mine"}`)).ID
	path := "/api/sessions/" + id

	for _, c := range sessionRoutes {
		for _, stranger := range []auth.User{bob, {}, {Subject: "Alice@example.com"}} {
			if rec := f.do(stranger, c.method, path, c.body); rec.Code != http.StatusNotFound {
				t.Errorf("stranger %q %s %s: %d, want 404", stranger.Subject, c.method, c.body, rec.Code)
			}
		}
	}
	if s, err := f.store.Get(t.Context(), id); err != nil || s.Name != "mine" || s.State != sessions.Starting {
		t.Errorf("a stranger changed the session: %+v, %v", s, err)
	}
	if rec := f.do(alice, "GET", "/api/sessions/s-missing222", ""); rec.Code != http.StatusNotFound {
		t.Errorf("missing session: %d", rec.Code)
	}
	if rec := f.do(root, "GET", "/api/sessions/s-missing222", ""); rec.Code != http.StatusNotFound {
		t.Errorf("missing session, as admin: %d", rec.Code)
	}
}

func TestOwnerAndAdminMayDoEverything(t *testing.T) {
	for name, user := range map[string]auth.User{"owner": alice, "admin": root} {
		f := newFixture(t)
		id := decode[session](t, f.do(alice, "POST", "/api/sessions", `{"name":"mine"}`)).ID
		for _, c := range sessionRoutes {
			want := http.StatusOK
			if c.method == "DELETE" {
				want = http.StatusNoContent
			}
			if rec := f.do(user, c.method, "/api/sessions/"+id, c.body); rec.Code != want {
				t.Errorf("%s %s %s: %d, want %d", name, c.method, c.body, rec.Code, want)
			}
		}
		if _, err := f.store.Get(t.Context(), id); !errors.Is(err, sessions.ErrNotFound) {
			t.Errorf("%s: session still exists after DELETE: %v", name, err)
		}
	}
}

func TestPatchAndDelete(t *testing.T) {
	f := newFixture(t)
	id := decode[session](t, f.do(alice, "POST", "/api/sessions", `{"name":"a"}`)).ID
	path := "/api/sessions/" + id

	if rec := f.do(alice, "PATCH", path, `{"name":"renamed"}`); rec.Code != http.StatusOK ||
		decode[session](t, rec).Name != "renamed" {
		t.Errorf("rename: %d %s", rec.Code, rec.Body)
	}
	if rec := f.do(alice, "PATCH", path, `{"action":"stop"}`); rec.Code != http.StatusOK {
		t.Errorf("stop: %d", rec.Code)
	}
	// Without a controller the status never shows "suspended", so the state
	// reads as stopping; what matters is that the user's intent was recorded.
	if s, _ := f.store.Get(t.Context(), id); s.State != sessions.Stopping {
		t.Errorf("state after stop = %s", s.State)
	}
	if rec := f.do(alice, "PATCH", path, `{"action":"resume"}`); rec.Code != http.StatusOK {
		t.Errorf("resume: %d", rec.Code)
	}
	if rec := f.do(alice, "PATCH", path, `{"action":"explode"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown action: %d", rec.Code)
	}

	if rec := f.do(alice, "DELETE", path, ""); rec.Code != http.StatusNoContent {
		t.Errorf("delete: %d", rec.Code)
	}
	if rec := f.do(alice, "GET", path, ""); rec.Code != http.StatusNotFound {
		t.Errorf("GET after delete: %d", rec.Code)
	}
	if rec := f.do(alice, "DELETE", path, ""); rec.Code != http.StatusNotFound {
		t.Errorf("second delete: %d, want 404", rec.Code)
	}
}

// The cap is "list, then create": two requests must not both see room for
// one more.
func TestConcurrentCreatesRespectTheCap(t *testing.T) {
	f := newFixture(t) // cap 2
	const n = 20
	codes := make([]int, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Go(func() {
			<-start
			codes[i] = f.do(alice, "POST", "/api/sessions", `{"name":"x"}`).Code
		})
	}
	close(start)
	wg.Wait()

	count := map[int]int{}
	for _, c := range codes {
		count[c]++
	}
	if count[http.StatusCreated] != 2 || count[http.StatusConflict] != n-2 {
		t.Errorf("status codes = %v; want 2 x 201 and %d x 409", count, n-2)
	}
	if mine, err := f.store.List(t.Context(), alice.Subject); err != nil || len(mine) != 2 {
		t.Errorf("alice has %d sessions (%v), want 2", len(mine), err)
	}
	// One user at the cap does not hold anyone else up.
	if rec := f.do(bob, "POST", "/api/sessions", `{"name":"x"}`); rec.Code != http.StatusCreated {
		t.Errorf("bob's create: %d", rec.Code)
	}
}

// racing is a store through which another replica of the backend creates a
// session for the same user at the worst moment: after this replica counted
// the user's sessions, before it creates its own.
type racing struct {
	*sessions.Store
	meanwhile func()
}

func (r racing) CreateWithPolicy(ctx context.Context, name, owner string, policy *sessions.PolicySpec) (sessions.Session, error) {
	if r.meanwhile != nil {
		r.meanwhile()
	}
	return r.Store.CreateWithPolicy(ctx, name, owner, policy)
}

// The lock that keeps a user's creates apart is one replica's. A create
// through another replica is caught after the fact: the session that came
// second is deleted and its caller refused.
func TestACreateThroughAnotherReplicaDoesNotPassTheCap(t *testing.T) {
	store, _ := sessionstest.New(t)
	ctx := t.Context()
	other := func() {
		if _, err := store.Create(ctx, "from the other replica", alice.Subject); err != nil {
			t.Error(err)
		}
	}
	replica := func(meanwhile func()) http.Handler {
		mux := http.NewServeMux()
		api.New(racing{store, meanwhile}, authz.NewOwners(store, 0), sessionstest.URLs(), 2).Register(mux)
		return mux
	}
	create := func(h http.Handler) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/api/sessions", strings.NewReader(`{"name":"x"}`))
		h.ServeHTTP(rec, req.WithContext(auth.WithUser(req.Context(), alice)))
		return rec
	}
	count := func() int {
		t.Helper()
		mine, err := store.List(ctx, alice.Subject)
		if err != nil {
			t.Fatal(err)
		}
		return len(mine)
	}

	// Room for both: the other replica's create is no reason to refuse.
	if rec := create(replica(other)); rec.Code != http.StatusCreated || count() != 2 {
		t.Fatalf("with room for both: %d %s, %d sessions; want 201 and 2", rec.Code, rec.Body, count())
	}
	if err := store.Delete(ctx, firstOf(t, store, alice.Subject)); err != nil {
		t.Fatal(err)
	}

	// Room for one, and the other replica takes it.
	rec := create(replica(other))
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "session limit reached") {
		t.Fatalf("past the cap: %d %s, want 409 session limit reached", rec.Code, rec.Body)
	}
	if count() != 2 {
		t.Fatalf("alice has %d sessions, want 2: the one past the cap is deleted", count())
	}

	// Many at once, through two replicas.
	for _, id := range all(t, store, alice.Subject) {
		if err := store.Delete(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	replicas := []http.Handler{replica(nil), replica(nil)}
	const n = 20
	codes := make([]int, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { codes[i] = create(replicas[i%2]).Code })
	}
	wg.Wait()
	created := 0
	for _, code := range codes {
		switch code {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
		default:
			t.Errorf("a create answered %d", code)
		}
	}
	if got := count(); got > 2 || got != created {
		t.Errorf("alice has %d sessions after %d creates were answered 201; want the same number, and at most 2", got, created)
	}
}

func all(t *testing.T, store *sessions.Store, owner string) []string {
	t.Helper()
	mine, err := store.List(t.Context(), owner)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(mine))
	for _, s := range mine {
		ids = append(ids, s.ID)
	}
	return ids
}

func firstOf(t *testing.T, store *sessions.Store, owner string) string {
	t.Helper()
	return all(t, store, owner)[0]
}

// A PATCH is checked as a whole before any of it is applied.
func TestPatchIsAllOrNothing(t *testing.T) {
	f := newFixture(t)
	id := decode[session](t, f.do(alice, "POST", "/api/sessions", `{"name":"original"}`)).ID
	path := "/api/sessions/" + id

	for _, body := range []string{
		`{"name":"changed","action":"explode"}`,
		`{"name":"","action":"stop"}`,
		`{"name":"changed","action":"STOP"}`,
	} {
		if rec := f.do(alice, "PATCH", path, body); rec.Code != http.StatusBadRequest {
			t.Errorf("PATCH %s: %d, want 400", body, rec.Code)
		}
		if s, _ := f.store.Get(t.Context(), id); s.Name != "original" || s.State != sessions.Starting {
			t.Fatalf("PATCH %s was partly applied: %+v", body, s)
		}
	}

	rec := f.do(alice, "PATCH", path, `{"name":"changed","action":"stop"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("rename and stop: %d %s", rec.Code, rec.Body)
	}
	if s := decode[session](t, rec); s.Name != "changed" || s.State != sessions.Stopping {
		t.Errorf("after rename and stop: %+v", s)
	}
}

// A check that could not be made is not a denial and not a grant: 503, and
// nothing is done, whoever asks.
func TestAuthorizationFailureFailsClosed(t *testing.T) {
	f := newFixture(t)
	id := decode[session](t, f.do(alice, "POST", "/api/sessions", `{"name":"a"}`)).ID
	path := "/api/sessions/" + id

	f.faults.fail(errDown)
	for _, user := range []auth.User{alice, bob, root} {
		for _, c := range sessionRoutes {
			if rec := f.do(user, c.method, path, c.body); rec.Code != http.StatusServiceUnavailable {
				t.Errorf("%s %s with authorization down: %d, want 503", user.Subject, c.method, rec.Code)
			}
		}
	}
	f.faults.fail(nil)
	if s, err := f.store.Get(t.Context(), id); err != nil || s.Name != "a" || s.State != sessions.Starting {
		t.Errorf("session changed while authorization was down: %+v, %v", s, err)
	}
	if rec := f.do(alice, "GET", path, ""); rec.Code != http.StatusOK {
		t.Errorf("owner after recovery: %d, want 200", rec.Code)
	}
}

// The same, with the real check against a cluster that cannot be asked who
// owns the session.
func TestClusterFailureFailsClosed(t *testing.T) {
	f := newFixture(t)
	id := decode[session](t, f.do(alice, "POST", "/api/sessions", `{"name":"a"}`)).ID
	f.client.(*dynfake.FakeDynamicClient).PrependReactor("get", sessions.SandboxGVR.Resource,
		func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewInternalError(errors.New("etcd is down"))
		})
	for _, user := range []auth.User{alice, bob, root} {
		for _, c := range sessionRoutes {
			rec := f.do(user, c.method, "/api/sessions/"+id, c.body)
			if rec.Code != http.StatusServiceUnavailable || strings.Contains(rec.Body.String(), "etcd") {
				t.Errorf("%s %s with the cluster down: %d %s, want 503", user.Subject, c.method, rec.Code, rec.Body)
			}
		}
	}
	if all, err := f.store.ListAll(t.Context()); err != nil || len(all) != 1 || all[0].Name != "a" {
		t.Errorf("sessions after the outage: %+v, %v", all, err)
	}
}

func TestNoUserIs401(t *testing.T) {
	f := newFixture(t)
	for _, c := range []struct{ method, path string }{
		{"GET", "/api/me"}, {"GET", "/api/sessions"}, {"POST", "/api/sessions"},
		{"GET", "/api/sessions/s-aaaaaaaaaa"}, {"DELETE", "/api/sessions/s-aaaaaaaaaa"},
	} {
		rec := httptest.NewRecorder()
		f.handler.ServeHTTP(rec, httptest.NewRequest(c.method, c.path, strings.NewReader(`{"name":"x"}`)))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without a user: %d, want 401", c.method, c.path, rec.Code)
		}
	}
	if all, _ := f.store.ListAll(t.Context()); len(all) != 0 {
		t.Error("a session was created without a user")
	}
}

// Something that is not a session ID is answered like a missing session,
// without asking who owns it.
func TestMalformedIDsAre404(t *testing.T) {
	f := newFixture(t)
	fake := f.client.(*dynfake.FakeDynamicClient)
	fake.ClearActions()
	for _, id := range []string{"nope", "s-ABCDEFGHIJ", "s-aaaaaaaaaaa", "s-aaaa%2Faaaaa", "..%2Fsecrets"} {
		rec := f.do(alice, "GET", "/api/sessions/"+id, "")
		if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "session not found") {
			t.Errorf("GET %s: %d %s", id, rec.Code, rec.Body)
		}
	}
	if n := f.faults.count(); n != 0 {
		t.Errorf("%d malformed IDs were checked", n)
	}
	if n := len(fake.Actions()); n != 0 {
		t.Errorf("malformed IDs caused %d cluster requests", n)
	}
}

// Being an admin is a property of the request's identity, not something
// remembered: once it is gone, so is the access.
func TestAdminDemotion(t *testing.T) {
	f := newFixture(t)
	id := decode[session](t, f.do(alice, "POST", "/api/sessions", `{"name":"a"}`)).ID
	path := "/api/sessions/" + id
	if rec := f.do(root, "GET", path, ""); rec.Code != http.StatusOK {
		t.Fatalf("admin GET: %d", rec.Code)
	}
	demoted := auth.User{Subject: root.Subject}
	if rec := f.do(demoted, "GET", path, ""); rec.Code != http.StatusNotFound {
		t.Errorf("former admin GET: %d, want 404", rec.Code)
	}
	if got := decode[[]session](t, f.do(demoted, "GET", "/api/sessions?all=1", "")); len(got) != 0 {
		t.Errorf("former admin lists %d sessions with all=1", len(got))
	}
}

func TestCreateWithoutANameGetsAPetName(t *testing.T) {
	petName := regexp.MustCompile(`^[a-z]+-[a-z]+$`)
	for _, body := range []string{`{}`, `{"name":""}`, `{"name":"  "}`, ``} {
		f := newFixture(t)
		rec := f.do(alice, "POST", "/api/sessions", body)
		if rec.Code != http.StatusCreated {
			t.Fatalf("create %q: %d %s", body, rec.Code, rec.Body)
		}
		created := decode[session](t, rec)
		if !petName.MatchString(created.Name) {
			t.Errorf("create %q: name = %q, want adjective-animal", body, created.Name)
		}
		if s, err := f.store.Get(t.Context(), created.ID); err != nil || s.Name != created.Name {
			t.Errorf("create %q: stored = %+v, %v", body, s, err)
		}
	}
}

func TestSuppliedNamesAreKeptAndStillValidated(t *testing.T) {
	f := newFixture(t)
	f.api.SetPetName(func() string { t.Error("generated a name for a session that has one"); return "x" })
	if rec := f.do(alice, "POST", "/api/sessions", `{"name":" mine "}`); rec.Code != http.StatusCreated ||
		decode[session](t, rec).Name != "mine" {
		t.Errorf("named create: %d %s", rec.Code, rec.Body)
	}
	for _, body := range []string{`{"name":"` + strings.Repeat("x", 64) + `"}`, `{"name":"a\u0000b"}`, `{`, `[]`} {
		if rec := f.do(alice, "POST", "/api/sessions", body); rec.Code != http.StatusBadRequest {
			t.Errorf("create %q: %d %s, want 400", body, rec.Code, rec.Body)
		}
	}
}

func TestGeneratedNameAvoidsTheUsersOwn(t *testing.T) {
	f := newFixture(t)
	names := []string{"brave-otter", "brave-otter", "calm-heron"}
	next := func() string {
		n := names[0]
		if len(names) > 1 {
			names = names[1:]
		}
		return n
	}
	f.api.SetPetName(next)
	first := decode[session](t, f.do(alice, "POST", "/api/sessions", `{}`))
	second := decode[session](t, f.do(alice, "POST", "/api/sessions", `{}`))
	if first.Name != "brave-otter" || second.Name != "calm-heron" {
		t.Errorf("names = %q, %q", first.Name, second.Name)
	}

	// Another user's sessions do not count, and a generator that only ever
	// repeats itself still gets a session made.
	f.api.SetPetName(func() string { return "brave-otter" })
	if s := decode[session](t, f.do(bob, "POST", "/api/sessions", `{}`)); s.Name != "brave-otter" {
		t.Errorf("bob's name = %q", s.Name)
	}
	if rec := f.do(bob, "POST", "/api/sessions", `{}`); rec.Code != http.StatusCreated ||
		decode[session](t, rec).Name != "brave-otter" {
		t.Errorf("exhausted tries: %d %s", rec.Code, rec.Body)
	}
}
