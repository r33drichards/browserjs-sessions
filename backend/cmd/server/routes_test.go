package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"k8s.io/client-go/dynamic"

	"github.com/r33drichards/computer-use/backend/internal/auth"
	"github.com/r33drichards/computer-use/backend/internal/config"
	"github.com/r33drichards/computer-use/backend/internal/idle"
	"github.com/r33drichards/computer-use/backend/internal/sessions"
	"github.com/r33drichards/computer-use/backend/internal/sessions/sessionstest"
)

const (
	appHost = "app.example.com"
	alice   = "alice@example.com"
	bob     = "bob@example.com"
	root    = "root@example.com" // in ADMIN_EMAILS
)

// server is the whole route table, built exactly as run() builds it, with
// Pomerium's part played by the test: it signs the assertions.
type server struct {
	t       *testing.T
	handler http.Handler
	key     *ecdsa.PrivateKey
	client  dynamic.Interface
	pod     atomic.Int64 // requests that reached a session pod
}

// sessionsHost is where the sessions are, each under its ID; legacyHost is
// the host a session used to have to itself.
const sessionsHost = "sessions.example.com"

func legacyHost(id string) string { return id + ".sessions.example.com" }

func newServer(t *testing.T) *server {
	t.Helper()
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html>app</html>"), 0o644)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := auth.NewAssertionVerifier(func(*jwt.Token) (any, error) { return &key.PublicKey, nil }, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		WebDir: dir, PublicURL: sessionstest.PublicURL,
		SessionURLs: sessionstest.URLs(), LegacySessionURLs: sessionstest.LegacyURLs(),
		SignOutURL: "/.pomerium/sign_out", ReadyTimeout: time.Second, MaxSessionsPerUser: 5,
	}
	store, client := sessionstest.New(t)
	s := &server{t: t, key: key, client: client}
	pod := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.pod.Add(1)
		_, _ = io.WriteString(w, "pod:"+r.URL.Path)
	}))
	t.Cleanup(pod.Close)
	// ServeMux panics at registration if two patterns conflict.
	handler, px := newHandler(cfg, verifier, store, idle.New(15*time.Minute, time.Now))
	px.Target = func(sessions.Session, int) string { return strings.TrimPrefix(pod.URL, "http://") }
	s.handler = handler
	return s
}

// assertion is the header Pomerium adds to a request by email to host.
func (s *server) assertion(email, host string, mutate ...func(jwt.MapClaims)) string {
	s.t.Helper()
	now := time.Now()
	claims := jwt.MapClaims{
		"iss": host, "aud": host, "iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(),
		"sub": "idp|" + email, "user": "idp|" + email, "email": email, "name": "N. " + email,
	}
	for _, m := range mutate {
		m(claims)
	}
	raw, err := jwt.NewWithClaims(jwt.SigningMethodES256, claims).SignedString(s.key)
	if err != nil {
		s.t.Fatal(err)
	}
	return raw
}

// send makes a request to host carrying assertion ("" for none).
func (s *server) send(method, host, path, assertion, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Host = host
	if assertion != "" {
		req.Header.Set(auth.AssertionHeader, assertion)
	}
	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, req)
	return rec
}

// do makes a request to host as email, the way it arrives through Pomerium.
func (s *server) do(method, host, path, email, body string) *httptest.ResponseRecorder {
	s.t.Helper()
	assertion := ""
	if email != "" {
		assertion = s.assertion(email, host)
	}
	return s.send(method, host, path, assertion, body)
}

type sessionJSON struct {
	ID, Name, Owner, State string
	MCPURL                 string `json:"mcp_url"`
}

// session creates a running session owned by email and returns it.
func (s *server) session(email string) sessionJSON {
	s.t.Helper()
	rec := s.do("POST", appHost, "/api/sessions", email, `{"name":"work"}`)
	if rec.Code != http.StatusCreated {
		s.t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var created sessionJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		s.t.Fatal(err)
	}
	sessionstest.SetStatus(s.t, s.client, created.ID, sessionstest.Ready("10.0.0.7"))
	return created
}

