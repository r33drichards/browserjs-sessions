package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	dynfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/r33drichards/computer-use/backend/internal/auth"
	"github.com/r33drichards/computer-use/backend/internal/authz"
	"github.com/r33drichards/computer-use/backend/internal/idle"
	"github.com/r33drichards/computer-use/backend/internal/sessions"
	"github.com/r33drichards/computer-use/backend/internal/sessions/sessionstest"
)

// The users of these tests. alice owns the session.
const (
	alice = "alice@example.com"
	bob   = "bob@example.com"
	root  = "root@example.com" // an admin
)

const appHost = "app.example.com"

// textVerifier takes the assertion's text for the user's email.
type textVerifier struct{}

func (textVerifier) Verify(_ context.Context, raw, host string) (auth.User, error) {
	if raw == "bad" || host == "" {
		return auth.User{}, errors.New("bad assertion")
	}
	return auth.User{Subject: raw, Admin: raw == root}, nil
}

type upstreamCall struct {
	method, path, host, body, uri string
	header                        http.Header
}

// A well-formed one-time upload token, as mcp-js issues them.
const uploadToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

type env struct {
	handler  http.Handler
	id       string // alice's session
	host     string // the host it is asked at
	base     string // what its paths start with there: "/<id>", or "" on a host of its own
	calls    *[]upstreamCall
	store    *sessions.Store
	tracker  *idle.Tracker
	upstream *httptest.Server
	client   dynamic.Interface
	skew     *atomic.Int64 // nanoseconds the tracker's clock runs ahead
	proxy    *Proxy

	mu sync.Mutex // guards appends to calls
	// respond, when set, answers for the pod instead of the default reply.
	respond atomic.Pointer[http.HandlerFunc]
}

// seen is a snapshot of what reached the pod so far.
func (e *env) seen() []upstreamCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]upstreamCall(nil), *e.calls...)
}

func (e *env) respondWith(h http.HandlerFunc) { e.respond.Store(&h) }

// clusterDown makes every read of a session fail from now on.
func (e *env) clusterDown() {
	e.client.(*dynfake.FakeDynamicClient).PrependReactor("get", sessions.SandboxGVR.Resource,
		func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewInternalError(errors.New("etcd is down"))
		})
}

// sessionsHost is where every session is; hostOf is the host a session used
// to have to itself, which still answers.
const sessionsHost = "sessions.example.com"

func hostOf(id string) string { return id + ".sessions.example.com" }

// newEnv is a deployment whose requests for sessions go to the sessions'
// host, the session's ID first in the path.
func newEnv(t *testing.T) *env { return newEnvAt(t, false) }

// newLegacyEnv is the same deployment, asked at the sessions' old hosts.
func newLegacyEnv(t *testing.T) *env { return newEnvAt(t, true) }

// eachForm runs test against both.
func eachForm(t *testing.T, test func(t *testing.T, e *env)) {
	t.Run("path", func(t *testing.T) { test(t, newEnv(t)) })
	t.Run("legacy host", func(t *testing.T) { test(t, newLegacyEnv(t)) })
}

// where is the host and path of path at session id.
func (e *env) where(id, path string) (host, fullPath string) {
	if e.base == "" {
		return hostOf(id), path
	}
	return sessionsHost, "/" + id + path
}

