package auth

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/r33drichards/browserjs-sessions/backend/internal/hosts"
)

// The scopes an API token may carry.
const (
	ScopeSessionsRead  = "sessions:read"
	ScopeSessionsWrite = "sessions:write"
	ScopePoliciesRead  = "policies:read"
	ScopePoliciesWrite = "policies:write"
)

// Scopes is every scope there is.
var Scopes = []string{ScopeSessionsRead, ScopeSessionsWrite, ScopePoliciesRead, ScopePoliciesWrite}

// Has reports whether the token carries scope.
func (t *TokenInfo) Has(scope string) bool {
	for _, s := range t.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

// ErrInvalidToken is a TokenVerifier's answer for every token that is not
// good, whatever is wrong with it: malformed, unknown, revoked or expired.
var ErrInvalidToken = errors.New("invalid token")

// TokenVerifier checks an API token (internal/tokens does).
type TokenVerifier interface {
	// VerifyToken returns the user the token acts as and what it may do.
	// The error is ErrInvalidToken for a token that is not good, and
	// anything else when the token could not be checked.
	VerifyToken(ctx context.Context, token string) (owner string, info TokenInfo, err error)
}

// AllowList is who may use the product at all: the same email addresses as
// the policy of Pomerium's routes. Pomerium is not asked on the API host, so
// the backend has to know the list itself.
type AllowList map[string]bool

func NewAllowList(emails []string) AllowList {
	list := AllowList{}
	for _, email := range emails {
		if email = normalEmail(email); email != "" {
			list[email] = true
		}
	}
	return list
}

func (l AllowList) Allows(email string) bool { return l[normalEmail(email)] }

// SameHost reports whether a request's Host is host, a configured "name" or
// "name:port". The ports must agree only when both sides state one.
func SameHost(requestHost, host string) bool {
	name, port := hosts.Split(host)
	reqName, reqPort := hosts.Split(requestHost)
	return name != "" && reqName == name && (reqPort == port || reqPort == "" || port == "")
}

// apiRoutes is everything the API host serves, with the scope each needs
// ("" for any token). What is not listed does not exist there: the token
// endpoints, VNC tickets, file transfer, the UI.
//
// The policy routes are internal/policy's. It checks their scopes itself,
// from User.Token; the ones here are the same rule, applied first.
var apiRoutes = []struct{ pattern, scope string }{
	{"GET /v1/me", ""},
	{"GET /v1/sessions", ScopeSessionsRead},
	{"POST /v1/sessions", ScopeSessionsWrite},
	{"GET /v1/sessions/{id}", ScopeSessionsRead},
	{"PATCH /v1/sessions/{id}", ScopeSessionsWrite},
	{"DELETE /v1/sessions/{id}", ScopeSessionsWrite},
	{"GET /v1/sessions/{id}/policy", ScopePoliciesRead},
	{"PUT /v1/sessions/{id}/policy", ScopePoliciesWrite},
	{"DELETE /v1/sessions/{id}/policy", ScopePoliciesWrite},
	{"PUT /v1/sessions/{id}/policy/management", ScopePoliciesWrite},
	{"POST /v1/policies/validate", ""},
	{"POST /v1/policies/evaluate", ""},
	{"GET /v1/policy-schema.json", ""},
	{"GET /v1/policy-presets", ""},
}

// APIHost is the handler of the API host (api.<domain>), for clients that
// cannot sign in through Pomerium. Pomerium passes requests to this host
// through without asking who is calling, so nothing here trusts a cookie or
// an assertion: the only credential is "Authorization: Bearer <API token>".
type APIHost struct {
	tokens  TokenVerifier
	allowed AllowList
	limiter *FailureLimiter
	api     http.Handler
	routes  *http.ServeMux
}

// NewAPIHost serves /v1/... by handing the request to api as /api/..., with
// the token's owner as its caller. api must be behind Middleware, which lets
// such a request through.
func NewAPIHost(tokens TokenVerifier, allowed AllowList, limiter *FailureLimiter, api http.Handler) *APIHost {
	h := &APIHost{tokens: tokens, allowed: allowed, limiter: limiter, api: api, routes: http.NewServeMux()}
	for _, route := range apiRoutes {
		h.routes.Handle(route.pattern, scoped(route.scope))
	}
	return h
}

// scoped is a route's entry in the table: all it holds is the scope.
type scoped string

func (scoped) ServeHTTP(http.ResponseWriter, *http.Request) {}

func apiError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// bearer is the token of "Authorization: Bearer <token>".
func bearer(r *http.Request) (string, bool) {
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return "", false
	}
	scheme, token, ok := strings.Cut(values[0], " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	return token, token != ""
}

// clientAddr is the address a request came from, as the proxy in front saw
// it. Pomerium appends that to X-Forwarded-For, so the last entry is its
// own word; the ones before it are whatever the client sent.
func clientAddr(r *http.Request) string {
	if values := r.Header.Values("X-Forwarded-For"); len(values) > 0 {
		list := strings.Split(values[len(values)-1], ",")
		if addr := strings.TrimSpace(list[len(list)-1]); addr != "" {
			return addr
		}
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

func (h *APIHost) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	// Only /v1/, spelled plainly: no redirects to a clean path, no encoded
	// separators.
	if !strings.HasPrefix(p, "/v1/") || path.Clean(p) != p || r.URL.RawPath != "" {
		apiError(w, http.StatusNotFound, "not found")
		return
	}
	addr := clientAddr(r)
	if wait, blocked := h.limiter.Blocked(addr); blocked {
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
		apiError(w, http.StatusTooManyRequests, "too many failed attempts")
		return
	}
	u, err := h.authenticate(r)
	if errors.Is(err, ErrInvalidToken) {
		// One answer for every kind of bad token: the caller does not learn
		// whether it was unknown, expired, revoked, or its owner's access
		// was withdrawn.
		h.limiter.Failed(addr)
		w.Header().Set("WWW-Authenticate", "Bearer")
		apiError(w, http.StatusUnauthorized, "invalid token")
		return
	}
	if err != nil {
		if r.Context().Err() == nil { // not just the caller hanging up
			slog.Error("API token check failed", "err", err)
		}
		apiError(w, http.StatusServiceUnavailable, "token check unavailable")
		return
	}
	route, pattern := h.routes.Handler(r)
	if pattern == "" {
		apiError(w, http.StatusNotFound, "not found")
		return
	}
	if scope := string(route.(scoped)); scope != "" && !u.Token.Has(scope) {
		apiError(w, http.StatusForbidden, "this token lacks the scope "+scope)
		return
	}

	inner := r.Clone(WithUser(r.Context(), u))
	inner.URL.Path = "/api" + strings.TrimPrefix(p, "/v1")
	// Nothing past this point may take the caller for anyone else.
	inner.Header.Del(AssertionHeader)
	inner.Header.Del("Authorization")
	inner.Header.Del("Cookie")
	h.api.ServeHTTP(w, inner)
}

// authenticate establishes who a request's token acts as. A token never
// makes an admin, even an admin's.
func (h *APIHost) authenticate(r *http.Request) (User, error) {
	token, ok := bearer(r)
	if !ok || len(h.allowed) == 0 {
		return User{}, ErrInvalidToken
	}
	owner, info, err := h.tokens.VerifyToken(r.Context(), token)
	if err != nil {
		return User{}, err
	}
	// Removing a user from the list must end their API access at once, not
	// when their tokens expire.
	if !h.allowed.Allows(owner) {
		return User{}, ErrInvalidToken
	}
	return User{Subject: normalEmail(owner), Name: normalEmail(owner), Token: &info}, nil
}

// FailureLimiter slows down guessing: it counts each source address's
// failed attempts up to limit, forgets one every interval, and blocks an
// address until its count is back under the limit by a whole attempt. So an
// address gets limit failures at once and one more per interval after that.
type FailureLimiter struct {
	limit    float64
	interval time.Duration
	now      func() time.Time

	mu       sync.Mutex
	failures map[string]*failures
}

type failures struct {
	count float64
	at    time.Time // when count was right
}

// How many addresses a FailureLimiter remembers before it starts over.
const limiterAddresses = 10000

func NewFailureLimiter(limit int, interval time.Duration, now func() time.Time) *FailureLimiter {
	return &FailureLimiter{limit: float64(limit), interval: interval, now: now, failures: map[string]*failures{}}
}

// current is addr's count now, with what has been forgotten since taken off.
func (l *FailureLimiter) current(addr string) *failures {
	f := l.failures[addr]
	if f == nil {
		return nil
	}
	now := l.now()
	f.count = max(0, f.count-float64(now.Sub(f.at))/float64(l.interval))
	f.at = now
	if f.count == 0 {
		delete(l.failures, addr)
		return nil
	}
	return f
}

// Blocked reports whether addr has failed too often, and for how long yet.
func (l *FailureLimiter) Blocked(addr string) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	f := l.current(addr)
	if f == nil || f.count <= l.limit-1 {
		return 0, false
	}
	return time.Duration((f.count - l.limit + 1) * float64(l.interval)), true
}

// Failed records a failed attempt from addr.
func (l *FailureLimiter) Failed(addr string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	f := l.current(addr)
	if f == nil {
		if len(l.failures) >= limiterAddresses {
			// Out of room: drop what has been forgotten, or failing that
			// everything, rather than grow without bound.
			for a := range l.failures {
				l.current(a)
			}
			if len(l.failures) >= limiterAddresses {
				clear(l.failures)
			}
		}
		f = &failures{at: l.now()}
		l.failures[addr] = f
	}
	f.count = min(l.limit, f.count+1)
}
