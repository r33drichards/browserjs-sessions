package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/r33drichards/browserjs-sessions/backend/internal/api"
	"github.com/r33drichards/browserjs-sessions/backend/internal/auth"
	"github.com/r33drichards/browserjs-sessions/backend/internal/authz"
	"github.com/r33drichards/browserjs-sessions/backend/internal/sessions"
	"github.com/r33drichards/browserjs-sessions/backend/internal/sessions/sessionstest"
)

type fixture struct {
	t       *testing.T
	handler http.Handler
	api     *api.API
	store   *sessions.Store
	authz   *authz.Memory
	faults  *faulty
}

// faulty is an authorizer that can be made to fail, or to do something in
// the middle of a call.
type faulty struct {
	*authz.Memory
	mu                                    sync.Mutex
	checkErr, addErr, removeErr, adminErr error
	checks, adminCalls                    int
	onAddSession                          func()
	onSetAdmin                            func(userID string, admin bool)
}

func (f *faulty) fail(err *error, with error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	*err = with
}

func (f *faulty) Check(ctx context.Context, userID, sessionID string, p authz.Permission) (bool, error) {
	f.mu.Lock()
	f.checks++
	err := f.checkErr
	f.mu.Unlock()
	if err != nil {
		return false, err
	}
	return f.Memory.Check(ctx, userID, sessionID, p)
}

func (f *faulty) AddSession(ctx context.Context, sessionID, ownerID string) error {
	f.mu.Lock()
	err, hook := f.addErr, f.onAddSession
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	if err != nil {
		return err
	}
	return f.Memory.AddSession(ctx, sessionID, ownerID)
}

func (f *faulty) RemoveSession(ctx context.Context, sessionID string) error {
	f.mu.Lock()
	err := f.removeErr
	f.mu.Unlock()
	if err != nil {
		return err
	}
	return f.Memory.RemoveSession(ctx, sessionID)
}

func (f *faulty) SetAdmin(ctx context.Context, userID string, admin bool) error {
	f.mu.Lock()
	f.adminCalls++
	err, hook := f.adminErr, f.onSetAdmin
	f.mu.Unlock()
	if hook != nil {
		hook(userID, admin)
	}
	if err != nil {
		return err
	}
	return f.Memory.SetAdmin(ctx, userID, admin)
}

func (f *faulty) count(n *int) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return *n
}

var errDown = errors.New("authorizer is down")

func newFixture(t *testing.T) *fixture {
	store, _ := sessionstest.New(t)
	az := &faulty{Memory: authz.NewMemory()}
	mux := http.NewServeMux()
	a := api.New(store, az, 2)
	a.Register(mux)
	return &fixture{t: t, handler: mux, api: a, store: store, authz: az.Memory, faults: az}
}

// do performs a request as user, bypassing token verification.
func (f *fixture) do(user auth.User, method, path, body string) *httptest.ResponseRecorder {
	f.t.Helper()
	return f.doCtx(f.t.Context(), user, method, path, body)
}

