// Package api is the REST API the UI calls.
package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"sync"

	"github.com/r33drichards/browserjs-sessions/backend/internal/auth"
	"github.com/r33drichards/browserjs-sessions/backend/internal/authz"
	"github.com/r33drichards/browserjs-sessions/backend/internal/sessions"
)

type API struct {
	store *sessions.Store
	authz authz.Authorizer
	cap   int

	mu        sync.Mutex
	adminSeen map[string]bool // last admin flag pushed to the authorizer, per user
}

func New(store *sessions.Store, az authz.Authorizer, maxPerUser int) *API {
	return &API{store: store, authz: az, cap: maxPerUser, adminSeen: map[string]bool{}}
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

// SyncAdmin pushes the user's admin flag to the authorizer when it changes.
func (a *API) SyncAdmin(r *http.Request, u auth.User) error {
	a.mu.Lock()
	seen, known := a.adminSeen[u.Subject]
	a.mu.Unlock()
	if known && seen == u.Admin {
		return nil
	}
	if err := a.authz.SetAdmin(r.Context(), u.Subject, u.Admin); err != nil {
		return err
	}
	a.mu.Lock()
	a.adminSeen[u.Subject] = u.Admin
	a.mu.Unlock()
	return nil
}

type sessionHandler func(w http.ResponseWriter, r *http.Request, id string)

// session additionally requires permission p on {id}. Denied and missing
// both answer 404 so session IDs don't leak.
func (a *API) session(p authz.Permission, next sessionHandler) http.HandlerFunc {
	return a.user(func(w http.ResponseWriter, r *http.Request, u auth.User) {
		id := r.PathValue("id")
		allowed, err := a.authz.Check(r.Context(), u.Subject, id, p)
		if err != nil {
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
	case errors.Is(err, sessions.ErrInvalidName):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
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
		// A session nobody is allowed to reach is worse than none.
		_ = a.store.Delete(r.Context(), s.ID)
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
	var err error
	if body.Name != nil {
		err = a.store.Rename(r.Context(), id, *body.Name)
	}
	if err == nil {
		switch body.Action {
		case "":
		case "stop":
			err = a.store.Suspend(r.Context(), id, sessions.StoppedByUser)
		case "resume":
			err = a.store.Resume(r.Context(), id)
		default:
			writeError(w, http.StatusBadRequest, `action must be "stop" or "resume"`)
			return
		}
	}
	if err != nil {
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
		w.Header().Set("X-Authz-Cleanup", "deferred")
	}
	w.WriteHeader(http.StatusNoContent)
}
