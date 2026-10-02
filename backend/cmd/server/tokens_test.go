package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"

	"github.com/r33drichards/browserjs-sessions/backend/internal/auth"
	"github.com/r33drichards/browserjs-sessions/backend/internal/config"
	"github.com/r33drichards/browserjs-sessions/backend/internal/sessions/sessionstest"
	"github.com/r33drichards/browserjs-sessions/backend/internal/tokens"
)

const (
	apiHost = "api.example.com"
	mallory = "mallory@example.com" // signs in nowhere: not in ALLOWED_EMAILS
)

// tokenServer is newServer's route table with API tokens in front, as run()
// builds it when API_URL is set. allowed is ALLOWED_EMAILS.
func tokenServer(t *testing.T, allowed ...string) (*server, *tokens.Store) {
	t.Helper()
	s := newServer(t)
	verifier, err := auth.NewAssertionVerifier(func(*jwt.Token) (any, error) { return &s.key.PublicKey, nil }, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	store := tokens.NewStore(dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{tokens.GVR: "APITokenList"}), sessionstest.Namespace)
	cfg := config.Config{PublicURL: sessionstest.PublicURL, SessionURLs: sessionstest.URLs(),
		APIURL: "https://" + apiHost, AllowedEmails: allowed}
	if s.handler, err = withAPITokens(cfg, verifier, store, s.handler); err != nil {
		t.Fatal(err)
	}
	return s, store
}

// bearer makes a request to host with an API token, and whatever other
// headers.
func (s *server) bearer(method, host, path, token, body string, headers ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Host = host
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, req)
	return rec
}

// newToken makes a token as email, in the browser.
func (s *server) newToken(email string, scopes ...string) (id, token string) {
	s.t.Helper()
	body, _ := json.Marshal(map[string]any{"name": "ci", "scopes": scopes})
	rec := s.do("POST", appHost, "/api/tokens", email, string(body))
	var created struct{ ID, Token string }
	if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &created) != nil || created.Token == "" {
		s.t.Fatalf("create token: %d %s", rec.Code, rec.Body)
	}
	return created.ID, created.Token
}

func TestAPIHostWithAToken(t *testing.T) {
	s, _ := tokenServer(t, alice, bob, root)
	mine := s.session(alice)
	theirs := s.session(bob)
	_, token := s.newToken(alice, auth.Scopes...)

	rec := s.bearer("GET", apiHost, "/v1/sessions", token, "")
	var list []sessionJSON
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &list) != nil || len(list) != 1 || list[0].ID != mine.ID {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}
	if rec := s.bearer("GET", apiHost, "/v1/sessions/"+mine.ID, token, ""); rec.Code != http.StatusOK {
		t.Errorf("get: %d %s", rec.Code, rec.Body)
	}
	// Another user's session is not there, as in the UI.
	if rec := s.bearer("GET", apiHost, "/v1/sessions/"+theirs.ID, token, ""); rec.Code != http.StatusNotFound {
		t.Errorf("another's session: %d %s", rec.Code, rec.Body)
	}
	rec = s.bearer("POST", apiHost, "/v1/sessions", token, `{"name":"from terraform"}`)
	var created sessionJSON
	if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &created) != nil || created.Owner != alice {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	if rec := s.bearer("PATCH", apiHost, "/v1/sessions/"+created.ID, token, `{"name":"renamed"}`); rec.Code != http.StatusOK {
		t.Errorf("rename: %d %s", rec.Code, rec.Body)
	}
	if rec := s.bearer("DELETE", apiHost, "/v1/sessions/"+created.ID, token, ""); rec.Code != http.StatusNoContent {
		t.Errorf("delete: %d %s", rec.Code, rec.Body)
	}
	want := `{"admin":false,"email":"alice@example.com","name":"alice@example.com"}`
	if rec := s.bearer("GET", apiHost, "/v1/me", token, ""); rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != want {
		t.Errorf("me: %d %s", rec.Code, rec.Body)
	}
	// The host may arrive with its port.
	if rec := s.bearer("GET", apiHost+":443", "/v1/sessions", token, ""); rec.Code != http.StatusOK {
		t.Errorf("with a port: %d", rec.Code)
	}
}

