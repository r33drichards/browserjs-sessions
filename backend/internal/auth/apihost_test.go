package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeTokens knows tokens by their whole text.
type fakeTokens struct {
	tokens map[string]fakeToken
	err    error
	calls  int
}

type fakeToken struct {
	owner   string
	scopes  []string
	session string
}

// ExchangeToken grants "access-<secret>" for the client "id-<secret>".
func (f *fakeTokens) ExchangeToken(_ context.Context, id, secret string, scopes []string) (string, Grant, error) {
	f.calls++
	if f.err != nil {
		return "", Grant{}, f.err
	}
	t, ok := f.tokens[secret]
	if !ok || id != "id-"+secret {
		return "", Grant{}, ErrInvalidToken
	}
	if len(scopes) == 0 {
		scopes = t.scopes
	}
	for _, scope := range scopes {
		if !(&TokenInfo{Scopes: t.scopes}).Has(scope) {
			return "", Grant{}, ErrInvalidScope
		}
	}
	return t.owner, Grant{AccessToken: "access-" + secret, Scopes: scopes, ExpiresIn: time.Hour}, nil
}

func (f *fakeTokens) VerifyToken(_ context.Context, token string) (string, TokenInfo, error) {
	f.calls++
	if f.err != nil {
		return "", TokenInfo{}, f.err
	}
	t, ok := f.tokens[token]
	if !ok {
		return "", TokenInfo{}, ErrInvalidToken
	}
	return t.owner, TokenInfo{Name: "ci", Scopes: t.scopes, Session: t.session}, nil
}

type apiHostFixture struct {
	tokens *fakeTokens
	host   *APIHost
	clock  time.Time
	seen   []*http.Request // what reached the API
}

func newAPIHostFixture(allowed ...string) *apiHostFixture {
	f := &apiHostFixture{clock: time.Unix(1_800_000_000, 0)}
	f.tokens = &fakeTokens{tokens: map[string]fakeToken{
		"all":     {owner: "alice@example.com", scopes: Scopes},
		"read":    {owner: "alice@example.com", scopes: []string{ScopeSessionsRead}},
		"policy":  {owner: "alice@example.com", scopes: []string{ScopePoliciesRead}},
		"connect": {owner: "alice@example.com", scopes: []string{ScopeSessionsConnect}},
		"bound":   {owner: "alice@example.com", scopes: Scopes, session: "s-abcdefghij"},
		"root":    {owner: "root@example.com", scopes: Scopes},
		"removed": {owner: "mallory@example.com", scopes: Scopes},
	}}
	limiter := NewFailureLimiter(3, 10*time.Second, func() time.Time { return f.clock })
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.seen = append(f.seen, r)
		w.WriteHeader(http.StatusNoContent)
	})
	f.host = NewAPIHost(APIHostConfig{
		Tokens: f.tokens, Allowed: NewAllowList(allowed), Limiter: limiter, App: api,
		ValidSessionID: func(id string) bool { return strings.HasPrefix(id, "s-") && len(id) == 12 },
		SessionBase:    func(id string) string { return "https://sessions.example.com/" + id },
	})
	return f
}

