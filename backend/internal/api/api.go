// Package api is the REST API the UI calls.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	petname "github.com/dustinkirkland/golang-petname"

	"github.com/r33drichards/browserjs-sessions/backend/internal/auth"
	"github.com/r33drichards/browserjs-sessions/backend/internal/authz"
	"github.com/r33drichards/browserjs-sessions/backend/internal/policy"
	"github.com/r33drichards/browserjs-sessions/backend/internal/sessions"
)

// Store is what the API needs of the session store (a *sessions.Store).
type Store interface {
	CreateWithPolicy(ctx context.Context, name, owner string, policy *sessions.PolicySpec) (sessions.Session, error)
	Get(ctx context.Context, id string) (sessions.Session, error)
	List(ctx context.Context, owner string) ([]sessions.Session, error)
	ListAll(ctx context.Context) ([]sessions.Session, error)
	Update(ctx context.Context, id string, name *string, action string) error
	Delete(ctx context.Context, id string) error
}

type API struct {
	store Store
	authz authz.Checker
	urls  *sessions.URLTemplate
	cap   int

	// The lock is per user. It is enough because there is one backend
	// replica; more would need the cap enforced cluster-side.
	creating keyedMutex // the cap is "list, then create"

	petName func() string // names a session created without a name

	policies *policy.Handlers // nil: no session policies (see policy.go)

	billing Billing // nil: no billing (see billing.go)
}

func New(store Store, az authz.Checker, urls *sessions.URLTemplate, maxPerUser int) *API {
	return &API{store: store, authz: az, urls: urls, cap: maxPerUser, petName: petName}
}

// petName is an adjective and an animal, like "brave-otter".
func petName() string { return petname.Generate(2, "-") }

// petNameTries bounds the search for a pet name the user doesn't already have.
const petNameTries = 5

// freshName generates a name that none of the user's sessions has. Names
// need not be unique, so after a few tries a repeated one will do.
func (a *API) freshName(mine []sessions.Session) string {
	taken := make(map[string]bool, len(mine))
	for _, s := range mine {
		taken[s.Name] = true
	}
	name := a.petName()
	for range petNameTries - 1 {
		if !taken[name] {
			break
		}
		name = a.petName()
	}
	return name
}

// keyedMutex is a mutex per key. A key takes up space only while it is held
// or waited for.
type keyedMutex struct {
	mu      sync.Mutex
	entries map[string]*keyedEntry
}

type keyedEntry struct {
	mu   sync.Mutex
	refs int
}

func (k *keyedMutex) lock(key string) (unlock func()) {
	k.mu.Lock()
	if k.entries == nil {
		k.entries = map[string]*keyedEntry{}
	}
	e := k.entries[key]
	if e == nil {
		e = &keyedEntry{}
		k.entries[key] = e
	}
	e.refs++
	k.mu.Unlock()

	e.mu.Lock()
	return func() {
		e.mu.Unlock()
		k.mu.Lock()
		if e.refs--; e.refs == 0 {
			delete(k.entries, key)
		}
		k.mu.Unlock()
	}
}

func (a *API) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/me", a.user(a.me))
	mux.HandleFunc("GET /api/sessions", a.user(a.list))
	mux.HandleFunc("POST /api/sessions", a.user(a.create))
	mux.HandleFunc("GET /api/sessions/{id}", a.session(a.get))
	mux.HandleFunc("PATCH /api/sessions/{id}", a.session(a.patch))
	mux.HandleFunc("DELETE /api/sessions/{id}", a.session(a.delete))
	a.registerPolicies(mux)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

type userHandler func(w http.ResponseWriter, r *http.Request, u auth.User)

// user resolves the caller, whom auth.Middleware put on the context.
func (a *API) user(next userHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, ok := auth.UserFrom(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, "not signed in")
			return
		}
		next(w, r, u)
	}
}

type sessionHandler func(w http.ResponseWriter, r *http.Request, id string)

// session additionally requires the caller to be allowed to use {id}: its
// owner, or an admin. Denied and missing both answer 404 so session IDs
// don't leak.
func (a *API) session(next sessionHandler) http.HandlerFunc {
	return a.user(func(w http.ResponseWriter, r *http.Request, u auth.User) {
		id := r.PathValue("id")
		if !sessions.ValidID(id) {
			writeError(w, http.StatusNotFound, "session not found")
			return
		}
		allowed, err := a.authz.Allowed(r.Context(), u, id)
		if err != nil {
			slog.Error("authorization check failed", "session", id, "err", err)
			writeError(w, http.StatusServiceUnavailable, "authorization unavailable")
			return
		}
		if !allowed {
			writeError(w, http.StatusNotFound, "session not found")
			return
		}
		next(w, r, id)
	})
}

