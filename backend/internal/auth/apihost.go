package auth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/url"
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
	// ScopeSessionsConnect is for a session's MCP endpoint: what an agent
	// does with the browser, as opposed to managing the session.
	ScopeSessionsConnect = "sessions:connect"
	ScopePoliciesRead    = "policies:read"
	ScopePoliciesWrite   = "policies:write"
)

// Scopes is every scope there is.
var Scopes = []string{ScopeSessionsRead, ScopeSessionsWrite, ScopeSessionsConnect, ScopePoliciesRead, ScopePoliciesWrite}

// Has reports whether the token carries scope.
func (t *TokenInfo) Has(scope string) bool {
	for _, s := range t.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

// ErrInvalidToken is a TokenVerifier's answer for every credential that is
// not good, whatever is wrong with it: malformed, unknown, revoked or
// expired.
var ErrInvalidToken = errors.New("invalid token")

// ErrInvalidScope is ExchangeToken's answer for a request for a scope the
// API token does not have.
var ErrInvalidScope = errors.New("invalid scope")

// Grant is a short-lived access token made from an API token.
type Grant struct {
	AccessToken string
	Scopes      []string
	ExpiresIn   time.Duration
}

// TokenVerifier checks the credentials of the API host (internal/tokens
// does).
type TokenVerifier interface {
	// VerifyToken returns the user a bearer token acts as and what it may
	// do. The token is an API token or an access token made from one. The
	// error is ErrInvalidToken for one that is not good, and anything else
	// when it could not be checked.
	VerifyToken(ctx context.Context, token string) (owner string, info TokenInfo, err error)
	// ExchangeToken is OAuth's client-credentials grant: the client ID is
	// an API token's id, the secret the token itself. scopes narrows the
	// grant; empty asks for all the token has. Errors as VerifyToken's, and
	// ErrInvalidScope.
	ExchangeToken(ctx context.Context, clientID, clientSecret string, scopes []string) (owner string, grant Grant, err error)
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

// apiRoutes is the API the API host serves, with the scope each route needs
// ("" for any token) and whether a token for one session may use it. What
// is not listed does not exist there: the token endpoints, VNC tickets,
// file transfer, the UI.
//
// The policy routes are internal/policy's. It checks their scopes itself,
// from User.Token; the ones here are the same rule, applied first.
var apiRoutes = []struct {
	pattern, scope string
	// unbound: the route names no session, and what it does reaches beyond
	// one, so a token for one session is refused.
	unbound bool
}{
	{pattern: "GET /v1/me"},
	{pattern: "GET /v1/sessions", scope: ScopeSessionsRead, unbound: true},
	{pattern: "POST /v1/sessions", scope: ScopeSessionsWrite, unbound: true},
	{pattern: "GET /v1/sessions/{id}", scope: ScopeSessionsRead},
	{pattern: "PATCH /v1/sessions/{id}", scope: ScopeSessionsWrite},
	{pattern: "DELETE /v1/sessions/{id}", scope: ScopeSessionsWrite},
	{pattern: "POST /v1/sessions/{id}/sleep", scope: ScopeSessionsWrite},
	{pattern: "POST /v1/sessions/{id}/wake", scope: ScopeSessionsWrite},
	{pattern: "GET /v1/sessions/{id}/policy", scope: ScopePoliciesRead},
	{pattern: "PUT /v1/sessions/{id}/policy", scope: ScopePoliciesWrite},
	{pattern: "DELETE /v1/sessions/{id}/policy", scope: ScopePoliciesWrite},
	{pattern: "PUT /v1/sessions/{id}/policy/management", scope: ScopePoliciesWrite},
	{pattern: "POST /v1/policies/validate"},
	{pattern: "POST /v1/policies/evaluate"},
	{pattern: "GET /v1/policy-presets"},
}

// tokenPath is where an API token is exchanged for an access token.
const tokenPath = "/oauth/token"

// APIHostConfig is what an APIHost is made of.
type APIHostConfig struct {
	Tokens  TokenVerifier
	Allowed AllowList
	Limiter *FailureLimiter
	// App is the server's whole handler. A request the API host lets in is
	// handed to it with the token's owner as its caller (Authenticate), as
	// the request it would have been had the caller signed in: /api/... on
	// this host for the API, and a session's own URL for its MCP endpoint.
	App http.Handler
	// ValidSessionID reports whether a path's first segment could name a
	// session, and SessionBase returns that session's base URL
	// (https://sessions.<domain>/<id>).
	ValidSessionID func(id string) bool
	SessionBase    func(id string) string
}

// APIHost is the handler of the API host (api.<domain>), for clients that
// cannot sign in through Pomerium. Pomerium passes requests to this host
// through without asking who is calling, so nothing here trusts a cookie or
// an assertion: the only credentials are an API token and an access token
// made from one, as "Authorization: Bearer".
//
// It serves three things and nothing else:
//
//	/v1/...            the API, as on the app's host under /api
//	/<id>/mcp[/...]    a session's MCP endpoint, as on the sessions' host
//	/oauth/token       an API token exchanged for a short-lived access token
type APIHost struct {
	APIHostConfig
	routes *http.ServeMux
}

func NewAPIHost(cfg APIHostConfig) *APIHost {
	h := &APIHost{APIHostConfig: cfg, routes: http.NewServeMux()}
	for i, route := range apiRoutes {
		h.routes.Handle(route.pattern, routeIndex(i))
	}
	return h
}

// routeIndex is a route's entry in the mux: its place in apiRoutes.
type routeIndex int

func (routeIndex) ServeHTTP(http.ResponseWriter, *http.Request) {}

func apiError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
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

// mcpRequest takes apart "/<id>/mcp" and "/<id>/mcp/...": the session, and
// the path as it was sent.
func (h *APIHost) mcpRequest(r *http.Request) (id string, ok bool) {
	first, rest, _ := strings.Cut(strings.TrimPrefix(r.URL.EscapedPath(), "/"), "/")
	if !h.ValidSessionID(first) || (rest != "mcp" && !strings.HasPrefix(rest, "mcp/")) {
		return "", false
	}
	return first, true
}

func (h *APIHost) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	session, isMCP := h.mcpRequest(r)
	isAPI := strings.HasPrefix(p, "/v1/") && r.URL.RawPath == ""
	// Only these, spelled plainly: no redirects to a clean path.
	if !(isAPI || isMCP || p == tokenPath) || path.Clean(p) != p {
		apiError(w, http.StatusNotFound, "not found")
		return
	}
	addr := clientAddr(r)
	if wait, blocked := h.Limiter.Blocked(addr); blocked {
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
		apiError(w, http.StatusTooManyRequests, "too many failed attempts")
		return
	}
	if p == tokenPath {
		h.exchange(w, r, addr)
		return
	}
	u, err := h.authenticate(r)
	if errors.Is(err, ErrInvalidToken) {
		// One answer for every kind of bad token: the caller does not learn
		// whether it was unknown, expired, revoked, or its owner's access
		// was withdrawn.
		h.Limiter.Failed(addr)
		w.Header().Set("WWW-Authenticate", "Bearer")
		apiError(w, http.StatusUnauthorized, "invalid token")
		return
	}
	if err != nil {
		h.unavailable(w, r, err)
		return
	}

	inner := r.Clone(WithUser(r.Context(), u))
	// Nothing past this point may take the caller for anyone else.
	inner.Header.Del(AssertionHeader)
	inner.Header.Del("Authorization")
	inner.Header.Del("Cookie")

	if isMCP {
		if !h.may(w, u, ScopeSessionsConnect, session) {
			return
		}
		// The session's own URL, so that it is the session proxy's request
		// in every respect: its routes, its checks of the path and of the
		// kind of request, its waking and idle tracking. The owner check is
		// the proxy's too.
		base, err := url.Parse(h.SessionBase(session))
		if err != nil {
			apiError(w, http.StatusNotFound, "not found")
			return
		}
		rest := strings.TrimPrefix(r.URL.EscapedPath(), "/"+session)
		inner.Host = base.Host
		inner.URL.RawPath = base.EscapedPath() + rest
		if inner.URL.Path, err = url.PathUnescape(inner.URL.RawPath); err != nil {
			apiError(w, http.StatusNotFound, "not found")
			return
		}
		h.App.ServeHTTP(w, inner)
		return
	}

	entry, pattern := h.routes.Handler(r)
	if pattern == "" {
		apiError(w, http.StatusNotFound, "not found")
		return
	}
	route := apiRoutes[entry.(routeIndex)]
	if route.unbound && u.Token.Session != "" {
		apiError(w, http.StatusForbidden, "this token is for one session")
		return
	}
	if !h.may(w, u, route.scope, sessionOf(p, pattern)) {
		return
	}
	inner.URL.Path = "/api" + strings.TrimPrefix(p, "/v1")
	h.App.ServeHTTP(w, inner)
}

// sessionOf is the {id} of an API path matched to pattern, "" if the
// pattern has none.
func sessionOf(path, pattern string) string {
	if !strings.Contains(pattern, "/v1/sessions/{id}") {
		return ""
	}
	id, _, _ := strings.Cut(strings.TrimPrefix(path, "/v1/sessions/"), "/")
	return id
}

// may reports whether u's token has scope ("" for none needed) and is not
// for a session other than session ("" for a route that names none). It
// answers the request itself when not.
func (h *APIHost) may(w http.ResponseWriter, u User, scope, session string) bool {
	if scope != "" && !u.Token.Has(scope) {
		apiError(w, http.StatusForbidden, "this token lacks the scope "+scope)
		return false
	}
	if u.Token.Session != "" && session != "" && u.Token.Session != session {
		apiError(w, http.StatusForbidden, "this token is for another session")
		return false
	}
	return true
}

func (h *APIHost) unavailable(w http.ResponseWriter, r *http.Request, err error) {
	if r.Context().Err() == nil { // not just the caller hanging up
		slog.Error("API token check failed", "err", err)
	}
	apiError(w, http.StatusServiceUnavailable, "token check unavailable")
}

// authenticate establishes who a request's token acts as. A token never
// makes an admin, even an admin's.
func (h *APIHost) authenticate(r *http.Request) (User, error) {
	token, ok := bearer(r)
	if !ok || len(h.Allowed) == 0 {
		return User{}, ErrInvalidToken
	}
	owner, info, err := h.Tokens.VerifyToken(r.Context(), token)
	if err != nil {
		return User{}, err
	}
	// Removing a user from the list must end their API access at once, not
	// when their tokens expire.
	if !h.Allowed.Allows(owner) {
		return User{}, ErrInvalidToken
	}
	return User{Subject: normalEmail(owner), Name: normalEmail(owner), Token: &info}, nil
}

// exchange is OAuth 2.0's token endpoint, for the client-credentials grant
// alone (RFC 6749, 4.4): an API token's id and the token, as HTTP Basic or
// in the form, for an access token of an hour at most.
func (h *APIHost) exchange(w http.ResponseWriter, r *http.Request, addr string) {
	oauthError := func(status int, code string) { apiError(w, status, code) }
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		oauthError(http.StatusMethodNotAllowed, "invalid_request")
		return
	}
	// The credentials are read from the body only; a query string ends up
	// in logs.
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	if mediaType, _, _ := strings.Cut(r.Header.Get("Content-Type"), ";"); !strings.EqualFold(strings.TrimSpace(mediaType), "application/x-www-form-urlencoded") {
		oauthError(http.StatusBadRequest, "invalid_request")
		return
	}
	raw, err := io.ReadAll(r.Body)
	form, formErr := url.ParseQuery(string(raw))
	if err != nil || formErr != nil {
		oauthError(http.StatusBadRequest, "invalid_request")
		return
	}
	if form.Get("grant_type") != "client_credentials" {
		oauthError(http.StatusBadRequest, "unsupported_grant_type")
		return
	}
	id, secret, basic := r.BasicAuth()
	if !basic {
		id, secret = form.Get("client_id"), form.Get("client_secret")
	}
	invalidClient := func() {
		h.Limiter.Failed(addr)
		if basic {
			w.Header().Set("WWW-Authenticate", `Basic realm="api"`)
		}
		oauthError(http.StatusUnauthorized, "invalid_client")
	}
	if id == "" || secret == "" || len(h.Allowed) == 0 {
		invalidClient()
		return
	}
	owner, grant, err := h.Tokens.ExchangeToken(r.Context(), id, secret, strings.Fields(form.Get("scope")))
	switch {
	case errors.Is(err, ErrInvalidToken), err == nil && !h.Allowed.Allows(owner):
		invalidClient()
	case errors.Is(err, ErrInvalidScope):
		oauthError(http.StatusBadRequest, "invalid_scope")
	case err != nil:
		h.unavailable(w, r, err)
	default:
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Pragma", "no-cache")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": grant.AccessToken,
			"token_type":   "Bearer",
			"expires_in":   int(grant.ExpiresIn / time.Second),
			"scope":        strings.Join(grant.Scopes, " "),
		})
	}
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