func (f *fixture) doCtx(ctx context.Context, user auth.User, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(auth.WithUser(ctx, user))
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

var (
	alice = auth.User{Subject: "alice"}
	bob   = auth.User{Subject: "bob"}
	root  = auth.User{Subject: "root", Admin: true}
)

func TestCreateListAndCap(t *testing.T) {
	f := newFixture(t)

	rec := f.do(alice, "POST", "/api/sessions", `{"name":"one"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	created := decode[sessions.Session](t, rec)
	if created.Name != "one" || created.Owner != "alice" {
		t.Errorf("created = %+v", created)
	}
	// Creating registers ownership with the authorizer.
	if ok, _ := f.authz.Check(t.Context(), "alice", created.ID, authz.Manage); !ok {
		t.Error("owner relation not recorded")
	}

	if rec := f.do(alice, "POST", "/api/sessions", `{"name":""}`); rec.Code != http.StatusBadRequest {
		t.Errorf("empty name: %d", rec.Code)
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
	if got := decode[[]sessions.Session](t, f.do(alice, "GET", "/api/sessions", "")); len(got) != 2 {
		t.Errorf("alice sees %d sessions, want 2", len(got))
	}
	// all=1 is honoured for admins only.
	if got := decode[[]sessions.Session](t, f.do(alice, "GET", "/api/sessions?all=1", "")); len(got) != 2 {
		t.Errorf("non-admin all=1 returned %d", len(got))
	}
	if got := decode[[]sessions.Session](t, f.do(root, "GET", "/api/sessions?all=1", "")); len(got) != 3 {
		t.Errorf("admin all=1 returned %d, want 3", len(got))
	}
}

func TestOwnershipIsEnforcedAs404(t *testing.T) {
	f := newFixture(t)
	id := decode[sessions.Session](t, f.do(alice, "POST", "/api/sessions", `{"name":"mine"}`)).ID
	path := "/api/sessions/" + id

	for _, c := range []struct{ method, body string }{
		{"GET", ""}, {"PATCH", `{"name":"x"}`}, {"PATCH", `{"action":"stop"}`}, {"DELETE", ""},
	} {
		if rec := f.do(bob, c.method, path, c.body); rec.Code != http.StatusNotFound {
			t.Errorf("stranger %s: %d, want 404", c.method, rec.Code)
		}
	}
	if rec := f.do(root, "GET", path, ""); rec.Code != http.StatusOK {
		t.Errorf("admin GET: %d", rec.Code)
	}
	if rec := f.do(alice, "GET", "/api/sessions/s-missing000", ""); rec.Code != http.StatusNotFound {
		t.Errorf("missing session: %d", rec.Code)
	}
}

func TestPatchAndDelete(t *testing.T) {
	f := newFixture(t)
	id := decode[sessions.Session](t, f.do(alice, "POST", "/api/sessions", `{"name":"a"}`)).ID
	path := "/api/sessions/" + id

	if rec := f.do(alice, "PATCH", path, `{"name":"renamed"}`); rec.Code != http.StatusOK ||
		decode[sessions.Session](t, rec).Name != "renamed" {
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
	if ok, _ := f.authz.Check(t.Context(), "alice", id, authz.View); ok {
		t.Error("owner relation survived delete")
	}
	if rec := f.do(alice, "GET", path, ""); rec.Code != http.StatusNotFound {
		t.Errorf("GET after delete: %d", rec.Code)
	}
}

func TestAdminMembershipIsSynced(t *testing.T) {
	f := newFixture(t)
	id := decode[sessions.Session](t, f.do(alice, "POST", "/api/sessions", `{"name":"a"}`)).ID
	// root has never been seen; its token says admin, so the first request
	// must register that before the check.
	if rec := f.do(root, "DELETE", "/api/sessions/"+id, ""); rec.Code != http.StatusNoContent {
		t.Errorf("admin delete: %d", rec.Code)
	}
}

// A viewer can look at a session but not change it. This is what catches a
// route gated on the wrong permission.
func TestViewerCanSeeButNotChange(t *testing.T) {
	f := newFixture(t)
	id := decode[sessions.Session](t, f.do(alice, "POST", "/api/sessions", `{"name":"mine"}`)).ID
	path := "/api/sessions/" + id
	if err := f.authz.AddViewer(t.Context(), id, "bob"); err != nil {
		t.Fatal(err)
	}

	if rec := f.do(bob, "GET", path, ""); rec.Code != http.StatusOK {
		t.Errorf("viewer GET: %d, want 200", rec.Code)
	}
	for _, c := range []struct{ method, body string }{
		{"PATCH", `{"name":"x"}`}, {"PATCH", `{"action":"stop"}`}, {"DELETE", ""},
	} {
		if rec := f.do(bob, c.method, path, c.body); rec.Code != http.StatusNotFound {
			t.Errorf("viewer %s %s: %d, want 404", c.method, c.body, rec.Code)
		}
	}
	if s, err := f.store.Get(t.Context(), id); err != nil || s.Name != "mine" || s.State == sessions.Stopping {
		t.Errorf("viewer changed the session: %+v, %v", s, err)
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
	if mine, err := f.store.List(t.Context(), "alice"); err != nil || len(mine) != 2 {
		t.Errorf("alice has %d sessions (%v), want 2", len(mine), err)
	}
	// One user at the cap does not hold anyone else up.
	if rec := f.do(bob, "POST", "/api/sessions", `{"name":"x"}`); rec.Code != http.StatusCreated {
		t.Errorf("bob's create: %d", rec.Code)
	}
}

// A PATCH is checked as a whole before any of it is applied.
func TestPatchIsAllOrNothing(t *testing.T) {
	f := newFixture(t)
	id := decode[sessions.Session](t, f.do(alice, "POST", "/api/sessions", `{"name":"original"}`)).ID
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
	if s := decode[sessions.Session](t, rec); s.Name != "changed" || s.State != sessions.Stopping {
		t.Errorf("after rename and stop: %+v", s)
	}
}

// If ownership cannot be recorded the Sandbox is removed again, even when
// the reason is that the caller has gone away.
func TestCreateRollsBackWithoutAnOwnerRelation(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	f.faults.addErr = errDown
	f.faults.onAddSession = cancel // the client disconnects mid-request

	rec := f.doCtx(ctx, alice, "POST", "/api/sessions", `{"name":"orphan"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("create: %d, want 503", rec.Code)
	}
	if all, err := f.store.ListAll(t.Context()); err != nil || len(all) != 0 {
		t.Errorf("a session nobody can reach was left behind: %+v, %v", all, err)
	}
}

func TestAuthorizerFailures(t *testing.T) {
	f := newFixture(t)
	id := decode[sessions.Session](t, f.do(alice, "POST", "/api/sessions", `{"name":"a"}`)).ID
	path := "/api/sessions/" + id

	// A failed check is not a denial and not a grant: 503, nothing done.
	f.faults.fail(&f.faults.checkErr, errDown)
	for _, c := range []struct{ method, body string }{{"GET", ""}, {"PATCH", `{"name":"x"}`}, {"DELETE", ""}} {
		if rec := f.do(alice, c.method, path, c.body); rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s with the authorizer down: %d, want 503", c.method, rec.Code)
		}
	}
	f.faults.fail(&f.faults.checkErr, nil)
	if s, err := f.store.Get(t.Context(), id); err != nil || s.Name != "a" {
		t.Errorf("session changed while the authorizer was down: %+v, %v", s, err)
	}

	// The admin sync failing blocks the request, and is retried on the next.
	f.faults.fail(&f.faults.adminErr, errDown)
	if rec := f.do(root, "GET", path, ""); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("admin sync failing: %d, want 503", rec.Code)
	}
	f.faults.fail(&f.faults.adminErr, nil)
	if rec := f.do(root, "GET", path, ""); rec.Code != http.StatusOK {
		t.Errorf("admin after the authorizer recovered: %d, want 200", rec.Code)
	}

	// Deleting succeeds even if the relation cannot be removed right now.
	f.faults.fail(&f.faults.removeErr, errDown)
	if rec := f.do(alice, "DELETE", path, ""); rec.Code != http.StatusNoContent {
		t.Errorf("delete with cleanup failing: %d, want 204", rec.Code)
	}
	if _, err := f.store.Get(t.Context(), id); !errors.Is(err, sessions.ErrNotFound) {
		t.Errorf("session still exists: %v", err)
	}
}