func TestAPIHostAdminsTokenIsNotAnAdmin(t *testing.T) {
	s, _ := tokenServer(t, alice, root)
	theirs := s.session(alice)
	_, token := s.newToken(root, auth.Scopes...)

	if rec := s.do("GET", appHost, "/api/sessions/"+theirs.ID, root, ""); rec.Code != http.StatusOK {
		t.Fatalf("the admin, signed in: %d", rec.Code)
	}
	if rec := s.bearer("GET", apiHost, "/v1/sessions/"+theirs.ID, token, ""); rec.Code != http.StatusNotFound {
		t.Errorf("the admin's token on another's session: %d %s", rec.Code, rec.Body)
	}
	if rec := s.bearer("DELETE", apiHost, "/v1/sessions/"+theirs.ID, token, ""); rec.Code != http.StatusNotFound {
		t.Errorf("the admin's token deleting another's session: %d %s", rec.Code, rec.Body)
	}
	rec := s.bearer("GET", apiHost, "/v1/sessions?all=1", token, "")
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Errorf("all=1 with the admin's token: %d %s", rec.Code, rec.Body)
	}
	if rec := s.bearer("GET", apiHost, "/v1/me", token, ""); !strings.Contains(rec.Body.String(), `"admin":false`) {
		t.Errorf("me: %s", rec.Body)
	}
}

func TestAPIHostIgnoresPomeriumsAssertion(t *testing.T) {
	s, _ := tokenServer(t, alice, root)
	s.session(alice)
	// A perfectly good assertion, even one made for this host, is no way in.
	for _, host := range []string{apiHost, appHost} {
		assertion := s.assertion(root, host)
		for _, path := range []string{"/v1/sessions", "/v1/me", "/api/sessions", "/api/me", "/api/tokens"} {
			rec := s.send("GET", apiHost, path, assertion, "")
			if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusNotFound {
				t.Errorf("GET %s with an assertion for %s: %d %s", path, host, rec.Code, rec.Body)
			}
		}
	}
	// And beside a token it changes nothing: the caller is the token's owner.
	_, token := s.newToken(alice, auth.ScopeSessionsRead)
	rec := s.bearer("GET", apiHost, "/v1/me", token, "", auth.AssertionHeader, s.assertion(root, apiHost))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), alice) || strings.Contains(rec.Body.String(), `"admin":true`) {
		t.Errorf("token and assertion together: %d %s", rec.Code, rec.Body)
	}
}

func TestAppAndSessionHostsIgnoreABearerToken(t *testing.T) {
	s, _ := tokenServer(t, alice)
	session := s.session(alice)
	_, token := s.newToken(alice, auth.Scopes...)
	for _, path := range []string{"/api/me", "/api/sessions", "/api/sessions/" + session.ID, "/api/tokens", "/v1/sessions"} {
		rec := s.bearer("GET", appHost, path, token, "")
		if rec.Code == http.StatusOK && !strings.Contains(rec.Body.String(), "<html>") {
			t.Errorf("GET %s on the app's host with a token: %d %s", path, rec.Code, rec.Body)
		}
	}
	if rec := s.bearer("GET", appHost, "/api/sessions", token, ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("the app's API with a token: %d", rec.Code)
	}
	if rec := s.bearer("POST", appHost, "/api/tokens", token, `{"name":"x","scopes":["sessions:read"]}`); rec.Code != http.StatusUnauthorized {
		t.Errorf("making a token with a token: %d", rec.Code)
	}
	before := s.pod.Load()
	host := session.ID + ".sessions.example.com"
	if rec := s.bearer("POST", host, "/mcp", token, "{}"); rec.Code/100 == 2 || s.pod.Load() != before {
		t.Errorf("a session's MCP endpoint with a token: %d", rec.Code)
	}
}