func newEnvAt(t *testing.T, legacy bool) *env {
	t.Helper()
	ctx := t.Context()
	store, client := sessionstest.New(t)
	s, _ := store.Create(ctx, "a", alice)
	sessionstest.SetStatus(t, client, s.ID, sessionstest.Ready("10.0.0.7"))

	var calls []upstreamCall
	e := &env{id: s.ID, host: sessionsHost, base: "/" + s.ID, calls: &calls, store: store, client: client}
	if legacy {
		e.host, e.base = hostOf(s.ID), ""
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		e.mu.Lock()
		calls = append(calls, upstreamCall{r.Method, r.URL.Path, r.Host, string(body), r.RequestURI, r.Header.Clone()})
		e.mu.Unlock()
		if h := e.respond.Load(); h != nil {
			(*h)(w, r)
			return
		}
		if r.Header.Get("Upgrade") != "" { // a websocket: switch protocols, then echo
			conn, buf, err := w.(http.Hijacker).Hijack()
			if err != nil {
				return
			}
			defer conn.Close()
			_, _ = buf.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
			_ = buf.Flush()
			_, _ = io.Copy(conn, buf)
			return
		}
		_, _ = io.WriteString(w, "upstream-ok")
	}))
	t.Cleanup(upstream.Close)

	skew := &atomic.Int64{}
	tracker := idle.New(15*time.Minute, func() time.Time { return time.Now().Add(time.Duration(skew.Load())) })
	p := &Proxy{
		Verifier:   textVerifier{},
		Authz:      authz.NewOwners(store, time.Minute),
		Waker:      &Waker{Store: store, Timeout: time.Second, Poll: 5 * time.Millisecond},
		Idle:       tracker,
		URLs:       sessionstest.URLs(),
		LegacyURLs: sessionstest.LegacyURLs(),
		// Every pod port maps to the one test upstream.
		Target: func(sessions.Session, int) string { return strings.TrimPrefix(upstream.URL, "http://") },
	}
	// The app's side, as cmd/server wires it: the API behind the middleware,
	// and everything else the UI's.
	api := http.NewServeMux()
	p.RegisterApp(api)
	app := http.NewServeMux()
	app.Handle("/api/", auth.Middleware(p.Verifier)(api))
	app.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "the app") })
	e.handler, e.tracker, e.upstream, e.skew, e.proxy = p.Handler(app), tracker, upstream, skew, p
	return e
}

// request is a request as Pomerium hands it on: made to host, stating who
// the user is (if anyone).
func request(method, host, path, user, body string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Host = host
	if user != "" {
		req.Header.Set(auth.AssertionHeader, user)
	}
	return req
}

// doAt makes a request for path at session id.
func (e *env) doAt(id, method, path, user, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	host, path := e.where(id, path)
	e.handler.ServeHTTP(rec, request(method, host, path, user, body))
	return rec
}

// do makes a request for path at alice's session.
func (e *env) do(method, path, user, body string) *httptest.ResponseRecorder {
	return e.doAt(e.id, method, path, user, body)
}

// app makes a request to the app's host.
func (e *env) app(method, path, user, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, request(method, appHost, path, user, body))
	return rec
}

// doUpgrade is do for a websocket handshake. The recorder cannot be
// hijacked, so it only suits handshakes the backend itself refuses.
func (e *env) doUpgrade(id, path string) *httptest.ResponseRecorder {
	host, path := e.where(id, path)
	req := request("GET", host, path, "", "")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
}

type ticketBody struct{ Ticket, URL string }

// ticket fetches a VNC ticket for the session as user.
func (e *env) ticket(t *testing.T, user string) string {
	t.Helper()
	rec := e.app("POST", "/api/sessions/"+e.id+"/vnc-ticket", user, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("%s's ticket: %d", user, rec.Code)
	}
	var body ticketBody
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return body.Ticket
}