func TestNoUserIs401(t *testing.T) {
	f := newFixture(t)
	for _, c := range []struct{ method, path string }{
		{"GET", "/api/sessions"}, {"POST", "/api/sessions"}, {"GET", "/api/sessions/s-aaaaaaaaaa"}, {"DELETE", "/api/sessions/s-aaaaaaaaaa"},
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
// without asking the authorizer or the cluster about it.
func TestMalformedIDsAre404(t *testing.T) {
	f := newFixture(t)
	for _, id := range []string{"nope", "s-ABCDEFGHIJ", "s-aaaaaaaaaaa", "s-aaaa%2Faaaaa", "..%2Fsecrets"} {
		rec := f.do(alice, "GET", "/api/sessions/"+id, "")
		if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "session not found") {
			t.Errorf("GET %s: %d %s", id, rec.Code, rec.Body)
		}
	}
	if n := f.faults.count(&f.faults.checks); n != 0 {
		t.Errorf("the authorizer was asked about %d malformed IDs", n)
	}
}

func TestAdminDemotion(t *testing.T) {
	f := newFixture(t)
	id := decode[sessions.Session](t, f.do(alice, "POST", "/api/sessions", `{"name":"a"}`)).ID
	path := "/api/sessions/" + id
	if rec := f.do(root, "GET", path, ""); rec.Code != http.StatusOK {
		t.Fatalf("admin GET: %d", rec.Code)
	}
	// The same user's next token no longer carries the admin role.
	demoted := auth.User{Subject: "root"}
	if rec := f.do(demoted, "GET", path, ""); rec.Code != http.StatusNotFound {
		t.Errorf("former admin GET: %d, want 404", rec.Code)
	}
	if got := decode[[]sessions.Session](t, f.do(demoted, "GET", "/api/sessions?all=1", "")); len(got) != 0 {
		t.Errorf("former admin lists %d sessions with all=1", len(got))
	}
}

// Two requests for one user must not push that user's admin flag at the
// same time: whichever finished last would decide the directory while the
// other decided what the backend remembers pushing.
func TestAdminSyncIsSerialisedPerUser(t *testing.T) {
	f := newFixture(t)
	var mu sync.Mutex
	inFlight, worst := map[string]int{}, 0
	f.faults.onSetAdmin = func(userID string, _ bool) {
		mu.Lock()
		inFlight[userID]++
		worst = max(worst, inFlight[userID])
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		mu.Lock()
		inFlight[userID]--
		mu.Unlock()
	}
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			f.do(auth.User{Subject: "root", Admin: i%2 == 0}, "GET", "/api/sessions", "")
		})
	}
	wg.Wait()
	if worst != 1 {
		t.Errorf("%d admin updates for one user ran at once, want 1", worst)
	}
	// What the backend believes it pushed is what the directory holds: one
	// more request with either flag leaves the two in step.
	f.faults.onSetAdmin = nil
	id := decode[sessions.Session](t, f.do(alice, "POST", "/api/sessions", `{"name":"a"}`)).ID
	if rec := f.do(auth.User{Subject: "root"}, "GET", "/api/sessions/"+id, ""); rec.Code != http.StatusNotFound {
		t.Errorf("non-admin token after the scramble: %d, want 404", rec.Code)
	}
	if rec := f.do(root, "GET", "/api/sessions/"+id, ""); rec.Code != http.StatusOK {
		t.Errorf("admin token after the scramble: %d, want 200", rec.Code)
	}
}

// The remembered admin flag expires, so a directory changed behind the
// backend's back (another replica, an operator) is put right within a minute.
func TestAdminSyncIsRepeatedAfterAWhile(t *testing.T) {
	f := newFixture(t)
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	f.api.SetClock(func() time.Time { return now })
	id := decode[sessions.Session](t, f.do(alice, "POST", "/api/sessions", `{"name":"a"}`)).ID
	path := "/api/sessions/" + id

	if rec := f.do(bob, "GET", path, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("stranger: %d", rec.Code)
	}
	calls := f.faults.count(&f.faults.adminCalls)
	// Out of band, bob is made an admin in the directory.
	_ = f.authz.SetAdmin(t.Context(), "bob", true)
	now = now.Add(30 * time.Second)
	f.do(bob, "GET", path, "")
	if n := f.faults.count(&f.faults.adminCalls); n != calls {
		t.Errorf("admin flag pushed again after 30s (%d calls): it should be remembered", n-calls)
	}
	now = now.Add(31 * time.Second)
	if rec := f.do(bob, "GET", path, ""); rec.Code != http.StatusNotFound {
		t.Errorf("bob's token is not an admin's, but he still got %d a minute later", rec.Code)
	}
}