func TestAppHostRoutes(t *testing.T) {
	s := newServer(t)

	if rec := s.do("GET", appHost, "/healthz", "", ""); rec.Code != http.StatusOK {
		t.Errorf("healthz: %d", rec.Code)
	}
	// A probe reaches the pod by its address, not by a name.
	if rec := s.do("GET", "10.0.0.5:8080", "/healthz", "", ""); rec.Code != http.StatusOK {
		t.Errorf("healthz by address: %d", rec.Code)
	}
	want := `window.__BROWSERJS_CFG__ = {"signOutUrl":"/.pomerium/sign_out"};`
	if rec := s.do("GET", appHost, "/config.js", "", ""); rec.Code != http.StatusOK || rec.Body.String() != want {
		t.Errorf("config.js: %d %q, want %q", rec.Code, rec.Body, want)
	}
	if rec := s.do("GET", appHost, "/sessions/s-abcdefghij", "", ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "app") {
		t.Errorf("SPA route: %d %q", rec.Code, rec.Body)
	}

	// The API wants to know who is asking, on every path under it.
	for _, c := range []struct{ method, path string }{
		{"GET", "/api/me"}, {"GET", "/api/sessions"}, {"POST", "/api/sessions"},
		{"GET", "/api/sessions/s-abcdefghij"}, {"POST", "/api/sessions/s-abcdefghij/vnc-ticket"},
		{"GET", "/api/nope"}, {"GET", "/api/"},
	} {
		rec := s.do(c.method, appHost, c.path, "", `{"name":"x"}`)
		var body struct{ Error string }
		if rec.Code != http.StatusUnauthorized || json.Unmarshal(rec.Body.Bytes(), &body) != nil || body.Error == "" {
			t.Errorf("%s %s with no assertion: %d %q, want 401 with a JSON error", c.method, c.path, rec.Code, rec.Body)
		}
	}
	if rec := s.do("GET", appHost, "/api/sessions", alice, ""); rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Errorf("list: %d %q", rec.Code, rec.Body)
	}
	// Unknown API paths are the API's to refuse, never the app shell.
	if rec := s.do("GET", appHost, "/api/nope", alice, ""); rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "<html>") {
		t.Errorf("unknown API path: %d %q", rec.Code, rec.Body)
	}

	// Nothing answers for OAuth metadata (Pomerium does, in front), and a
	// file that is not there is not the app shell.
	for _, path := range []string{
		"/.well-known/oauth-authorization-server",
		"/.well-known/oauth-protected-resource",
		"/.well-known/oauth-protected-resource/mcp",
		"/.well-known/oauth-protected-resource/s/s-abcdefghij/mcp",
		"/assets/missing.js",
	} {
		rec := s.do("GET", appHost, path, "", "")
		if rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "<html>") {
			t.Errorf("GET %s: %d %q, want 404 and not the app shell", path, rec.Code, rec.Body)
		}
	}
}

func TestMe(t *testing.T) {
	s := newServer(t)
	for email, want := range map[string]string{
		alice:               `{"admin":false,"email":"alice@example.com","name":"N. alice@example.com"}`,
		"Alice@Example.com": `{"admin":false,"email":"alice@example.com","name":"N. Alice@Example.com"}`,
		root:                `{"admin":true,"email":"root@example.com","name":"N. root@example.com"}`,
		"ROOT@example.com":  `{"admin":true,"email":"root@example.com","name":"N. ROOT@example.com"}`,
	} {
		rec := s.do("GET", appHost, "/api/me", email, "")
		if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != want {
			t.Errorf("%s: %d %s, want %s", email, rec.Code, rec.Body, want)
		}
	}
}