// websocket sends a websocket handshake for path on the session's host to a
// real server in front of the proxy, and returns the answer and the
// connection it arrived on.
func (e *env) websocket(t *testing.T, path string) (*http.Response, net.Conn, *bufio.Reader) {
	t.Helper()
	front := httptest.NewServer(e.handler)
	t.Cleanup(front.Close)
	conn, err := net.Dial("tcp", strings.TrimPrefix(front.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = io.WriteString(conn, "GET "+e.base+path+" HTTP/1.1\r\nHost: "+e.host+"\r\n"+
		"Connection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	return resp, conn, br
}

func TestMCPProxy(t *testing.T) { eachForm(t, testMCPProxy) }

func testMCPProxy(t *testing.T, e *env) {

	rec := e.do("POST", "/mcp", alice, `{"jsonrpc":"2.0"}`)
	if rec.Code != http.StatusOK || rec.Body.String() != "upstream-ok" {
		t.Fatalf("owner call: %d %s", rec.Code, rec.Body)
	}
	c := e.seen()[0]
	if c.method != "POST" || c.path != "/mcp" || c.host != "localhost:8080" || c.body != `{"jsonrpc":"2.0"}` {
		t.Errorf("upstream saw %+v", c)
	}
	if rec := e.do("POST", "/mcp", root, "{}"); rec.Code != http.StatusOK {
		t.Errorf("admin call: %d, want 200", rec.Code)
	}

	// Nobody, or an identity that does not verify: refused, and never with
	// the 401 that Pomerium would turn into a 502.
	for name, user := range map[string]string{"no assertion": "", "bad assertion": "bad"} {
		rec := e.do("POST", "/mcp", user, "{}")
		if rec.Code != http.StatusForbidden || rec.Header().Get("WWW-Authenticate") != "" {
			t.Errorf("%s: %d (WWW-Authenticate %q), want a bare 403", name, rec.Code, rec.Header().Get("WWW-Authenticate"))
		}
	}
	// Someone else's session, and a missing one, look the same.
	if rec := e.do("POST", "/mcp", bob, ""); rec.Code != http.StatusNotFound {
		t.Errorf("stranger: %d, want 404", rec.Code)
	}
	for _, user := range []string{alice, root} {
		if rec := e.doAt("s-missing222", "POST", "/mcp", user, ""); rec.Code != http.StatusNotFound {
			t.Errorf("missing session as %s: %d, want 404", user, rec.Code)
		}
	}
	if n := len(e.seen()); n != 2 {
		t.Errorf("rejected requests reached the pod: %d upstream calls, want 2", n)
	}
}

// The pod is whatever its user's agent runs. It is not told who is calling,
// and is handed nothing it could present to the backend as them.
func TestCallerIdentityIsNotForwarded(t *testing.T) { eachForm(t, testCallerIdentityIsNotForwarded) }

func testCallerIdentityIsNotForwarded(t *testing.T, e *env) {
	req := request("POST", e.host, e.base+"/mcp", alice, "{}")
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("Cookie", "_pomerium=abc")
	req.Header.Set("X-Pomerium-Claim-Email", alice)
	req.Header.Set("Mcp-Session-Id", "m-1")
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("call: %d", rec.Code)
	}
	got := e.seen()[0].header
	for _, name := range []string{auth.AssertionHeader, "Authorization", "Cookie", "X-Pomerium-Claim-Email"} {
		if v := got.Values(name); len(v) != 0 {
			t.Errorf("%s was forwarded into the session pod: %q", name, v)
		}
	}
	if got.Get("Mcp-Session-Id") != "m-1" {
		t.Errorf("the MCP session header was not forwarded: %v", got)
	}
}

// Whatever is wrong with a request to the MCP endpoint, the answer is not
// 401: on Pomerium's MCP route an upstream 401 comes out as a 502.
func TestMCPNeverAnswers401(t *testing.T) { eachForm(t, testMCPNeverAnswers401) }

func testMCPNeverAnswers401(t *testing.T, e *env) {
	down := newEnv(t)
	down.clusterDown()
	stopped := newEnv(t)
	_ = stopped.store.Suspend(t.Context(), stopped.id, sessions.StoppedByUser)
	sessionstest.SetStatus(t, stopped.client, stopped.id, sessionstest.Suspended())

	for _, method := range []string{"GET", "POST", "DELETE", "PUT", "OPTIONS"} {
		for name, rec := range map[string]*httptest.ResponseRecorder{
			"no assertion":            e.do(method, "/mcp", "", ""),
			"bad assertion":           e.do(method, "/mcp", "bad", ""),
			"bad assertion, subpath":  e.do(method, "/mcp/sse", "bad", ""),
			"no assertion, bad path":  e.do(method, "/mcp/%2e%2e", "", ""),
			"stranger":                e.do(method, "/mcp", bob, ""),
			"missing session":         e.doAt("s-missing222", method, "/mcp", alice, ""),
			"missing session, nobody": e.doAt("s-missing222", method, "/mcp", "", ""),
			"not a session":           e.doAt("nope", method, "/mcp", "", ""),
			"cluster down":            down.do(method, "/mcp", alice, ""),
			"cluster down, nobody":    down.do(method, "/mcp", "", ""),
			"stopped session":         stopped.do(method, "/mcp", alice, ""),
		} {
			if rec.Code == http.StatusUnauthorized || rec.Code/100 == 2 || rec.Header().Get("WWW-Authenticate") != "" {
				t.Errorf("%s, %s: %d (WWW-Authenticate %q)", method, name, rec.Code, rec.Header().Get("WWW-Authenticate"))
			}
		}
	}
}

// A session's host has the session's routes and nothing else: not the rest
// of the pod's API, and not the app.
func TestSessionHostServesOnlyTheSession(t *testing.T) {
	eachForm(t, testSessionHostServesOnlyTheSession)
}

func testSessionHostServesOnlyTheSession(t *testing.T, e *env) {
	for _, c := range []struct{ method, path string }{
		{"GET", "/"}, {"GET", "/index.html"}, {"GET", "/config.js"}, {"GET", "/healthz"},
		{"GET", "/api/me"}, {"GET", "/api/sessions"}, {"POST", "/api/sessions"},
		{"POST", "/api/sessions/" + e.id + "/vnc-ticket"},
		{"GET", "/api/artifacts"}, {"GET", "/api/artifact-uploads/" + uploadToken},
		{"POST", "/api/artifact-uploads/" + uploadToken}, {"POST", "/api/exec"},
		{"GET", "/s/" + e.id + "/mcp"}, {"POST", "/s/" + e.id + "/mcp"},
		{"GET", "/.well-known/openid-configuration"}, {"GET", "/.well-known/pomerium/jwks.json"},
		{"POST", "/vnc"}, {"GET", "/vnc/x"}, {"GET", "/websockify"}, {"GET", "/mcpx"},
	} {
		rec := e.do(c.method, c.path, alice, "")
		if rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "the app") {
			t.Errorf("%s %s on a session host: %d %q, want 404", c.method, c.path, rec.Code, rec.Body)
		}
	}
	if n := len(e.seen()); n != 0 {
		t.Errorf("%d of them reached the pod", n)
	}
	// And the app's hosts have none of the session's routes.
	for _, host := range []string{appHost, "localhost:8080", "10.0.0.5:8080", "x.app.example.com", e.id + ".example.com"} {
		for _, c := range []struct{ method, path string }{
			{"POST", "/mcp"}, {"PUT", "/api/artifact-uploads/" + uploadToken}, {"GET", "/vnc?ticket=x"},
		} {
			rec := httptest.NewRecorder()
			e.handler.ServeHTTP(rec, request(c.method, host, c.path, alice, ""))
			if c.path == "/mcp" || strings.HasPrefix(c.path, "/vnc") {
				if rec.Body.String() != "the app" {
					t.Errorf("%s %s at %s: %d %q, want the app's answer", c.method, c.path, host, rec.Code, rec.Body)
				}
			} else if rec.Code != http.StatusNotFound {
				t.Errorf("%s %s at %s: %d, want the API's 404", c.method, c.path, host, rec.Code)
			}
		}
	}
	if rec := e.app("GET", "/", "", ""); rec.Body.String() != "the app" {
		t.Errorf("the app's own page: %d %q", rec.Code, rec.Body)
	}
	if n := len(e.seen()); n != 0 {
		t.Errorf("%d requests to the app's host reached the pod", n)
	}
}

// The host is the sessions' (or the session's), however it is spelled.
func TestSessionHostSpellings(t *testing.T) { eachForm(t, testSessionHostSpellings) }

func testSessionHostSpellings(t *testing.T, e *env) {
	for _, host := range []string{e.host, strings.ToUpper(e.host), e.host + ":443", e.host + "."} {
		rec := httptest.NewRecorder()
		e.handler.ServeHTTP(rec, request("POST", host, e.base+"/mcp", alice, "{}"))
		if rec.Code != http.StatusOK {
			t.Errorf("POST /mcp at %q: %d, want 200", host, rec.Code)
		}
	}
	// Another port is not the session's host; nor is it the app's.
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, request("POST", e.host+":8443", e.base+"/mcp", alice, "{}"))
	if rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "the app") {
		t.Errorf("POST /mcp at another port: %d %q, want 404", rec.Code, rec.Body)
	}
}

