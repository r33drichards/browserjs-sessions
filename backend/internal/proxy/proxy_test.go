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

	"k8s.io/client-go/dynamic"
	dynfake "k8s.io/client-go/dynamic/fake"

	"github.com/r33drichards/browserjs-sessions/backend/internal/auth"
	"github.com/r33drichards/browserjs-sessions/backend/internal/authz"
	"github.com/r33drichards/browserjs-sessions/backend/internal/idle"
	"github.com/r33drichards/browserjs-sessions/backend/internal/sessions"
	"github.com/r33drichards/browserjs-sessions/backend/internal/sessions/sessionstest"
)

// tokenVerifier treats the token text as the user ID.
type tokenVerifier struct{}

func (tokenVerifier) Verify(_ context.Context, raw string) (auth.User, error) {
	if raw == "bad" {
		return auth.User{}, errors.New("bad token")
	}
	return auth.User{Subject: raw}, nil
}

type upstreamCall struct{ method, path, host, authorization, body, uri string }

// A well-formed one-time upload token, as mcp-js issues them.
const uploadToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

type env struct {
	mux      *http.ServeMux
	id       string
	calls    *[]upstreamCall
	store    *sessions.Store
	tracker  *idle.Tracker
	upstream *httptest.Server
	client   dynamic.Interface
	skew     *atomic.Int64 // nanoseconds the tracker's clock runs ahead
	authz    *authz.Memory
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

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := t.Context()
	store, client := sessionstest.New(t)
	az := authz.NewMemory()
	s, _ := store.Create(ctx, "a", "alice")
	_ = az.AddSession(ctx, s.ID, "alice")
	sessionstest.SetStatus(t, client, s.ID, sessionstest.Ready("10.0.0.7"))

	var calls []upstreamCall
	e := &env{id: s.ID, calls: &calls, store: store, client: client, authz: az}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		e.mu.Lock()
		calls = append(calls, upstreamCall{r.Method, r.URL.Path, r.Host, r.Header.Get("Authorization"), string(body), r.RequestURI})
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
		Verifier:  tokenVerifier{},
		Authz:     az,
		Waker:     &Waker{Store: store, Timeout: time.Second, Poll: 5 * time.Millisecond},
		Idle:      tracker,
		PublicURL: "https://sessions.example.com",
		Issuer:    "https://kc.example.com/realms/browserjs",
		// Every pod port maps to the one test upstream.
		Target: func(sessions.Session, int) string { return strings.TrimPrefix(upstream.URL, "http://") },
	}
	mux := http.NewServeMux()
	p.Register(mux)
	e.mux, e.tracker, e.upstream, e.skew, e.proxy = mux, tracker, upstream, skew, p
	return e
}

// doUpgrade is do for a websocket handshake. The recorder cannot be
// hijacked, so it only suits handshakes the backend itself refuses.
func (e *env) doUpgrade(path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", path, nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	return rec
}

// ticket fetches a VNC ticket for the session as user.
func (e *env) ticket(t *testing.T, user string) string {
	t.Helper()
	rec := e.do("POST", "/api/sessions/"+e.id+"/vnc-ticket", user, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("%s's ticket: %d", user, rec.Code)
	}
	var body struct{ Ticket string }
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return body.Ticket
}

