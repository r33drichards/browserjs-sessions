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
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/client-go/dynamic"

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

type upstreamCall struct{ method, path, host, authorization, body string }

type env struct {
	mux      *http.ServeMux
	id       string
	calls    *[]upstreamCall
	store    *sessions.Store
	tracker  *idle.Tracker
	upstream *httptest.Server
	client   dynamic.Interface
	skew     *atomic.Int64 // nanoseconds the tracker's clock runs ahead
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := t.Context()
	store, client := sessionstest.New(t)
	az := authz.NewMemory()
	s, _ := store.Create(ctx, "a", "alice")
	_ = az.AddSession(ctx, s.ID, "alice")
	sessionstest.SetStatus(t, client, s.ID, sessionstest.Ready("10.0.0.7"))

	var calls []upstreamCall
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		calls = append(calls, upstreamCall{r.Method, r.URL.Path, r.Host, r.Header.Get("Authorization"), string(body)})
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
	return &env{mux: mux, id: s.ID, calls: &calls, store: store, tracker: tracker, upstream: upstream, client: client, skew: skew}
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
	rec := e.do("PUT", "/s/"+e.id+"/api/artifact-uploads/abc123", "", "file-bytes")
	if rec.Code != http.StatusOK {
		t.Fatalf("upload: %d", rec.Code)
	}
	c := (*e.calls)[0]
	if c.method != "PUT" || c.path != "/api/artifact-uploads/abc123" || c.body != "file-bytes" {
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
	if rec := e.do("GET", vnc, "", ""); rec.Code != http.StatusOK {
		t.Fatalf("vnc with ticket: %d", rec.Code)
	}
	if c := (*e.calls)[0]; c.path != "/websockify" || c.host != "localhost:6080" {
		t.Errorf("upstream saw %+v", c)
	}
	if rec := e.do("GET", vnc, "", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("ticket reuse: %d, want 401", rec.Code)
	}
	if rec := e.do("GET", "/s/"+e.id+"/vnc", "", ""); rec.Code != http.StatusUnauthorized {
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