func TestAPIHostServesOnlyTheAPI(t *testing.T) {
	s, _ := tokenServer(t, alice)
	session := s.session(alice)
	_, token := s.newToken(alice, auth.Scopes...)
	before := s.pod.Load()
	for _, c := range []struct{ method, path string }{
		{"GET", "/"}, {"GET", "/index.html"}, {"GET", "/config.js"}, {"GET", "/sessions/" + session.ID},
		{"GET", "/healthz"}, {"GET", "/api/sessions"}, {"GET", "/api/tokens"},
		{"GET", "/v1/tokens"}, {"POST", "/v1/tokens"}, {"DELETE", "/v1/tokens/abcdefghijkl"},
		{"POST", "/v1/sessions/" + session.ID + "/vnc-ticket"},
		{"POST", "/api/sessions/" + session.ID + "/vnc-ticket"},
		{"GET", "/v1/sessions/" + session.ID + "/files"},
		{"POST", "/mcp"}, {"GET", "/vnc"}, {"PUT", "/api/artifact-uploads/abc"},
		{"GET", "/.well-known/oauth-authorization-server"},
	} {
		rec := s.bearer(c.method, apiHost, c.path, token, "")
		if rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "<html>") {
			t.Errorf("%s %s: %d %s", c.method, c.path, rec.Code, rec.Body)
		}
	}
	if s.pod.Load() != before {
		t.Error("a request to the API host reached a session's pod")
	}
}

func TestTokenPageAPI(t *testing.T) {
	s, _ := tokenServer(t, alice, bob)
	id, token := s.newToken(alice, auth.ScopeSessionsRead)

	rec := s.do("GET", appHost, "/api/tokens", alice, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), id) || strings.Contains(rec.Body.String(), token) {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}
	if rec := s.do("GET", appHost, "/api/tokens", "", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("list, not signed in: %d", rec.Code)
	}
	// An assertion made for another host is not the app's.
	if rec := s.send("GET", appHost, "/api/tokens", s.assertion(alice, apiHost), ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("list with the API host's assertion: %d", rec.Code)
	}
	// The API has no redirects.
	for _, path := range []string{"/api/tokens/", "//api/tokens", "/api/tokens/../sessions", "/api/tokens/x/../../me"} {
		if rec := s.do("GET", appHost, path, alice, ""); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s: %d (Location %q)", path, rec.Code, rec.Header().Get("Location"))
		}
	}
	// Only the app's host has it.
	host := "s-abcdefghij.sessions.example.com"
	if rec := s.do("GET", host, "/api/tokens", alice, ""); rec.Code != http.StatusNotFound {
		t.Errorf("on a session's host: %d", rec.Code)
	}

	// Revoked: the very next request is refused.
	if rec := s.bearer("GET", apiHost, "/v1/sessions", token, ""); rec.Code != http.StatusOK {
		t.Fatalf("before revocation: %d", rec.Code)
	}
	if rec := s.do("DELETE", appHost, "/api/tokens/"+id, bob, ""); rec.Code != http.StatusNoContent {
		t.Errorf("bob's revocation: %d", rec.Code)
	}
	if rec := s.bearer("GET", apiHost, "/v1/sessions", token, ""); rec.Code != http.StatusOK {
		t.Fatalf("bob revoked alice's token: %d", rec.Code)
	}
	if rec := s.do("DELETE", appHost, "/api/tokens/"+id, alice, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke: %d", rec.Code)
	}
	if rec := s.bearer("GET", apiHost, "/v1/sessions", token, ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("after revocation: %d", rec.Code)
	}
}

