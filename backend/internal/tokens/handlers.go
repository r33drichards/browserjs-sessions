package tokens

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/r33drichards/computer-use/backend/internal/auth"
	"github.com/r33drichards/computer-use/backend/internal/sessions"
)

const (
	// MaxPerUser is how many tokens a user may have at a time.
	MaxPerUser = 20

	defaultDays = 90
	maxDays     = 365
	maxNameLen  = 64
)

// Handlers is the token page's API: /api/tokens, for a user signed in
// through Pomerium. A token cannot be used here, so a leaked token cannot
// make itself a successor.
type Handlers struct {
	store   *Store
	allowed auth.AllowList
	// tokenURL is where a token is exchanged for an access token.
	tokenURL string

	creating sync.Mutex // the cap is "list, then create"
}

// NewHandlers serves the tokens in store to the users in allowed. apiURL is
// the API host's base URL.
func NewHandlers(store *Store, allowed auth.AllowList, apiURL string) *Handlers {
	return &Handlers{store: store, allowed: allowed, tokenURL: apiURL + "/oauth/token"}
}

// Register adds the routes to mux, which must be behind auth.Middleware.
func (h *Handlers) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/tokens", h.user(h.list))
	mux.HandleFunc("POST /api/tokens", h.user(h.create))
	mux.HandleFunc("DELETE /api/tokens/{token_id}", h.user(h.revoke))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	// An answer here may hold a token.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (h *Handlers) user(next func(http.ResponseWriter, *http.Request, auth.User)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, ok := auth.UserFrom(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, "not signed in")
			return
		}
		if u.Token != nil {
			writeError(w, http.StatusForbidden, "API tokens are managed in the browser, not with a token")
			return
		}
		next(w, r, u)
	}
}

func (h *Handlers) failed(w http.ResponseWriter, err error) {
	slog.Error("cluster request failed", "err", err)
	writeError(w, http.StatusInternalServerError, "cluster request failed")
}

func (h *Handlers) list(w http.ResponseWriter, r *http.Request, u auth.User) {
	var list []Token
	var err error
	if u.Admin && r.URL.Query().Get("all") == "1" {
		list, err = h.store.ListAll(r.Context())
	} else {
		list, err = h.store.List(r.Context(), u.Subject)
	}
	if err != nil {
		h.failed(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// validName reports whether name can be shown as it is: some text, no
// control characters.
func validName(name string) bool {
	if name == "" || utf8.RuneCountInString(name) > maxNameLen || !utf8.ValidString(name) {
		return false
	}
	return strings.IndexFunc(name, unicode.IsControl) < 0
}

// validScopes returns scopes without repeats, or false if there are none or
// one of them is not a scope.
func validScopes(scopes []string) ([]string, bool) {
	var out []string
	for _, known := range auth.Scopes {
		for _, scope := range scopes {
			if scope == known {
				out = append(out, known)
				break
			}
		}
	}
	for _, scope := range scopes {
		if !(&auth.TokenInfo{Scopes: auth.Scopes}).Has(scope) {
			return nil, false
		}
	}
	return out, len(out) > 0
}

func (h *Handlers) create(w http.ResponseWriter, r *http.Request, u auth.User) {
	// A token of someone the API host would refuse is of no use to them.
	if !h.allowed.Allows(u.Subject) {
		writeError(w, http.StatusForbidden, "this account may not use the API")
		return
	}
	var body struct {
		Name   string   `json:"name"`
		Scopes []string `json:"scopes"`
		Days   *int     `json:"expires_in_days"`
		// The one session the token is for. The owner check applies all
		// the same: naming somebody else's session gives a token for nothing.
		Session string `json:"session_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "body must be JSON with a name and scopes")
		return
	}
	name := strings.TrimSpace(body.Name)
	if !validName(name) {
		writeError(w, http.StatusBadRequest, "name must be 1 to 64 characters")
		return
	}
	scopes, ok := validScopes(body.Scopes)
	if !ok {
		writeError(w, http.StatusBadRequest, "scopes must be one or more of "+strings.Join(auth.Scopes, ", "))
		return
	}
	if body.Session != "" && !sessions.ValidID(body.Session) {
		writeError(w, http.StatusBadRequest, "session_id is not a session's ID")
		return
	}
	// Every token expires.
	days := defaultDays
	if body.Days != nil {
		days = *body.Days
	}
	if days < 1 || days > maxDays {
		writeError(w, http.StatusBadRequest, "expires_in_days must be between 1 and 365")
		return
	}

	h.creating.Lock()
	defer h.creating.Unlock()
	mine, err := h.store.List(r.Context(), u.Subject)
	if err != nil {
		h.failed(w, err)
		return
	}
	live := 0
	for _, t := range mine {
		if h.store.now().Before(t.Expires) {
			live++
			continue
		}
		// An expired token is of no use; it goes when its owner makes room.
		if err := h.store.Delete(r.Context(), t.ID); err != nil {
			h.failed(w, err)
			return
		}
	}
	if live >= MaxPerUser {
		writeError(w, http.StatusConflict, "token limit reached; revoke one first")
		return
	}
	t, token, err := h.store.Create(r.Context(), u.Subject, name, scopes, body.Session, time.Duration(days)*24*time.Hour)
	if err != nil {
		h.failed(w, err)
		return
	}
	slog.Info("API token created", "token", t.ID, "owner", t.Owner, "scopes", t.Scopes, "session", t.Session, "expires", t.Expires)
	// The token is also an OAuth client: its id and itself, at token_url.
	writeJSON(w, http.StatusCreated, struct {
		Token
		Secret   string `json:"token"`
		TokenURL string `json:"token_url"`
	}{t, token, h.tokenURL})
}

// revoke deletes a token of the caller's, or of anyone's for an admin.
// Somebody else's and none at all are answered alike.
func (h *Handlers) revoke(w http.ResponseWriter, r *http.Request, u auth.User) {
	id := r.PathValue("token_id")
	t, found, err := h.store.Get(r.Context(), id)
	if err != nil {
		h.failed(w, err)
		return
	}
	if found && (t.Owner == u.Subject || u.Admin) {
		if err := h.store.Delete(r.Context(), id); err != nil {
			h.failed(w, err)
			return
		}
		slog.Info("API token revoked", "token", id, "owner", t.Owner, "by", u.Subject)
	}
	w.WriteHeader(http.StatusNoContent)
}