// Who is asking is what Pomerium signed for this host and this moment, and
// nothing else.
func TestAssertionsThatDoNotVerify(t *testing.T) {
	s := newServer(t)
	mine := s.session(alice)
	host, mcp := sessionsHost, "/"+mine.ID+"/mcp"
	other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	forged, _ := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.MapClaims{
		"aud": appHost, "exp": time.Now().Add(time.Minute).Unix(), "email": alice,
	}).SignedString(other)
	unsigned, _ := jwt.NewWithClaims(jwt.SigningMethodNone, jwt.MapClaims{
		"aud": appHost, "exp": time.Now().Add(time.Minute).Unix(), "email": alice,
	}).SignedString(jwt.UnsafeAllowNoneSignatureType)

	for name, raw := range map[string]func(host string) string{
		"none":       func(string) string { return "" },
		"not a JWT":  func(string) string { return "alice@example.com" },
		"unsigned":   func(string) string { return unsigned },
		"other key":  func(string) string { return forged },
		"other host": func(string) string { return s.assertion(alice, "elsewhere.example.com") },
		"expired": func(h string) string {
			return s.assertion(alice, h, func(c jwt.MapClaims) { c["exp"] = time.Now().Add(-5 * time.Minute).Unix() })
		},
		"no expiry": func(h string) string { return s.assertion(alice, h, func(c jwt.MapClaims) { delete(c, "exp") }) },
		"no email":  func(h string) string { return s.assertion(alice, h, func(c jwt.MapClaims) { delete(c, "email") }) },
	} {
		// On the app's host the API says 401, which the UI takes as "sign in".
		if rec := s.send("GET", appHost, "/api/sessions/"+mine.ID, raw(appHost), ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("API, %s: %d, want 401", name, rec.Code)
		}
		if rec := s.send("DELETE", appHost, "/api/sessions/"+mine.ID, raw(appHost), ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("API DELETE, %s: %d, want 401", name, rec.Code)
		}
		// On a session's MCP endpoint it must not be 401: Pomerium turns
		// that into a 502.
		for _, method := range []string{"POST", "GET", "DELETE"} {
			rec := s.send(method, host, mcp, raw(host), "{}")
			if rec.Code != http.StatusForbidden || rec.Header().Get("WWW-Authenticate") != "" {
				t.Errorf("%s /mcp, %s: %d, want 403", method, name, rec.Code)
			}
		}
	}
	if n := s.pod.Load(); n != 0 {
		t.Errorf("%d of them reached the pod", n)
	}
	if rec := s.do("GET", appHost, "/api/sessions/"+mine.ID, alice, ""); rec.Code != http.StatusOK {
		t.Errorf("the session after all that: %d", rec.Code)
	}
}

// One key signs for every host, so an assertion is only good at the host it
// was made for: the app's is not the sessions', nor the other way round, and
// a session's old host is nobody else's. (On the sessions' host one
// assertion is good for every session there; whose a session is decides.)
func TestAssertionIsBoundToItsHost(t *testing.T) {
	s := newServer(t)
	a, b := s.session(alice), s.session(alice)
	oldA, oldB := legacyHost(a.ID), legacyHost(b.ID)

	for name, c := range map[string]struct{ assertionFor, sentTo string }{
		"the app's, at the sessions'":           {appHost, sessionsHost},
		"the sessions', at the app":             {sessionsHost, appHost},
		"the app's, at a session's old host":    {appHost, oldA},
		"an old host's, at another":             {oldA, oldB},
		"an old host's, at the app":             {oldA, appHost},
		"an old host's, at the sessions'":       {oldA, sessionsHost},
		"the sessions', at a session's old one": {sessionsHost, oldA},
	} {
		raw := s.assertion(alice, c.assertionFor)
		path, want := "/mcp", http.StatusForbidden
		switch c.sentTo {
		case appHost:
			path, want = "/api/sessions", http.StatusUnauthorized
		case sessionsHost:
			path = "/" + a.ID + "/mcp"
		}
		if rec := s.send("POST", c.sentTo, path, raw, `{"name":"x"}`); rec.Code != want {
			t.Errorf("%s: %d, want %d", name, rec.Code, want)
		}
	}
	if n := s.pod.Load(); n != 0 {
		t.Errorf("%d of them reached a pod", n)
	}
	for _, c := range []struct{ host, path string }{
		{sessionsHost, "/" + a.ID + "/mcp"}, {sessionsHost, "/" + b.ID + "/mcp"}, {oldA, "/mcp"},
	} {
		if rec := s.do("POST", c.host, c.path, alice, "{}"); rec.Code != http.StatusOK {
			t.Errorf("POST %s with %s's own assertion: %d", c.path, c.host, rec.Code)
		}
	}
}

// A session from creation to use, each request at the host it belongs to:
// the sessions', under the session's ID, and the host the session used to
// have to itself.
func TestSessionHostRoutes(t *testing.T) {
	t.Run("path", func(t *testing.T) {
		testSessionRoutes(t, func(id string) (string, string) { return sessionsHost, "/" + id })
	})
	t.Run("legacy host", func(t *testing.T) {
		testSessionRoutes(t, func(id string) (string, string) { return legacyHost(id), "" })
	})
}