func TestRemovedFromTheAllowListLosesAPIAccess(t *testing.T) {
	// mallory made a token while allowed; the list a restart later lacks her.
	s, store := tokenServer(t, alice)
	_, token, err := store.Create(context.Background(), mallory, "kept", auth.Scopes, "", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	_, good := s.newToken(alice, auth.ScopeSessionsRead)
	bad := s.bearer("GET", apiHost, "/v1/sessions", token, "")
	unknown := s.bearer("GET", apiHost, "/v1/sessions", good[:len(good)-1]+"-", "")
	if bad.Code != http.StatusUnauthorized || bad.Body.String() != unknown.Body.String() {
		t.Errorf("removed user's token: %d %s; unknown token: %d %s", bad.Code, bad.Body, unknown.Code, unknown.Body)
	}
	if rec := s.bearer("GET", apiHost, "/v1/sessions", good, ""); rec.Code != http.StatusOK {
		t.Errorf("an allowed user's token: %d", rec.Code)
	}
	// Nor can she make another, should she still get as far as the app.
	rec := s.do("POST", appHost, "/api/tokens", mallory, `{"name":"x","scopes":["sessions:read"]}`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("making a token: %d %s", rec.Code, rec.Body)
	}
}

// API_URL without ALLOWED_EMAILS: the host is the API's and refuses
// everything; the app has no token endpoints.
func TestAPITokensOffWithNobodyAllowed(t *testing.T) {
	s, store := tokenServer(t)
	_, token, err := store.Create(context.Background(), alice, "early", auth.Scopes, "", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if rec := s.bearer("GET", apiHost, "/v1/sessions", token, ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("a token with nobody allowed: %d", rec.Code)
	}
	if rec := s.bearer("GET", apiHost, "/", token, ""); rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "<html>") {
		t.Errorf("the API host's /: %d %s", rec.Code, rec.Body)
	}
	for _, c := range []struct{ method, path string }{{"GET", "/api/tokens"}, {"POST", "/api/tokens"}, {"DELETE", "/api/tokens/abcdefghijkl"}} {
		if rec := s.do(c.method, appHost, c.path, alice, `{"name":"x","scopes":["sessions:read"]}`); rec.Code != http.StatusNotFound {
			t.Errorf("%s %s: %d %s", c.method, c.path, rec.Code, rec.Body)
		}
	}
}

// Without API_URL run() does not call withAPITokens at all: the server is
// newHandler's, which has no API host and no token endpoints.
func TestNoAPIURLNoTokens(t *testing.T) {
	s := newServer(t)
	if rec := s.do("GET", appHost, "/api/tokens", alice, ""); rec.Code != http.StatusNotFound {
		t.Errorf("GET /api/tokens: %d", rec.Code)
	}
	if rec := s.bearer("GET", appHost, "/api/sessions", "bjs_abcdefghijkl_"+strings.Repeat("a", 43), ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("a bearer token: %d", rec.Code)
	}
}

// exchange trades an API token for an access token, as an OAuth client does.
func (s *server) exchange(id, token, scope string) (*httptest.ResponseRecorder, string) {
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {id}, "client_secret": {token}}
	if scope != "" {
		form.Set("scope", scope)
	}
	req := httptest.NewRequest("POST", "/oauth/token", strings.NewReader(form.Encode()))
	req.Host = apiHost
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, req)
	var granted struct {
		AccessToken string `json:"access_token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &granted)
	return rec, granted.AccessToken
}

func TestAPIHostMCP(t *testing.T) {
	s, store := tokenServer(t, alice, bob, root)
	mine, other, theirs := s.session(alice), s.session(alice), s.session(bob)
	id, token := s.newToken(alice, auth.ScopeSessionsConnect)
	mcp := func(session, token string, headers ...string) *httptest.ResponseRecorder {
		return s.bearer("POST", apiHost, "/"+session+"/mcp", token, "{}", headers...)
	}

	// The owner, with the scope: the pod answers, through the session
	// proxy, whose hardening is on the answer.
	before := s.pod.Load()
	rec := mcp(mine.ID, token)
	if rec.Code != http.StatusOK || rec.Body.String() != "pod:/mcp" || s.pod.Load() != before+1 {
		t.Fatalf("the owner's call: %d %q", rec.Code, rec.Body)
	}
	if rec.Header().Get("Content-Security-Policy") == "" || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("the answer is not hardened: %v", rec.Header())
	}
	if rec := s.bearer("POST", apiHost, "/"+mine.ID+"/mcp/sub", token, "{}"); rec.Code != http.StatusOK || rec.Body.String() != "pod:/mcp/sub" {
		t.Errorf("a path under mcp: %d %q", rec.Code, rec.Body)
	}
	if rec := s.bearer("GET", apiHost, "/"+mine.ID+"/mcp", token, ""); rec.Code != http.StatusOK {
		t.Errorf("the event stream: %d %q", rec.Code, rec.Body)
	}

	before = s.pod.Load()
	refused := func(name string, rec *httptest.ResponseRecorder, want int) {
		t.Helper()
		if rec.Code != want {
			t.Errorf("%s: %d %s, want %d", name, rec.Code, rec.Body, want)
		}
	}
	// Somebody else's session is not there, for a user's token and for an
	// admin's alike: a token is never an admin.
	refused("another user's session", mcp(theirs.ID, token), http.StatusNotFound)
	_, adminToken := s.newToken(root, auth.Scopes...)
	refused("an admin's token on another's session", mcp(mine.ID, adminToken), http.StatusNotFound)
	refused("a session that does not exist", mcp("s-zzzzzzzzzz", token), http.StatusNotFound)
	// Without sessions:connect, whatever else the token has.
	_, manage := s.newToken(alice, auth.ScopeSessionsRead, auth.ScopeSessionsWrite, auth.ScopePoliciesRead, auth.ScopePoliciesWrite)
	refused("without the scope", mcp(mine.ID, manage), http.StatusForbidden)
	// A token for one session.
	_, bound, err := store.Create(context.Background(), alice, "one", auth.Scopes, mine.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	refused("a bound token on another session", mcp(other.ID, bound), http.StatusForbidden)
	refused("a bound token on another's API", s.bearer("GET", apiHost, "/v1/sessions/"+other.ID, bound, ""), http.StatusForbidden)
	refused("a bound token listing", s.bearer("GET", apiHost, "/v1/sessions", bound, ""), http.StatusForbidden)
	// Expired, revoked, none.
	_, expired, err := store.Create(context.Background(), alice, "old", auth.Scopes, "", -time.Second)
	if err != nil {
		t.Fatal(err)
	}
	refused("an expired token", mcp(mine.ID, expired), http.StatusUnauthorized)
	refused("no token", mcp(mine.ID, ""), http.StatusUnauthorized)
	refused("a cookie and an assertion", mcp(mine.ID, "", "Cookie", "_pomerium=x", auth.AssertionHeader, s.assertion(alice, apiHost)), http.StatusUnauthorized)
	refused("an assertion for the sessions' host", mcp(mine.ID, "", auth.AssertionHeader, s.assertion(alice, "sessions.example.com")), http.StatusUnauthorized)
	// The browser-request guard is the session proxy's, and still there.
	refused("a navigation", mcp(mine.ID, token, "Sec-Fetch-Mode", "navigate"), http.StatusForbidden)
	refused("a script", mcp(mine.ID, token, "Sec-Fetch-Mode", "no-cors", "Sec-Fetch-Dest", "script"), http.StatusForbidden)
	// Nothing else of a session.
	refused("the screen", s.bearer("GET", apiHost, "/"+mine.ID+"/vnc", token, ""), http.StatusNotFound)
	refused("an upload", s.bearer("PUT", apiHost, "/"+mine.ID+"/api/artifact-uploads/"+strings.Repeat("a", 64), token, "x"), http.StatusNotFound)
	if s.pod.Load() != before {
		t.Errorf("%d refused requests reached a pod", s.pod.Load()-before)
	}

	if rec := mcp(mine.ID, bound); rec.Code != http.StatusOK {
		t.Errorf("a bound token on its session: %d %s", rec.Code, rec.Body)
	}
	if rec := s.do("DELETE", appHost, "/api/tokens/"+id, alice, ""); rec.Code != http.StatusNoContent {
		t.Fatal("revoke")
	}
	refused("a revoked token", mcp(mine.ID, token), http.StatusUnauthorized)
}

// The sessions' host is Pomerium's: a token is nothing there.
func TestSessionsHostIgnoresATokenOnMCP(t *testing.T) {
	s, _ := tokenServer(t, alice)
	mine := s.session(alice)
	_, token := s.newToken(alice, auth.Scopes...)
	_, access := s.exchange(strings.Split(token, "_")[1], token, "")
	before := s.pod.Load()
	for _, credential := range []string{token, access} {
		rec := s.bearer("POST", "sessions.example.com", "/"+mine.ID+"/mcp", credential, "{}")
		if rec.Code != http.StatusForbidden || s.pod.Load() != before {
			t.Errorf("a token on the sessions' host: %d %s", rec.Code, rec.Body)
		}
	}
	// And Pomerium's assertion still works there.
	rec := s.send("POST", "sessions.example.com", "/"+mine.ID+"/mcp", s.assertion(alice, "sessions.example.com"), "{}")
	if rec.Code != http.StatusOK {
		t.Errorf("signed in on the sessions' host: %d %s", rec.Code, rec.Body)
	}
}

func TestClientCredentials(t *testing.T) {
	s, _ := tokenServer(t, alice, bob)
	mine, theirs := s.session(alice), s.session(bob)
	id, token := s.newToken(alice, auth.ScopeSessionsConnect, auth.ScopeSessionsRead)

	rec, access := s.exchange(id, token, "")
	var granted struct {
		TokenType string `json:"token_type"`
		ExpiresIn int    `json:"expires_in"`
		Scope     string
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &granted) != nil || access == "" ||
		granted.TokenType != "Bearer" || granted.ExpiresIn != 3600 || granted.Scope != "sessions:read sessions:connect" {
		t.Fatalf("exchange: %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), token) || strings.HasPrefix(access, "bjs_") {
		t.Error("the access token is the API token")
	}
	if rec := s.bearer("POST", apiHost, "/"+mine.ID+"/mcp", access, "{}"); rec.Code != http.StatusOK || rec.Body.String() != "pod:/mcp" {
		t.Errorf("MCP with the access token: %d %s", rec.Code, rec.Body)
	}
	if rec := s.bearer("GET", apiHost, "/v1/sessions", access, ""); rec.Code != http.StatusOK {
		t.Errorf("the API with the access token: %d %s", rec.Code, rec.Body)
	}
	if rec := s.bearer("POST", apiHost, "/"+theirs.ID+"/mcp", access, "{}"); rec.Code != http.StatusNotFound {
		t.Errorf("another's session with the access token: %d", rec.Code)
	}
	if rec := s.bearer("POST", apiHost, "/v1/sessions", access, "{}"); rec.Code != http.StatusForbidden {
		t.Errorf("a scope the API token lacks: %d", rec.Code)
	}

	// Narrowed: the access token has what was asked for, and no more.
	_, narrow := s.exchange(id, token, "sessions:read")
	if rec := s.bearer("POST", apiHost, "/"+mine.ID+"/mcp", narrow, "{}"); rec.Code != http.StatusForbidden {
		t.Errorf("MCP with a token narrowed to sessions:read: %d", rec.Code)
	}
	if rec, _ := s.exchange(id, token, "sessions:write"); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid_scope") {
		t.Errorf("a wider scope: %d %s", rec.Code, rec.Body)
	}
	// An access token is not a client secret, and makes no token.
	if rec, _ := s.exchange(id, access, ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("exchanging an access token: %d", rec.Code)
	}
	if rec := s.bearer("POST", appHost, "/api/tokens", access, `{"name":"x","scopes":["sessions:read"]}`); rec.Code != http.StatusUnauthorized {
		t.Errorf("making a token with an access token: %d", rec.Code)
	}

	// A wrong secret and a client that does not exist get the same answer.
	wrong, _ := s.exchange(id, token[:len(token)-1]+"-", "")
	_, bobs := s.newToken(bob, auth.ScopeSessionsRead)
	swapped, _ := s.exchange(id, bobs, "")
	unknown, _ := s.exchange("aaaaaaaaaaaa", "bjs_aaaaaaaaaaaa_"+strings.Repeat("a", 43), "")
	for name, rec := range map[string]*httptest.ResponseRecorder{"wrong secret": wrong, "another's secret": swapped, "unknown client": unknown} {
		if rec.Code != http.StatusUnauthorized || rec.Body.String() != unknown.Body.String() || strings.TrimSpace(rec.Body.String()) != `{"error":"invalid_client"}` {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}

	// Revoking the API token ends its access tokens at once.
	if rec := s.do("DELETE", appHost, "/api/tokens/"+id, alice, ""); rec.Code != http.StatusNoContent {
		t.Fatal("revoke")
	}
	if rec := s.bearer("POST", apiHost, "/"+mine.ID+"/mcp", access, "{}"); rec.Code != http.StatusUnauthorized {
		t.Errorf("the access token after its API token was revoked: %d", rec.Code)
	}
	if rec, _ := s.exchange(id, token, ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("exchange after revocation: %d", rec.Code)
	}
}