func TestStoppedSessionIsNotWoken(t *testing.T) {
	e := newEnv(t)
	_ = e.store.Suspend(t.Context(), e.id, sessions.StoppedByUser)
	// (status still says ready in the fake cluster; operatingMode decides)
	if rec := e.do("POST", "/mcp", alice, ""); rec.Code != http.StatusConflict && rec.Code != http.StatusGatewayTimeout {
		t.Errorf("stopped session: %d", rec.Code)
	}
	if len(e.seen()) != 0 {
		t.Error("request reached a stopped session")
	}
}

func TestUploadNeedsNoLogin(t *testing.T) { eachForm(t, testUploadNeedsNoLogin) }

func testUploadNeedsNoLogin(t *testing.T, e *env) {
	rec := e.do("PUT", "/api/artifact-uploads/"+uploadToken, "", "file-bytes")
	if rec.Code != http.StatusOK {
		t.Fatalf("upload: %d", rec.Code)
	}
	c := e.seen()[0]
	if c.method != "PUT" || c.path != "/api/artifact-uploads/"+uploadToken || c.body != "file-bytes" {
		t.Errorf("upstream saw %+v", c)
	}
	// Only that route is open: the rest of the pod's API is not reachable.
	if rec := e.do("GET", "/api/artifacts", "", ""); rec.Code != http.StatusNotFound {
		t.Errorf("other pod API paths must not be exposed: %d", rec.Code)
	}
}