func testSessionRoutes(t *testing.T, where func(id string) (host, base string)) {
	s := newServer(t)
	mine := s.session(alice)
	host, base := where(mine.ID)
	// The URLs given out are the sessions' host's, wherever a request arrives.
	public := sessionsHost + "/" + mine.ID
	if mine.Owner != alice || mine.MCPURL != "https://"+public+"/mcp" {
		t.Fatalf("created = %+v", mine)
	}

	// The MCP endpoint: the owner and an admin, nobody else.
	for user, want := range map[string]int{alice: http.StatusOK, root: http.StatusOK, bob: http.StatusNotFound, "": http.StatusForbidden} {
		for _, path := range []string{"/mcp", "/mcp/sse"} {
			rec := s.do("POST", host, base+path, user, "{}")
			if rec.Code != want {
				t.Errorf("POST %s as %q: %d, want %d", path, user, rec.Code, want)
			}
			if want == http.StatusOK && rec.Body.String() != "pod:"+path {
				t.Errorf("POST %s as %q: answered %q", path, user, rec.Body)
			}
		}
	}
	// The host is the same host, whatever case it arrives in.
	if rec := s.do("POST", strings.ToUpper(host), base+"/mcp", alice, "{}"); rec.Code != http.StatusOK {
		t.Errorf("POST /mcp at %s: %d", strings.ToUpper(host), rec.Code)
	}
	// A session of nobody's.
	for _, user := range []string{alice, root} {
		missingHost, missingBase := where("s-aaaaaaaaaa")
		if rec := s.do("POST", missingHost, missingBase+"/mcp", user, "{}"); rec.Code != http.StatusNotFound {
			t.Errorf("missing session as %s: %d, want 404", user, rec.Code)
		}
	}

	// The upload route has no sign-in.
	token := strings.Repeat("0123456789abcdef", 4)
	if rec := s.do("PUT", host, base+"/api/artifact-uploads/"+token, "", "bytes"); rec.Code != http.StatusOK ||
		rec.Body.String() != "pod:/api/artifact-uploads/"+token {
		t.Errorf("upload: %d %q", rec.Code, rec.Body)
	}

	// The screen: a ticket from the app's host opens it on the sessions'.
	ticketPath := "/api/sessions/" + mine.ID + "/vnc-ticket"
	if rec := s.do("POST", appHost, ticketPath, bob, ""); rec.Code != http.StatusNotFound {
		t.Errorf("stranger's ticket: %d, want 404", rec.Code)
	}
	for _, user := range []string{alice, root} {
		rec := s.do("POST", appHost, ticketPath, user, "")
		var ticket struct{ Ticket, URL string }
		_ = json.Unmarshal(rec.Body.Bytes(), &ticket)
		if rec.Code != http.StatusOK || len(ticket.Ticket) != 64 || ticket.URL != "wss://"+public+"/vnc?ticket="+ticket.Ticket {
			t.Errorf("%s's ticket: %d %s", user, rec.Code, rec.Body)
		}
		rec = s.do("GET", host, base+"/vnc?ticket="+ticket.Ticket, "", "")
		if rec.Code != http.StatusUpgradeRequired {
			t.Errorf("plain GET of the VNC route: %d, want 426", rec.Code)
		}
	}

	// Nothing else is there: not the app, its API, the old paths, or OpenID
	// metadata; neither under the session nor, on the sessions' host, beside
	// it.
	before := s.pod.Load()
	for _, c := range []struct{ method, path string }{
		{"GET", "/"}, {"GET", "/healthz"}, {"GET", "/config.js"}, {"GET", "/sessions/" + mine.ID},
		{"GET", "/api/me"}, {"GET", "/api/sessions"}, {"POST", ticketPath},
		{"GET", "/api/artifacts"}, {"POST", "/vnc"},
		{"POST", "/s/" + mine.ID + "/mcp"}, {"GET", "/.well-known/openid-configuration"},
	} {
		for _, path := range []string{base + c.path, c.path} {
			rec := s.do(c.method, host, path, alice, "")
			if rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "<html>") {
				t.Errorf("%s %s on a session host: %d %q, want 404", c.method, path, rec.Code, rec.Body)
			}
		}
	}
	// And the app's host has no session routes, however they are spelled.
	for _, c := range []struct{ method, path string }{
		{"POST", "/s/" + mine.ID + "/mcp"}, {"PUT", "/s/" + mine.ID + "/api/artifact-uploads/" + token},
		{"POST", "/" + mine.ID + "/mcp"}, {"PUT", "/" + mine.ID + "/api/artifact-uploads/" + token},
		{"POST", "/mcp"}, {"PUT", "/api/artifact-uploads/" + token},
	} {
		if rec := s.do(c.method, appHost, c.path, alice, "{}"); rec.Code/100 == 2 {
			t.Errorf("%s %s on the app's host: %d", c.method, c.path, rec.Code)
		}
	}
	if n := s.pod.Load() - before; n != 0 {
		t.Errorf("%d of them reached the pod", n)
	}
}