// websocket sends a websocket handshake for path to a real server in front
// of the proxy and returns the answer and the connection it arrived on.
func (e *env) websocket(t *testing.T, path string) (*http.Response, net.Conn, *bufio.Reader) {
	t.Helper()
	front := httptest.NewServer(e.mux)
	t.Cleanup(front.Close)
	conn, err := net.Dial("tcp", strings.TrimPrefix(front.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = io.WriteString(conn, "GET "+path+" HTTP/1.1\r\nHost: sessions.example.com\r\n"+
		"Connection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	return resp, conn, br
}

func (e *env) do(method, path, token, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	return rec
}

func TestMCPProxy(t *testing.T) {
	e := newEnv(t)
	path := "/s/" + e.id + "/mcp"

	rec := e.do("POST", path, "alice", `{"jsonrpc":"2.0"}`)
	if rec.Code != http.StatusOK || rec.Body.String() != "upstream-ok" {
		t.Fatalf("owner call: %d %s", rec.Code, rec.Body)
	}
	c := (*e.calls)[0]
	if c.method != "POST" || c.path != "/mcp" || c.host != "localhost:8080" || c.body != `{"jsonrpc":"2.0"}` {
		t.Errorf("upstream saw %+v", c)
	}
	if c.authorization != "" {
		t.Error("the user's token was forwarded into the session pod")
	}

	// No token: 401 that points the client at the sign-in metadata.
	rec = e.do("POST", path, "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", rec.Code)
	}
	want := `Bearer resource_metadata="https://sessions.example.com/.well-known/oauth-protected-resource/s/` + e.id + `/mcp"`
	if got := rec.Header().Get("WWW-Authenticate"); got != want {
		t.Errorf("WWW-Authenticate = %q", got)
	}
	if rec := e.do("POST", path, "bad", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("bad token: %d", rec.Code)
	}
	// Someone else's session, and a missing one, look the same.
	if rec := e.do("POST", path, "bob", ""); rec.Code != http.StatusNotFound {
		t.Errorf("stranger: %d, want 404", rec.Code)
	}
	if rec := e.do("POST", "/s/s-missing000/mcp", "alice", ""); rec.Code != http.StatusNotFound {
		t.Errorf("missing: %d", rec.Code)
	}
	if len(*e.calls) != 1 {
		t.Errorf("rejected requests reached the pod: %d upstream calls", len(*e.calls))
	}
}

func TestStoppedSessionIsNotWoken(t *testing.T) {
	e := newEnv(t)
	_ = e.store.Suspend(t.Context(), e.id, sessions.StoppedByUser)
	// (status still says ready in the fake cluster; operatingMode decides)
	if rec := e.do("POST", "/s/"+e.id+"/mcp", "alice", ""); rec.Code != http.StatusConflict && rec.Code != http.StatusGatewayTimeout {
		t.Errorf("stopped session: %d", rec.Code)
	}
	if len(*e.calls) != 0 {
		t.Error("request reached a stopped session")
	}
}

func TestMetadata(t *testing.T) {
	e := newEnv(t)
	rec := e.do("GET", "/.well-known/oauth-protected-resource/s/"+e.id+"/mcp", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("metadata: %d", rec.Code)
	}
	var m struct {
		Resource             string   `json:"resource"`
		AuthorizationServers []string `json:"authorization_servers"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	if m.Resource != "https://sessions.example.com/s/"+e.id+"/mcp" ||
		len(m.AuthorizationServers) != 1 || m.AuthorizationServers[0] != "https://kc.example.com/realms/browserjs" {
		t.Errorf("metadata = %+v", m)
	}
}

func TestUploadNeedsNoLogin(t *testing.T) {
	e := newEnv(t)
	rec := e.do("PUT", "/s/"+e.id+"/api/artifact-uploads/"+uploadToken, "", "file-bytes")
	if rec.Code != http.StatusOK {
		t.Fatalf("upload: %d", rec.Code)
	}
	c := (*e.calls)[0]
	if c.method != "PUT" || c.path != "/api/artifact-uploads/"+uploadToken || c.body != "file-bytes" {
		t.Errorf("upstream saw %+v", c)
	}
	// Only that route is open: the rest of the pod's API is not reachable.
	if rec := e.do("GET", "/s/"+e.id+"/api/artifacts", "", ""); rec.Code != http.StatusNotFound {
		t.Errorf("other pod API paths must not be exposed: %d", rec.Code)
	}
}

func TestVNCTicket(t *testing.T) {
	e := newEnv(t)
	ticketPath := "/api/sessions/" + e.id + "/vnc-ticket"

	if rec := e.do("POST", ticketPath, "bob", ""); rec.Code != http.StatusNotFound {
		t.Errorf("stranger ticket: %d", rec.Code)
	}
	rec := e.do("POST", ticketPath, "alice", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("ticket: %d", rec.Code)
	}
	var body struct{ Ticket string }
	_ = json.Unmarshal(rec.Body.Bytes(), &body)

	vnc := "/s/" + e.id + "/vnc?ticket=" + body.Ticket
	// The route carries a websocket and nothing else: a plain GET (a link
	// someone was sent) never reaches the pod, and does not use up the ticket.
	rec = e.do("GET", vnc, "", "")
	if rec.Code != http.StatusUpgradeRequired || rec.Header().Get("Upgrade") != "websocket" {
		t.Fatalf("plain GET: %d, Upgrade %q; want 426 naming websocket", rec.Code, rec.Header().Get("Upgrade"))
	}
	if n := len(e.seen()); n != 0 {
		t.Fatalf("a plain GET reached the pod (%d calls)", n)
	}
	resp, _, _ := e.websocket(t, vnc)
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("vnc with ticket: %d, want 101", resp.StatusCode)
	}
	if c := e.seen()[0]; c.path != "/websockify" || c.host != "localhost:6080" {
		t.Errorf("upstream saw %+v", c)
	}
	if rec := e.doUpgrade(vnc); rec.Code != http.StatusUnauthorized {
		t.Errorf("ticket reuse: %d, want 401", rec.Code)
	}
	if rec := e.doUpgrade("/s/" + e.id + "/vnc"); rec.Code != http.StatusUnauthorized {
		t.Errorf("no ticket: %d", rec.Code)
	}
}

func TestStoppedSessionAnswers409(t *testing.T) {
	e := newEnv(t)
	_ = e.store.Suspend(t.Context(), e.id, sessions.StoppedByUser)
	sessionstest.SetStatus(t, e.client, e.id, sessionstest.Suspended())
	if rec := e.do("POST", "/s/"+e.id+"/mcp", "alice", ""); rec.Code != http.StatusConflict {
		t.Errorf("stopped session: %d, want 409", rec.Code)
	}
	if len(*e.calls) != 0 {
		t.Error("request reached a stopped session")
	}
}

func TestWakingSessionTimesOutWith504(t *testing.T) {
	e := newEnv(t)
	sessionstest.SetStatus(t, e.client, e.id, map[string]any{}) // never becomes ready
	rec := e.do("POST", "/s/"+e.id+"/mcp", "alice", "")
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
	req := httptest.NewRequest("POST", "/s/"+e.id+"/mcp", strings.NewReader("{}")).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer alice")
	rec := httptest.NewRecorder()
	w := &countingWriter{ResponseWriter: rec}
	e.mux.ServeHTTP(w, req)

	if rec.Code == http.StatusGatewayTimeout {
		t.Error("a client disconnect was answered with 504")
	}
	if w.writes != 0 {
		t.Errorf("wrote a response (%d %q) to a client that is gone", rec.Code, rec.Body)
	}
	if len(*e.calls) != 0 {
		t.Error("request reached the pod")
	}
}

// The VNC route carries a real websocket, and an open viewer keeps the
// session from going idle.
func TestVNCWebsocketKeepsSessionAwake(t *testing.T) {
	e := newEnv(t)
	front := httptest.NewServer(e.mux)
	defer front.Close()

	var body struct{ Ticket string }
	_ = json.Unmarshal(e.do("POST", "/api/sessions/"+e.id+"/vnc-ticket", "alice", "").Body.Bytes(), &body)

	conn, err := net.Dial("tcp", strings.TrimPrefix(front.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = io.WriteString(conn, "GET /s/"+e.id+"/vnc?ticket="+body.Ticket+" HTTP/1.1\r\nHost: sessions.example.com\r\n"+
		"Connection: Upgrade\r\nUpgrade: websocket\r\nAuthorization: Bearer alice\r\n\r\n")
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
	if c := (*e.calls)[0]; c.path != "/websockify" || c.host != "localhost:6080" || c.authorization != "" {
		t.Errorf("upstream saw %+v", c)
	}
}

// A viewer may watch the session's screen but not drive it: the VNC ticket
// asks for View, the MCP endpoint for Manage.
func TestViewerCanWatchButNotDrive(t *testing.T) {
	e := newEnv(t)
	if err := e.authz.AddViewer(t.Context(), e.id, "vera"); err != nil {
		t.Fatal(err)
	}

	rec := e.do("POST", "/api/sessions/"+e.id+"/vnc-ticket", "vera", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("viewer ticket: %d, want 200", rec.Code)
	}
	var body struct{ Ticket string }
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if resp, _, _ := e.websocket(t, "/s/"+e.id+"/vnc?ticket="+body.Ticket); resp.StatusCode != http.StatusSwitchingProtocols {
		t.Errorf("viewer vnc: %d, want 101", resp.StatusCode)
	}

	for _, path := range []string{"/s/" + e.id + "/mcp", "/s/" + e.id + "/mcp/sse"} {
		if rec := e.do("POST", path, "vera", `{"jsonrpc":"2.0"}`); rec.Code != http.StatusNotFound {
			t.Errorf("viewer %s: %d, want 404", path, rec.Code)
		}
	}
	for _, c := range e.seen() {
		if c.path != "/websockify" {
			t.Errorf("a viewer's request reached the pod: %+v", c)
		}
	}
}

// Something that is not a session ID never reaches the cluster, on the
// routes with a login and the one without.
func TestMalformedIDsAre404(t *testing.T) {
	e := newEnv(t)
	fake := e.client.(*dynfake.FakeDynamicClient)
	fake.ClearActions()
	for _, c := range []struct{ method, path, token string }{
		{"POST", "/s/not-an-id/mcp", "alice"},
		{"POST", "/s/s-ABCDEFGHIJ/mcp/sse", "alice"},
		{"PUT", "/s/not-an-id/api/artifact-uploads/abc123", ""},
		{"PUT", "/s/..%2Fpods/api/artifact-uploads/abc123", ""},
		{"POST", "/api/sessions/not-an-id/vnc-ticket", "alice"},
		{"GET", "/s/not-an-id/vnc?ticket=x", ""},
	} {
		if c.path == "/s/not-an-id/vnc?ticket=x" {
			rec := e.doUpgrade(c.path)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("%s %s: %d, want 401", c.method, c.path, rec.Code)
			}
			continue
		}
		if rec := e.do(c.method, c.path, c.token, ""); rec.Code != http.StatusNotFound {
			t.Errorf("%s %s: %d, want 404", c.method, c.path, rec.Code)
		}
	}
	if n := len(fake.Actions()); n != 0 {
		t.Errorf("malformed IDs caused %d cluster requests: %v", n, fake.Actions())
	}
	if len(*e.calls) != 0 {
		t.Error("a request for a malformed ID reached a pod")
	}
	// Without a token the answer is still the sign-in challenge.
	if rec := e.do("POST", "/s/not-an-id/mcp", "", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("no token: %d, want 401", rec.Code)
	}
}