func TestVNCTicket(t *testing.T) { eachForm(t, testVNCTicket) }

func testVNCTicket(t *testing.T, e *env) {
	ticketPath := "/api/sessions/" + e.id + "/vnc-ticket"

	if rec := e.app("POST", ticketPath, bob, ""); rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "ticket") {
		t.Errorf("stranger ticket: %d %q", rec.Code, rec.Body)
	}
	if rec := e.app("POST", "/api/sessions/s-missing222/vnc-ticket", root, ""); rec.Code != http.StatusNotFound {
		t.Errorf("ticket for a missing session: %d", rec.Code)
	}
	for name, user := range map[string]string{"nobody": "", "bad assertion": "bad"} {
		if rec := e.app("POST", ticketPath, user, ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: %d, want 401", name, rec.Code)
		}
	}
	if rec := e.app("POST", ticketPath, root, ""); rec.Code != http.StatusOK {
		t.Errorf("admin ticket: %d, want 200", rec.Code)
	}
	rec := e.app("POST", ticketPath, alice, "")
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("ticket: %d (%s)", rec.Code, rec.Header().Get("Content-Type"))
	}
	var body ticketBody
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	// The URL is on the sessions' host: the screen is not on the app's.
	if want := "wss://" + sessionsHost + "/" + e.id + "/vnc?ticket=" + body.Ticket; body.Ticket == "" || body.URL != want {
		t.Fatalf("ticket response = %+v, want url %q", body, want)
	}

	vnc := "/vnc?ticket=" + body.Ticket
	// The route carries a websocket and nothing else: a plain GET (a link
	// someone was sent) never reaches the pod, and does not use up the ticket.
	rec = e.do("GET", vnc, "", "")
	if rec.Code != http.StatusUpgradeRequired || rec.Header().Get("Upgrade") != "websocket" {
		t.Fatalf("plain GET: %d, Upgrade %q; want 426 naming websocket", rec.Code, rec.Header().Get("Upgrade"))
	}
	if n := len(e.seen()); n != 0 {
		t.Fatalf("a plain GET reached the pod (%d calls)", n)
	}
	// No sign-in travels with the handshake: the ticket is the credential.
	resp, _, _ := e.websocket(t, vnc)
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("vnc with ticket: %d, want 101", resp.StatusCode)
	}
	if c := e.seen()[0]; c.path != "/websockify" || c.host != "localhost:6080" {
		t.Errorf("upstream saw %+v", c)
	}
	if rec := e.doUpgrade(e.id, vnc); rec.Code != http.StatusUnauthorized {
		t.Errorf("ticket reuse: %d, want 401", rec.Code)
	}
	if rec := e.doUpgrade(e.id, "/vnc"); rec.Code != http.StatusUnauthorized {
		t.Errorf("no ticket: %d", rec.Code)
	}
}