// The UI reads any redirect from the API as "signed out" and any 401 as the
// same, so the API never redirects, and 401 means only "no identity".
func TestAPINeverRedirects(t *testing.T) {
	s := newServer(t)
	mine := s.session(alice)
	for _, user := range []string{alice, ""} {
		for _, p := range []string{
			"/api", "/api/", "/api/sessions/", "//api/sessions", "/api//sessions", "/api/sessions//",
			"/api/./sessions", "/api/x/../sessions", "/x/../api/sessions", "//api/me", "/api/me/",
			"/api/sessions/" + mine.ID + "/", "/api/sessions/" + mine.ID + "//vnc-ticket",
		} {
			for _, method := range []string{"GET", "POST", "DELETE"} {
				rec := s.do(method, appHost, p, user, "{}")
				if rec.Code/100 == 3 || rec.Header().Get("Location") != "" {
					t.Errorf("%s %s as %q: %d, Location %q", method, p, user, rec.Code, rec.Header().Get("Location"))
				}
				if user != "" && rec.Code == http.StatusUnauthorized {
					t.Errorf("%s %s signed in: 401", method, p)
				}
				if rec.Code/100 == 2 && rec.Code != http.StatusNoContent && rec.Header().Get("Content-Type") != "application/json" {
					t.Errorf("%s %s: %d with Content-Type %q", method, p, rec.Code, rec.Header().Get("Content-Type"))
				}
			}
		}
	}
}

// Every session the API returns says where its MCP endpoint is, every 2xx
// body is JSON, and "not yours" is never 401.
func TestAPIResponsesForTheUI(t *testing.T) {
	s := newServer(t)
	mine := s.session(alice)
	want := "https://sessions.example.com/" + mine.ID + "/mcp"
	p := "/api/sessions/" + mine.ID
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"get":   s.do("GET", appHost, p, alice, ""),
		"patch": s.do("PATCH", appHost, p, alice, `{"name":"renamed"}`),
		"list":  s.do("GET", appHost, "/api/sessions", alice, ""),
		"all":   s.do("GET", appHost, "/api/sessions?all=1", root, ""),
		"me":    s.do("GET", appHost, "/api/me", alice, ""),
	} {
		if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/json" {
			t.Errorf("%s: %d, Content-Type %q", name, rec.Code, rec.Header().Get("Content-Type"))
		}
		if name != "me" && !strings.Contains(rec.Body.String(), `"mcp_url":"`+want+`"`) {
			t.Errorf("%s: no mcp_url in %s", name, rec.Body)
		}
	}
	rec := s.do("POST", appHost, p+"/vnc-ticket", alice, "")
	var ticket struct{ Ticket, URL string }
	_ = json.Unmarshal(rec.Body.Bytes(), &ticket)
	if rec.Header().Get("Content-Type") != "application/json" ||
		ticket.URL != "wss://sessions.example.com/"+mine.ID+"/vnc?ticket="+ticket.Ticket || ticket.Ticket == "" {
		t.Errorf("ticket: %d %s", rec.Code, rec.Body)
	}
	for _, c := range []struct{ method, path string }{
		{"GET", p}, {"PATCH", p}, {"DELETE", p}, {"POST", p + "/vnc-ticket"}, {"GET", "/api/nope"},
	} {
		if rec := s.do(c.method, appHost, c.path, bob, `{"name":"x"}`); rec.Code != http.StatusNotFound {
			t.Errorf("%s %s as a stranger: %d, want 404", c.method, c.path, rec.Code)
		}
	}
	if rec := s.do("DELETE", appHost, p, alice, ""); rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Errorf("delete: %d %q", rec.Code, rec.Body)
	}
}
