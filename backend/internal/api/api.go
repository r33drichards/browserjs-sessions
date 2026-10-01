// Package api is the REST API the UI calls.
package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"

	"github.com/r33drichards/browserjs-sessions/backend/internal/auth"
	"github.com/r33drichards/browserjs-sessions/backend/internal/authz"
	"github.com/r33drichards/browserjs-sessions/backend/internal/sessions"
)

type API struct {
	store *sessions.Store
	authz authz.Checker
	urls  *sessions.URLTemplate
	cap   int

	// The lock is per user. It is enough because there is one backend
	// replica; more would need the cap enforced cluster-side.
	creating keyedMutex // the cap is "list, then create"
}

func New(store *sessions.Store, az authz.Checker, urls *sessions.URLTemplate, maxPerUser int) *API {
	return &API{store: store, authz: az, urls: urls, cap: maxPerUser}
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
	default:
		slog.Error("cluster request failed", "err", err)
		writeError(w, http.StatusInternalServerError, "cluster request failed")
	}
}

// view is a session as the API shows it.
type view struct {
	sessions.Session
	MCPURL string `json:"mcp_url"` // what an MCP client is pointed at
}

func (a *API) view(s sessions.Session) view {
	return view{Session: s, MCPURL: a.urls.MCP(s.ID)}
}

func (a *API) me(w http.ResponseWriter, _ *http.Request, u auth.User) {
	writeJSON(w, http.StatusOK, map[string]any{"email": u.Subject, "name": u.Name, "admin": u.Admin})
}

func (a *API) list(w http.ResponseWriter, r *http.Request, u auth.User) {
	var list []sessions.Session
	var err error
	if u.Admin && r.URL.Query().Get("all") == "1" {
		list, err = a.store.ListAll(r.Context())
	} else {
		list, err = a.store.List(r.Context(), u.Subject)
	}
	if err != nil {
		a.storeError(w, err)
		return
	}
	views := make([]view, 0, len(list))
	for _, s := range list {
		views = append(views, a.view(s))
	}
	writeJSON(w, http.StatusOK, views)
}

func (a *API) create(w http.ResponseWriter, r *http.Request, u auth.User) {
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "body must be JSON with a name")
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
	if len(mine) >= a.cap {
		writeError(w, http.StatusConflict, "session limit reached; delete one first")
		return
	}
	// The owner is recorded on the session itself; that is all there is to
	// who may use it.
	s, err := a.store.Create(r.Context(), body.Name, u.Subject)
	if err != nil {
		a.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, a.view(s))
}

func (a *API) get(w http.ResponseWriter, r *http.Request, id string) {
	s, err := a.store.Get(r.Context(), id)
	if err != nil {
		a.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a.view(s))
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
