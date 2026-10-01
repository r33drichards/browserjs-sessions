// Package api is the REST API the UI calls.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/r33drichards/browserjs-sessions/backend/internal/auth"
	"github.com/r33drichards/browserjs-sessions/backend/internal/authz"
	"github.com/r33drichards/browserjs-sessions/backend/internal/sessions"
)

// How long a pushed admin flag is trusted before it is pushed again.
const adminSyncTTL = time.Minute

type API struct {
	store *sessions.Store
	authz authz.Authorizer
	cap   int
	now   func() time.Time

	// Both locks are per user. They are enough because there is one backend
	// replica; more would need the cap enforced cluster-side.
	creating keyedMutex // the cap is "list, then create"
	syncing  keyedMutex // one admin-flag push at a time

	mu        sync.Mutex
	adminSeen map[string]adminFlag // last admin flag pushed to the authorizer, per user
}

type adminFlag struct {
	admin   bool
	expires time.Time
}

func New(store *sessions.Store, az authz.Authorizer, maxPerUser int) *API {
	return &API{store: store, authz: az, cap: maxPerUser, now: time.Now, adminSeen: map[string]adminFlag{}}
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
	mux.HandleFunc("GET /api/sessions", a.user(a.list))
	mux.HandleFunc("POST /api/sessions", a.user(a.create))
	mux.HandleFunc("GET /api/sessions/{id}", a.session(authz.View, a.get))
	mux.HandleFunc("PATCH /api/sessions/{id}", a.session(authz.Manage, a.patch))
	mux.HandleFunc("DELETE /api/sessions/{id}", a.session(authz.Manage, a.delete))
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

// user resolves the caller and keeps the authorizer's admins group in step
// with the token's admin role.
func (a *API) user(next userHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, ok := auth.UserFrom(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, "not signed in")
			return
		}
		if err := a.SyncAdmin(r, u); err != nil {
			writeError(w, http.StatusServiceUnavailable, "authorization unavailable")
			return
		}
		next(w, r, u)
	}
}

// SyncAdmin pushes the user's admin flag to the authorizer when it changes,
// and again once what was pushed is older than adminSyncTTL. Pushes for one
// user happen one at a time, so what is remembered here is what the
// authorizer was last told.
func (a *API) SyncAdmin(r *http.Request, u auth.User) error {
	unlock := a.syncing.lock(u.Subject)
	defer unlock()

	now := a.now()
	a.mu.Lock()
	seen, known := a.adminSeen[u.Subject]
	a.mu.Unlock()
	if known && seen.admin == u.Admin && now.Before(seen.expires) {
		return nil
	}
	err := a.authz.SetAdmin(r.Context(), u.Subject, u.Admin)

	a.mu.Lock()
	defer a.mu.Unlock()
	for subject, flag := range a.adminSeen { // drop users who have not been back
		if !now.Before(flag.expires) {
			delete(a.adminSeen, subject)
		}
	}
	if err != nil {
		// What the authorizer holds is now unknown: push again next time.
		delete(a.adminSeen, u.Subject)
		slog.Error("admin sync failed", "user", u.Subject, "err", err)
		return err
	}
	a.adminSeen[u.Subject] = adminFlag{admin: u.Admin, expires: now.Add(adminSyncTTL)}
	return nil
}

type sessionHandler func(w http.ResponseWriter, r *http.Request, id string)

// session additionally requires permission p on {id}. Denied and missing
// both answer 404 so session IDs don't leak.
func (a *API) session(p authz.Permission, next sessionHandler) http.HandlerFunc {
	return a.user(func(w http.ResponseWriter, r *http.Request, u auth.User) {
		id := r.PathValue("id")
		if !sessions.ValidID(id) {
			writeError(w, http.StatusNotFound, "session not found")
			return
		}
		allowed, err := a.authz.Check(r.Context(), u.Subject, id, p)
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
	writeJSON(w, http.StatusOK, list)
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
	s, err := a.store.Create(r.Context(), body.Name, u.Subject)
	if err != nil {
		a.storeError(w, err)
		return
	}
	if err := a.authz.AddSession(r.Context(), s.ID, u.Subject); err != nil {
		// A session nobody is allowed to reach is worse than none. The
		// request may be why this failed (the caller hung up), so the
		// rollback must not depend on it.
		slog.Error("recording session owner failed; removing the session", "session", s.ID, "err", err)
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Second)
		defer cancel()
		if err := a.store.Delete(ctx, s.ID); err != nil && !errors.Is(err, sessions.ErrNotFound) {
			slog.Error("create rollback failed: session left with no owner relation", "session", s.ID, "owner", u.Subject, "err", err)
		}
		writeError(w, http.StatusServiceUnavailable, "authorization unavailable")
		return
	}
	writeJSON(w, http.StatusCreated, s)
}

func (a *API) get(w http.ResponseWriter, r *http.Request, id string) {
	s, err := a.store.Get(r.Context(), id)
	if err != nil {
		a.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s)
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
	if err := a.authz.RemoveSession(r.Context(), id); err != nil {
		// The session is gone; the reconcile loop (Task 12) clears the leftover.
		slog.Warn("removing session from the authorizer failed; left for reconcile", "session", id, "err", err)
		w.Header().Set("X-Authz-Cleanup", "deferred")
	}
	w.WriteHeader(http.StatusNoContent)
}
