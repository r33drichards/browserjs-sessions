package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/r33drichards/browserjs-sessions/backend/internal/api"
	"github.com/r33drichards/browserjs-sessions/backend/internal/auth"
	"github.com/r33drichards/browserjs-sessions/backend/internal/authz"
	"github.com/r33drichards/browserjs-sessions/backend/internal/sessions"
	"github.com/r33drichards/browserjs-sessions/backend/internal/sessions/sessionstest"
)

type fixture struct {
	t       *testing.T
	handler http.Handler
	store   *sessions.Store
	authz   *authz.Memory
}

func newFixture(t *testing.T) *fixture {
	store, _ := sessionstest.New(t)
	az := authz.NewMemory()
	mux := http.NewServeMux()
	api.New(store, az, 2).Register(mux)
	return &fixture{t: t, handler: mux, store: store, authz: az}
}

// do performs a request as user, bypassing token verification.
func (f *fixture) do(user auth.User, method, path, body string) *httptest.ResponseRecorder {
	f.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req = req.WithContext(auth.WithUser(req.Context(), user))
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
	f.do(alice, "POST", "/api/sessions", `{"name":"two"}`)
	if rec := f.do(alice, "POST", "/api/sessions", `{"name":"three"}`); rec.Code != http.StatusConflict {
		t.Errorf("over cap: %d, want 409", rec.Code)
	}

	f.do(bob, "POST", "/api/sessions", `{"name":"bobs"}`)
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