func (a *API) storeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, sessions.ErrNotFound):
		writeError(w, http.StatusNotFound, "session not found")
	case errors.Is(err, sessions.ErrInvalidName), errors.Is(err, sessions.ErrInvalidAction):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, sessions.ErrPolicyUnsupported):
		writeError(w, http.StatusConflict, "new sessions cannot be given a policy here yet")
	default:
		slog.Error("cluster request failed", "err", err)
		writeError(w, http.StatusInternalServerError, "cluster request failed")
	}
}

// view is a session as the API shows it.
type view struct {
	sessions.Session
	MCPURL string `json:"mcp_url"` // what an MCP client is pointed at
	// Policy is absent when policies are off.
	Policy *policy.Summary `json:"policy,omitempty"`
	// Absent when billing is off (billing.go).
	billed
}

func (a *API) view(ctx context.Context, s sessions.Session, p *policy.Summary) view {
	if p != nil {
		s = p.Gate(s)
	}
	return view{Session: s, MCPURL: a.urls.MCP(s.ID), Policy: p, billed: a.billed(ctx, s)}
}

func (a *API) me(w http.ResponseWriter, _ *http.Request, u auth.User) {
	writeJSON(w, http.StatusOK, map[string]any{"email": u.Subject, "name": u.Name, "admin": u.Admin})
}

func (a *API) list(w http.ResponseWriter, r *http.Request, u auth.User) {
	var list []sessions.Session
	var err error
	owner := u.Subject
	if u.Admin && r.URL.Query().Get("all") == "1" {
		owner = ""
		list, err = a.store.ListAll(r.Context())
	} else {
		list, err = a.store.List(r.Context(), u.Subject)
	}
	if err != nil {
		a.storeError(w, err)
		return
	}
	policies := a.summaries(r.Context(), list, owner)
	views := make([]view, 0, len(list))
	for _, s := range list {
		views = append(views, a.view(r.Context(), s, policies[s.ID]))
	}
	writeJSON(w, http.StatusOK, views)
}

func (a *API) create(w http.ResponseWriter, r *http.Request, u auth.User) {
	var body struct {
		Name   string        `json:"name"`
		Policy *policy.Input `json:"policy"`
	}
	// The name is optional, and so is a body that would only carry it.
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, a.maxCreateBody())).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "body must be JSON, optionally with a name")
		return
	}
	// Checked before anything is created: an invalid policy creates nothing.
	asked, ok := a.policyFor(w, r, u, body.Policy)
	if !ok {
		return
	}
	// Counting and creating must not interleave with the same user's other
	// creates, or each of them sees room for one more.
	unlock := a.creating.lock(u.Subject)
	defer unlock()
	mine, err := a.store.List(r.Context(), u.Subject)
	if err != nil {
		a.storeError(w, err)
		return
	}
	// Before anything is made, and so before any warm-pool claim.
	if err := a.mayCreate(r.Context(), u.Subject, mine); err != nil {
		a.refused(w, err)
		return
	}
	// With billing enforced the plan's limit has been applied instead.
	if !a.enforcing() && len(mine) >= a.cap {
		writeError(w, http.StatusConflict, "session limit reached; delete one first")
		return
	}
	name := body.Name
	if strings.TrimSpace(name) == "" {
		name = a.freshName(mine)
	}
	// The owner is recorded on the session itself; that is all there is to
	// who may use it.
	s, err := a.store.CreateWithPolicy(r.Context(), name, u.Subject, asked)
	if err != nil {
		a.storeError(w, err)
		return
	}
	a.created(s)
	writeJSON(w, http.StatusCreated, a.view(r.Context(), s, a.summary(r.Context(), s)))
}

func (a *API) get(w http.ResponseWriter, r *http.Request, id string) {
	s, err := a.store.Get(r.Context(), id)
	if err != nil {
		a.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a.view(r.Context(), s, a.summary(r.Context(), s)))
}

func (a *API) patch(w http.ResponseWriter, r *http.Request, id string) {
	var body struct {
		Name   *string `json:"name"`
		Action string  `json:"action"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "body must be JSON")
		return
	}
	if err := a.mayResume(r.Context(), id, body.Action); err != nil {
		a.refused(w, err)
		return
	}
	// One write, validated as a whole: a bad action must not leave a rename
	// behind.
	if err := a.store.Update(r.Context(), id, body.Name, body.Action); err != nil {
		a.storeError(w, err)
		return
	}
	a.get(w, r, id)
}

func (a *API) delete(w http.ResponseWriter, r *http.Request, id string) {
	if err := a.store.Delete(r.Context(), id); err != nil && !errors.Is(err, sessions.ErrNotFound) {
		a.storeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