// A ticket opens the screen of the session it was issued for, and no other.
func TestVNCTicketIsForOneSession(t *testing.T) { eachForm(t, testVNCTicketIsForOneSession) }

func testVNCTicketIsForOneSession(t *testing.T, e *env) {
	other, err := e.store.Create(t.Context(), "b", bob)
	if err != nil {
		t.Fatal(err)
	}
	sessionstest.SetStatus(t, e.client, other.ID, sessionstest.Ready("10.0.0.8"))

	if rec := e.doUpgrade(other.ID, "/vnc?ticket="+e.ticket(t, alice)); rec.Code != http.StatusUnauthorized {
		t.Errorf("alice's ticket at bob's session: %d, want 401", rec.Code)
	}
	if n := len(e.seen()); n != 0 {
		t.Errorf("%d requests reached a pod", n)
	}
}

func TestStoppedSessionAnswers409(t *testing.T) {
	e := newEnv(t)
	_ = e.store.Suspend(t.Context(), e.id, sessions.StoppedByUser)
	sessionstest.SetStatus(t, e.client, e.id, sessionstest.Suspended())
	if rec := e.do("POST", "/mcp", alice, ""); rec.Code != http.StatusConflict {
		t.Errorf("stopped session: %d, want 409", rec.Code)
	}
	if len(e.seen()) != 0 {
		t.Error("request reached a stopped session")
	}
}