func (f *apiHostFixture) do(method, path, token string, headers ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	req.Host = "api.example.com"
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Add(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	f.host.ServeHTTP(rec, req)
	return rec
}

func TestAPIHostServesTheAPIAsTheTokensOwner(t *testing.T) {
	f := newAPIHostFixture("alice@example.com", "root@example.com")
	rec := f.do("GET", "/v1/sessions/s-abcdefghij?x=1", "root",
		AssertionHeader, "forged", "Cookie", "_pomerium=forged")
	if rec.Code != http.StatusNoContent || len(f.seen) != 1 {
		t.Fatalf("got %d %s, reached the API %d times", rec.Code, rec.Body, len(f.seen))
	}
	inner := f.seen[0]
	if inner.URL.Path != "/api/sessions/s-abcdefghij" || inner.URL.RawQuery != "x=1" {
		t.Errorf("the API saw %s?%s", inner.URL.Path, inner.URL.RawQuery)
	}
	for _, h := range []string{AssertionHeader, "Cookie", "Authorization"} {
		if inner.Header.Get(h) != "" {
			t.Errorf("%s was passed on to the API", h)
		}
	}
	u, ok := UserFrom(inner.Context())
	if !ok || u.Subject != "root@example.com" || u.Token == nil || u.Token.Name != "ci" {
		t.Fatalf("caller: %+v %v", u, ok)
	}
	// root@example.com is an admin in the UI. Their token is not.
	if u.Admin {
		t.Error("a token made an admin")
	}
}

func TestAPIHostHasOneAnswerForEveryBadToken(t *testing.T) {
	var bodies []string
	for name, c := range map[string]struct{ token, header, value string }{
		"none":                 {},
		"unknown":              {token: "nope"},
		"owner not allowed":    {token: "removed"},
		"basic":                {header: "Authorization", value: "Basic YWxpY2U6eA=="},
		"bare":                 {header: "Authorization", value: "all"},
		"only an assertion":    {header: AssertionHeader, value: "anything"},
		"only a cookie":        {header: "Cookie", value: "_pomerium=anything"},
		"token in the query":   {header: "X-Nothing", value: "x"},
		"bearer with no token": {header: "Authorization", value: "Bearer "},
	} {
		f := newAPIHostFixture("alice@example.com")
		var headers []string
		if c.header != "" {
			headers = []string{c.header, c.value}
		}
		path := "/v1/sessions"
		if name == "token in the query" {
			path += "?token=all&access_token=all"
		}
		rec := f.do("GET", path, c.token, headers...)
		if rec.Code != http.StatusUnauthorized || len(f.seen) != 0 {
			t.Errorf("%s: %d, reached the API %d times", name, rec.Code, len(f.seen))
		}
		if rec.Header().Get("WWW-Authenticate") != "Bearer" {
			t.Errorf("%s: WWW-Authenticate %q", name, rec.Header().Get("WWW-Authenticate"))
		}
		bodies = append(bodies, rec.Body.String())
	}
	for _, body := range bodies {
		if body != "{\"error\":\"invalid token\"}\n" {
			t.Errorf("body %q", body)
		}
	}
}

func TestAPIHostTwoAuthorizationHeaders(t *testing.T) {
	f := newAPIHostFixture("alice@example.com")
	if rec := f.do("GET", "/v1/sessions", "nope", "Authorization", "Bearer all"); rec.Code != http.StatusUnauthorized {
		t.Errorf("two Authorization headers: %d", rec.Code)
	}
}

func TestAPIHostNobodyAllowed(t *testing.T) {
	f := newAPIHostFixture()
	if rec := f.do("GET", "/v1/sessions", "all"); rec.Code != http.StatusUnauthorized {
		t.Errorf("got %d", rec.Code)
	}
	if f.tokens.calls != 0 {
		t.Error("the token was looked up though nobody may use one")
	}
}

func TestAPIHostScopes(t *testing.T) {
	for _, c := range []struct {
		token, method, path string
		want                int
	}{
		{"all", "GET", "/v1/me", 204},
		{"policy", "GET", "/v1/me", 204},
		{"read", "GET", "/v1/sessions", 204},
		{"read", "GET", "/v1/sessions/s-abcdefghij", 204},
		{"read", "POST", "/v1/sessions", 403},
		{"read", "PATCH", "/v1/sessions/s-abcdefghij", 403},
		{"read", "DELETE", "/v1/sessions/s-abcdefghij", 403},
		{"policy", "GET", "/v1/sessions", 403},
		{"all", "POST", "/v1/sessions", 204},
		{"all", "PATCH", "/v1/sessions/s-abcdefghij", 204},
		{"all", "DELETE", "/v1/sessions/s-abcdefghij", 204},
		{"read", "GET", "/v1/sessions/s-abcdefghij/policy", 403},
		{"policy", "GET", "/v1/sessions/s-abcdefghij/policy", 204},
		{"policy", "PUT", "/v1/sessions/s-abcdefghij/policy", 403},
		{"policy", "DELETE", "/v1/sessions/s-abcdefghij/policy", 403},
		{"policy", "PUT", "/v1/sessions/s-abcdefghij/policy/management", 403},
		{"all", "PUT", "/v1/sessions/s-abcdefghij/policy", 204},
		{"all", "DELETE", "/v1/sessions/s-abcdefghij/policy", 204},
		{"all", "PUT", "/v1/sessions/s-abcdefghij/policy/management", 204},
		{"read", "POST", "/v1/policies/validate", 204},
		{"read", "POST", "/v1/policies/evaluate", 204},
		{"read", "GET", "/v1/policy-schema.json", 204},
		{"read", "GET", "/v1/policy-presets", 204},
	} {
		f := newAPIHostFixture("alice@example.com")
		if rec := f.do(c.method, c.path, c.token); rec.Code != c.want {
			t.Errorf("%s %s with %q: %d %s, want %d", c.method, c.path, c.token, rec.Code, rec.Body, c.want)
		}
	}
}

func TestAPIHostServesNothingElse(t *testing.T) {
	for _, c := range []struct{ method, path string }{
		// Tokens are not made or revoked with a token.
		{"GET", "/v1/tokens"}, {"POST", "/v1/tokens"}, {"DELETE", "/v1/tokens/abcdefghijkl"},
		// The screen and the files of a session's browser.
		{"POST", "/v1/sessions/s-abcdefghij/vnc-ticket"},
		{"GET", "/v1/sessions/s-abcdefghij/files"}, {"POST", "/v1/sessions/s-abcdefghij/files"},
		{"GET", "/v1/sessions/s-abcdefghij/files/x"},
		{"PUT", "/v1/sessions"}, {"GET", "/v1/"}, {"GET", "/v1/nope"},
		{"GET", "/v1/sessions/"}, {"GET", "/v1/sessions/../me"}, {"GET", "/v1//me"},
		{"GET", "/v1/sessions/a%2Fb"},
	} {
		f := newAPIHostFixture("alice@example.com")
		rec := f.do(c.method, c.path, "all")
		if rec.Code != http.StatusNotFound || len(f.seen) != 0 {
			t.Errorf("%s %s: %d (Location %q), reached the API %d times", c.method, c.path, rec.Code, rec.Header().Get("Location"), len(f.seen))
		}
	}
	// Outside /v1 there is nothing at all, with or without a token: not the
	// UI, not the app's API, not a session's endpoints.
	for _, path := range []string{"/", "/index.html", "/config.js", "/healthz", "/api/me", "/api/sessions", "/api/tokens",
		"/v1", "/mcp", "/vnc", "/api/artifact-uploads/abc", "/.well-known/oauth-authorization-server", "/sessions/create",
		// Of a session, only its MCP endpoint: not its screen, not the
		// upload URLs (those are on the sessions' host, where mcp-js makes
		// them), not the rest of its pod.
		"/s-abcdefghij", "/s-abcdefghij/", "/s-abcdefghij/vnc", "/s-abcdefghij/vnc?ticket=x",
		"/s-abcdefghij/api/artifact-uploads/" + strings.Repeat("a", 64), "/s-abcdefghij/mcpx",
		"/s-abcdefghij/api/exec", "/s-abcdefghij/../s-abcdefghij/mcp", "/s-abcdefghij//mcp", "/not-a-session/mcp",
		"/oauth/token/", "/oauth", "/oauth/authorize"} {
		for _, token := range []string{"", "all"} {
			f := newAPIHostFixture("alice@example.com")
			rec := f.do("GET", path, token)
			if rec.Code != http.StatusNotFound || len(f.seen) != 0 || f.tokens.calls != 0 {
				t.Errorf("GET %s (token %q): %d, reached the API %d times", path, token, rec.Code, len(f.seen))
			}
		}
	}
}

func TestAPIHostTokenCheckUnavailable(t *testing.T) {
	f := newAPIHostFixture("alice@example.com")
	f.tokens.err = errors.New("the API server is away")
	for range 10 {
		rec := f.do("GET", "/v1/sessions", "all")
		if rec.Code != http.StatusServiceUnavailable || strings.Contains(rec.Body.String(), "away") {
			t.Fatalf("got %d %s", rec.Code, rec.Body)
		}
	}
}

func TestAPIHostLimitsFailuresPerAddress(t *testing.T) {
	f := newAPIHostFixture("alice@example.com")
	from := func(addr string) []string { return []string{"X-Forwarded-For", addr} }
	for i := range 3 {
		if rec := f.do("GET", "/v1/sessions", "nope", from("198.51.100.7")...); rec.Code != http.StatusUnauthorized {
			t.Fatalf("failure %d: %d", i, rec.Code)
		}
	}
	// Now even a good token is not looked at from there.
	calls := f.tokens.calls
	rec := f.do("GET", "/v1/sessions", "all", from("198.51.100.7")...)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "10" || f.tokens.calls != calls {
		t.Fatalf("blocked address: %d, Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
	}
	// The address is the one the proxy appended, not what the client put
	// before it.
	if rec := f.do("GET", "/v1/sessions", "all", from("203.0.113.9, 198.51.100.7")...); rec.Code != http.StatusTooManyRequests {
		t.Errorf("forged X-Forwarded-For entry got past the limit: %d", rec.Code)
	}
	if rec := f.do("GET", "/v1/sessions", "all", from("198.51.100.7, 203.0.113.9")...); rec.Code != http.StatusNoContent {
		t.Errorf("another address: %d", rec.Code)
	}
	// One attempt comes back per interval.
	f.clock = f.clock.Add(10 * time.Second)
	if rec := f.do("GET", "/v1/sessions", "all", from("198.51.100.7")...); rec.Code != http.StatusNoContent {
		t.Errorf("after the interval: %d", rec.Code)
	}
	if rec := f.do("GET", "/v1/sessions", "nope", from("198.51.100.7")...); rec.Code != http.StatusUnauthorized {
		t.Errorf("after the interval, a failure: %d", rec.Code)
	}
	if rec := f.do("GET", "/v1/sessions", "all", from("198.51.100.7")...); rec.Code != http.StatusTooManyRequests {
		t.Errorf("after that failure: %d", rec.Code)
	}
	// Successes and refusals for scope do not count.
	for range 10 {
		if rec := f.do("POST", "/v1/sessions", "read", from("192.0.2.1")...); rec.Code != http.StatusForbidden {
			t.Fatalf("scope: %d", rec.Code)
		}
	}
}

func TestFailureLimiterForgetsAndStaysBounded(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	l := NewFailureLimiter(2, time.Second, func() time.Time { return now })
	for i := range limiterAddresses + 50 {
		l.Failed(strings.Repeat("a", 1+i%7) + string(rune('0'+i%10)) + time.Duration(i).String())
	}
	if len(l.failures) > limiterAddresses {
		t.Errorf("remembers %d addresses", len(l.failures))
	}
	l.Failed("x")
	l.Failed("x")
	if _, blocked := l.Blocked("x"); !blocked {
		t.Fatal("not blocked at the limit")
	}
	now = now.Add(time.Hour)
	if _, blocked := l.Blocked("x"); blocked {
		t.Error("still blocked an hour later")
	}
	if len(l.failures) > limiterAddresses {
		t.Errorf("remembers %d addresses", len(l.failures))
	}
}

func TestClientAddr(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "10.0.0.9:4711"
	if got := clientAddr(req); got != "10.0.0.9" {
		t.Errorf("no X-Forwarded-For: %q", got)
	}
	req.Header.Add("X-Forwarded-For", "1.1.1.1")
	req.Header.Add("X-Forwarded-For", "2.2.2.2, 3.3.3.3")
	if got := clientAddr(req); got != "3.3.3.3" {
		t.Errorf("X-Forwarded-For: %q", got)
	}
}

func TestSameHost(t *testing.T) {
	for _, c := range []struct {
		request, host string
		want          bool
	}{
		{"api.example.com", "api.example.com", true},
		{"API.example.com.", "api.example.com", true},
		{"api.example.com:443", "api.example.com", true},
		{"api.example.com", "api.example.com:8443", true},
		{"api.example.com:444", "api.example.com:8443", false},
		{"app.example.com", "api.example.com", false},
		{"api.example.com.evil.test", "api.example.com", false},
		{"", "", false},
	} {
		if got := SameHost(c.request, c.host); got != c.want {
			t.Errorf("SameHost(%q, %q) = %v", c.request, c.host, got)
		}
	}
}

// refuse is a Verifier nothing gets past.
type refuse struct{}

func (refuse) Verify(context.Context, string, string) (User, error) {
	return User{}, errors.New("no")
}

func TestMiddlewareLetsTheAPIHostsCallerThrough(t *testing.T) {
	reached := 0
	h := Middleware(refuse{})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached++ }))
	serve := func(u *User) int {
		req := httptest.NewRequest("GET", "/api/sessions", nil)
		req.Header.Set("Authorization", "Bearer bjs_whatever")
		if u != nil {
			req = req.WithContext(WithUser(req.Context(), *u))
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := serve(&User{Subject: "alice@example.com", Token: &TokenInfo{Name: "ci"}}); code != 200 || reached != 1 {
		t.Errorf("a caller with a token: %d, reached %d", code, reached)
	}
	// A bearer token is no credential here, and neither is a caller who did
	// not come by the API host.
	if code := serve(nil); code != http.StatusUnauthorized {
		t.Errorf("a bearer token alone: %d", code)
	}
	if code := serve(&User{Subject: "alice@example.com"}); code != http.StatusUnauthorized {
		t.Errorf("a caller without a token: %d", code)
	}
	if reached != 1 {
		t.Errorf("reached %d times", reached)
	}
}

func TestAPIHostMCP(t *testing.T) {
	const id, other = "s-abcdefghij", "s-zyxwvutsrq"
	for _, c := range []struct {
		name, token, method, path string
		want                      int
	}{
		{"with the scope", "connect", "POST", "/" + id + "/mcp", 204},
		{"every scope", "all", "POST", "/" + id + "/mcp", 204},
		{"the stream", "connect", "GET", "/" + id + "/mcp", 204},
		{"a path under it", "connect", "DELETE", "/" + id + "/mcp/x/y", 204},
		{"without the scope", "read", "POST", "/" + id + "/mcp", 403},
		{"a policy token", "policy", "POST", "/" + id + "/mcp", 403},
		{"bound to it", "bound", "POST", "/" + id + "/mcp", 204},
		{"bound to another", "bound", "POST", "/" + other + "/mcp", 403},
		{"no token", "", "POST", "/" + id + "/mcp", 401},
		{"a bad token", "nope", "POST", "/" + id + "/mcp", 401},
		{"its owner removed", "removed", "POST", "/" + id + "/mcp", 401},
	} {
		f := newAPIHostFixture("alice@example.com", "root@example.com")
		rec := f.do(c.method, c.path, c.token, AssertionHeader, "forged", "Cookie", "_pomerium=forged")
		if rec.Code != c.want {
			t.Errorf("%s: %d %s, want %d", c.name, rec.Code, rec.Body, c.want)
		}
		if (c.want == 204) != (len(f.seen) == 1) {
			t.Errorf("%s: reached the session proxy %d times", c.name, len(f.seen))
		}
		if c.want != 204 {
			continue
		}
		// What goes on is the request the session's own URL would have been.
		inner := f.seen[0]
		if inner.Host != "sessions.example.com" || inner.URL.Path != c.path || inner.Method != c.method {
			t.Errorf("%s: went on as %s %s%s", c.name, inner.Method, inner.Host, inner.URL.Path)
		}
		for _, h := range []string{AssertionHeader, "Cookie", "Authorization"} {
			if inner.Header.Get(h) != "" {
				t.Errorf("%s: %s was passed on", c.name, h)
			}
		}
		if u, ok := UserFrom(inner.Context()); !ok || u.Subject != "alice@example.com" || u.Admin || u.Token == nil {
			t.Errorf("%s: caller %+v", c.name, u)
		}
	}
}

func TestAPIHostMCPKeepsThePathAsSent(t *testing.T) {
	f := newAPIHostFixture("alice@example.com")
	rec := f.do("POST", "/s-abcdefghij/mcp/a%2Fb", "connect")
	if rec.Code != 204 || len(f.seen) != 1 {
		t.Fatalf("got %d", rec.Code)
	}
	// The session proxy refuses an encoded separator; it has to see it.
	if got := f.seen[0].URL.EscapedPath(); got != "/s-abcdefghij/mcp/a%2Fb" {
		t.Errorf("went on as %q", got)
	}
}

func TestAPIHostSessionBoundToken(t *testing.T) {
	const id, other = "s-abcdefghij", "s-zyxwvutsrq"
	for _, c := range []struct {
		method, path string
		want         int
	}{
		{"GET", "/v1/me", 204},
		{"GET", "/v1/sessions/" + id, 204},
		{"PATCH", "/v1/sessions/" + id, 204},
		{"PUT", "/v1/sessions/" + id + "/policy", 204},
		{"GET", "/v1/sessions/" + other, 403},
		{"DELETE", "/v1/sessions/" + other, 403},
		{"GET", "/v1/sessions/" + other + "/policy", 403},
		{"PUT", "/v1/sessions/" + other + "/policy/management", 403},
		// What reaches beyond one session.
		{"GET", "/v1/sessions", 403},
		{"POST", "/v1/sessions", 403},
	} {
		f := newAPIHostFixture("alice@example.com")
		if rec := f.do(c.method, c.path, "bound"); rec.Code != c.want {
			t.Errorf("%s %s: %d %s, want %d", c.method, c.path, rec.Code, rec.Body, c.want)
		}
	}
}

func (f *apiHostFixture) exchange(body string, headers ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/oauth/token", strings.NewReader(body))
	req.Host = "api.example.com"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	f.host.ServeHTTP(rec, req)
	return rec
}

func TestAPIHostTokenExchange(t *testing.T) {
	f := newAPIHostFixture("alice@example.com")
	rec := f.exchange("grant_type=client_credentials&client_id=id-all&client_secret=all&scope=sessions:connect+sessions:read")
	want := `{"access_token":"access-all","expires_in":3600,"scope":"sessions:connect sessions:read","token_type":"Bearer"}`
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != want || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("form: %d %s (Cache-Control %q)", rec.Code, rec.Body, rec.Header().Get("Cache-Control"))
	}
	// HTTP Basic, and no scope: all the token has.
	basic := func(id, secret string) string {
		req := httptest.NewRequest("GET", "/", nil)
		req.SetBasicAuth(id, secret)
		return req.Header.Get("Authorization")
	}
	rec = f.exchange("grant_type=client_credentials", "Authorization", basic("id-read", "read"))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"scope":"sessions:read"`) {
		t.Errorf("basic: %d %s", rec.Code, rec.Body)
	}
	if len(f.seen) != 0 {
		t.Error("the exchange reached the app")
	}

	for name, c := range map[string]struct {
		body, authorization, contentType string
		status                           int
		code                             string
	}{
		"a wider scope":      {body: "grant_type=client_credentials&client_id=id-read&client_secret=read&scope=sessions:write", status: 400, code: "invalid_scope"},
		"an unknown scope":   {body: "grant_type=client_credentials&client_id=id-read&client_secret=read&scope=admin", status: 400, code: "invalid_scope"},
		"another grant":      {body: "grant_type=password&username=alice&password=x", status: 400, code: "unsupported_grant_type"},
		"refresh":            {body: "grant_type=refresh_token&refresh_token=access-all", status: 400, code: "unsupported_grant_type"},
		"no grant":           {body: "client_id=id-all&client_secret=all", status: 400, code: "unsupported_grant_type"},
		"JSON":               {body: `{"grant_type":"client_credentials"}`, contentType: "application/json", status: 400, code: "invalid_request"},
		"a bearer token":     {body: "grant_type=client_credentials", authorization: "Bearer all", status: 401, code: "invalid_client"},
		"no credentials":     {body: "grant_type=client_credentials", status: 401, code: "invalid_client"},
		"its owner removed":  {body: "grant_type=client_credentials&client_id=id-removed&client_secret=removed", status: 401, code: "invalid_client"},
		"an unknown client":  {body: "grant_type=client_credentials&client_id=id-nope&client_secret=nope", status: 401, code: "invalid_client"},
		"a wrong secret":     {body: "grant_type=client_credentials&client_id=id-all&client_secret=read", status: 401, code: "invalid_client"},
		"another's id":       {body: "grant_type=client_credentials&client_id=id-read&client_secret=all", status: 401, code: "invalid_client"},
		"a wrong secret (b)": {body: "grant_type=client_credentials", authorization: basic("id-all", "x"), status: 401, code: "invalid_client"},
	} {
		f := newAPIHostFixture("alice@example.com")
		var headers []string
		if c.authorization != "" {
			headers = append(headers, "Authorization", c.authorization)
		}
		if c.contentType != "" {
			headers = append(headers, "Content-Type", c.contentType)
		}
		rec := f.exchange(c.body, headers...)
		if rec.Code != c.status || strings.TrimSpace(rec.Body.String()) != `{"error":"`+c.code+`"}` {
			t.Errorf("%s: %d %s, want %d %s", name, rec.Code, rec.Body, c.status, c.code)
		}
	}

	// Credentials in the query string are not read.
	f = newAPIHostFixture("alice@example.com")
	req := httptest.NewRequest("POST", "/oauth/token?grant_type=client_credentials&client_id=id-all&client_secret=all", strings.NewReader(""))
	req.Host = "api.example.com"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	f.host.ServeHTTP(rec, req)
	if rec.Code == 200 {
		t.Error("credentials in the query string were accepted")
	}
	if rec := f.do("GET", "/oauth/token", "all"); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: %d", rec.Code)
	}
}

func TestAPIHostTokenExchangeIsRateLimitedAndOffWithNobodyAllowed(t *testing.T) {
	f := newAPIHostFixture("alice@example.com")
	bad := "grant_type=client_credentials&client_id=id-all&client_secret=wrong"
	for i := range 3 {
		if rec := f.exchange(bad, "X-Forwarded-For", "198.51.100.7"); rec.Code != 401 {
			t.Fatalf("failure %d: %d", i, rec.Code)
		}
	}
	good := "grant_type=client_credentials&client_id=id-all&client_secret=all"
	if rec := f.exchange(good, "X-Forwarded-For", "198.51.100.7"); rec.Code != http.StatusTooManyRequests {
		t.Errorf("after three failures: %d", rec.Code)
	}
	// The limit is the address's, whatever it was failing at.
	if rec := f.do("GET", "/v1/sessions", "all", "X-Forwarded-For", "198.51.100.7"); rec.Code != http.StatusTooManyRequests {
		t.Errorf("the API after three failed exchanges: %d", rec.Code)
	}

	off := newAPIHostFixture()
	if rec := off.exchange(good); rec.Code != 401 || off.tokens.calls != 0 {
		t.Errorf("nobody allowed: %d, %d lookups", rec.Code, off.tokens.calls)
	}
}

func TestAuthenticatePrefersNothingOverTheAPIHostsCaller(t *testing.T) {
	// The session proxy calls Authenticate; this is how the API host's
	// caller reaches it. An assertion beside it is not looked at.
	req := httptest.NewRequest("POST", "/s-abcdefghij/mcp", nil)
	req.Header.Set(AssertionHeader, "anything")
	caller := User{Subject: "alice@example.com", Token: &TokenInfo{Name: "ci"}}
	u, err := Authenticate(refuse{}, req.WithContext(WithUser(req.Context(), caller)))
	if err != nil || u.Subject != caller.Subject || u.Token == nil {
		t.Errorf("got %+v %v", u, err)
	}
	if _, err := Authenticate(refuse{}, req); err == nil {
		t.Error("an assertion nobody vouches for was accepted")
	}
	if _, err := Authenticate(refuse{}, req.WithContext(WithUser(req.Context(), User{Subject: "alice@example.com"}))); err == nil {
		t.Error("a caller on the context without a token was accepted")
	}
}