func TestWakingSessionTimesOutWith504(t *testing.T) {
	e := newEnv(t)
	sessionstest.SetStatus(t, e.client, e.id, map[string]any{}) // never becomes ready
	rec := e.do("POST", "/mcp", alice, "")
	if rec.Code != http.StatusGatewayTimeout || rec.Header().Get("Retry-After") == "" {
		t.Errorf("waking session: %d, Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
	}
}

// countingWriter records whether a handler answered at all.
type countingWriter struct {
	http.ResponseWriter
	writes int
}

func (c *countingWriter) WriteHeader(code int) { c.writes++; c.ResponseWriter.WriteHeader(code) }
func (c *countingWriter) Write(b []byte) (int, error) {
	c.writes++
	return c.ResponseWriter.Write(b)
}

// A caller that hangs up while the session is waking is not a timeout: there
// is nobody to answer, and nothing must be sent on to the pod.
func TestClientDisconnectIsNotATimeout(t *testing.T) {
	e := newEnv(t)
	sessionstest.SetStatus(t, e.client, e.id, map[string]any{}) // not ready

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	req := request("POST", e.host, e.base+"/mcp", alice, "{}").WithContext(ctx)
	rec := httptest.NewRecorder()
	w := &countingWriter{ResponseWriter: rec}
	e.handler.ServeHTTP(w, req)

	if rec.Code == http.StatusGatewayTimeout {
		t.Error("a client disconnect was answered with 504")
	}
	if w.writes != 0 {
		t.Errorf("wrote a response (%d %q) to a client that is gone", rec.Code, rec.Body)
	}
	if len(e.seen()) != 0 {
		t.Error("request reached the pod")
	}
}

// The VNC route carries a real websocket, and an open viewer keeps the
// session from going idle.
func TestVNCWebsocketKeepsSessionAwake(t *testing.T) {
	e := newEnv(t)
	front := httptest.NewServer(e.handler)
	defer front.Close()

	ticket := e.ticket(t, alice)

	conn, err := net.Dial("tcp", strings.TrimPrefix(front.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = io.WriteString(conn, "GET "+e.base+"/vnc?ticket="+ticket+" HTTP/1.1\r\nHost: "+e.host+"\r\n"+
		"Connection: Upgrade\r\nUpgrade: websocket\r\nAuthorization: Bearer token\r\n\r\n")
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade: %d", resp.StatusCode)
	}
	_, _ = io.WriteString(conn, "ping")
	echo := make([]byte, 4)
	if _, err := io.ReadFull(br, echo); err != nil || string(echo) != "ping" {
		t.Fatalf("echo through the proxy: %q, %v", echo, err)
	}

	e.skew.Add(int64(time.Hour))
	if got := e.tracker.Idle([]string{e.id}); len(got) != 0 {
		t.Errorf("session with a viewer attached reported idle: %v", got)
	}

	// Once the viewer leaves, the idle period runs again.
	_ = conn.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		e.skew.Add(int64(16 * time.Minute))
		if got := e.tracker.Idle([]string{e.id}); len(got) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("session never went idle after the viewer left")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// (read after the connection is fully torn down, so the handler is done)
	if c := e.seen()[0]; c.path != "/websockify" || c.host != "localhost:6080" || c.header.Get("Authorization") != "" {
		t.Errorf("upstream saw %+v", c)
	}
}

// A host under the session domain that names no session never reaches the
// cluster, on the routes with a sign-in and those without.
func TestHostsThatNameNoSessionAre404(t *testing.T) { eachForm(t, testHostsThatNameNoSessionAre404) }

func testHostsThatNameNoSessionAre404(t *testing.T, e *env) {
	fake := e.client.(*dynfake.FakeDynamicClient)
	fake.ClearActions()
	for _, id := range []string{"nope", "s-abcdefg189", "s-aaaaaaaaaaa", "x." + e.id, e.id + ".x"} {
		for _, c := range []struct{ method, path, user string }{
			{"POST", "/mcp", alice},
			{"POST", "/mcp/sse", ""},
			{"PUT", "/api/artifact-uploads/" + uploadToken, ""},
			{"GET", "/", ""},
		} {
			if rec := e.doAt(id, c.method, c.path, c.user, ""); rec.Code != http.StatusNotFound {
				t.Errorf("%s %s at %s: %d, want 404", c.method, c.path, hostOf(id), rec.Code)
			}
		}
		if rec := e.doUpgrade(id, "/vnc?ticket=x"); rec.Code != http.StatusNotFound {
			t.Errorf("vnc at %s: %d, want 404", hostOf(id), rec.Code)
		}
	}
	for _, id := range []string{"not-an-id", "s-ABCDEFGHIJ", "..%2Fpods"} {
		if rec := e.app("POST", "/api/sessions/"+id+"/vnc-ticket", alice, ""); rec.Code != http.StatusNotFound {
			t.Errorf("ticket for %s: %d, want 404", id, rec.Code)
		}
	}
	if n := len(fake.Actions()); n != 0 {
		t.Errorf("they caused %d cluster requests: %v", n, fake.Actions())
	}
	if len(e.seen()) != 0 {
		t.Error("one of them reached a pod")
	}
}
