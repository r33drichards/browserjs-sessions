package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	dynfake "k8s.io/client-go/dynamic/fake"

	"github.com/r33drichards/computer-use/backend/internal/idle"
	"github.com/r33drichards/computer-use/backend/internal/sessions"
	"github.com/r33drichards/computer-use/backend/internal/sessions/sessionstest"
)

// What a pod answers is content chosen by whoever drives the session. It is
// handed on, but it must not act as a page of the origin it is served from.
func TestPodResponsesCannotActAsAPage(t *testing.T) { eachForm(t, testPodResponsesCannotActAsAPage) }

func testPodResponsesCannotActAsAPage(t *testing.T, e *env) {
	e.respondWith(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Header().Add("Set-Cookie", "evil=1; Path=/")
		w.Header().Set("Clear-Site-Data", `"storage"`)
		w.Header().Set("Service-Worker-Allowed", "/")
		_, _ = io.WriteString(w, "<script>alert(1)</script>")
	})
	check := func(route string, code int, h http.Header) {
		t.Helper()
		if code != http.StatusOK {
			t.Errorf("%s: %d, want the pod's 200", route, code)
		}
		for _, name := range []string{"Set-Cookie", "Clear-Site-Data", "Service-Worker-Allowed"} {
			if v := h.Values(name); len(v) != 0 {
				t.Errorf("%s: the pod's %s reached the browser: %q", route, name, v)
			}
		}
		for name, want := range map[string]string{
			"X-Content-Type-Options":       "nosniff",
			"Content-Security-Policy":      "sandbox; default-src 'none'",
			"Cross-Origin-Resource-Policy": "same-origin",
		} {
			if got := h.Get(name); got != want {
				t.Errorf("%s: %s = %q, want %q", route, name, got, want)
			}
		}
	}

	rec := e.do("POST", "/mcp", alice, "{}")
	check("mcp", rec.Code, rec.Header())
	rec = e.do("PUT", "/api/artifact-uploads/"+uploadToken, "", "bytes")
	check("upload", rec.Code, rec.Header())
	// A pod that answers the websocket handshake with a page instead.
	resp, _, _ := e.websocket(t, "/vnc?ticket="+e.ticket(t, alice))
	check("vnc handshake", resp.StatusCode, resp.Header)
}

// Path values arrive unescaped, so an encoded "../" must not be written into
// the pod's request path, where it could name another of the pod's routes.
func TestEncodedPathSegmentsAreRefused(t *testing.T) { eachForm(t, testEncodedPathSegmentsAreRefused) }

func testEncodedPathSegmentsAreRefused(t *testing.T, e *env) {
	const base = ""
	for _, c := range []struct{ method, path, token string }{
		{"PUT", base + "/api/artifact-uploads/..%2F..%2Fmcp", ""},
		{"PUT", base + "/api/artifact-uploads/..%2Fartifacts", ""},
		{"PUT", base + "/api/artifact-uploads/%2e%2e", ""},
		{"PUT", base + "/api/artifact-uploads/abc123", ""},
		{"PUT", base + "/api/artifact-uploads/" + strings.ToUpper(uploadToken), ""},
		{"PUT", base + "/api/artifact-uploads/" + uploadToken + "0", ""},
		{"PUT", base + "/api/artifact-uploads/" + uploadToken + "%2F..", ""},
		{"POST", base + "/mcp/..%2Fapi%2Fexec", alice},
		{"POST", base + "/mcp/%2e%2e", alice},
		{"POST", base + "/mcp/%2e", alice},
		{"POST", base + "/mcp/sse/%2e%2e/x", alice},
		{"POST", base + "/mcp/a%2F%2Fb", alice},
		{"POST", base + "/mcp/a%2Fb", alice},
		{"POST", base + "/mcp/a%5Cb", alice},
		{"POST", base + "/mcp/a%5C..%5Cb", alice},
	} {
		if rec := e.do(c.method, c.path, c.token, ""); rec.Code != http.StatusNotFound {
			t.Errorf("%s %s: %d, want 404", c.method, c.path, rec.Code)
		}
	}
	// Literal double slashes and dot segments never match a route as they are.
	for _, path := range []string{"/mcp//sse", "/mcp/../api/exec", "//mcp"} {
		if rec := e.do("POST", path, alice, ""); rec.Code/100 == 2 {
			t.Errorf("POST %s: %d", path, rec.Code)
		}
	}
	if calls := e.seen(); len(calls) != 0 {
		t.Fatalf("refused paths reached the pod: %+v", calls)
	}

	// What is forwarded is escaped again, so it stays one path.
	for _, c := range []struct{ method, path, token, want string }{
		{"PUT", base + "/api/artifact-uploads/" + uploadToken, "", "/api/artifact-uploads/" + uploadToken},
		{"POST", base + "/mcp", alice, "/mcp"},
		{"POST", base + "/mcp/sse", alice, "/mcp/sse"},
		{"POST", base + "/mcp/a%20b%3Fc%23d/e", alice, "/mcp/a%20b%3Fc%23d/e"},
		{"POST", base + "/mcp/sse?x=1", alice, "/mcp/sse"},
	} {
		if rec := e.do(c.method, c.path, c.token, ""); rec.Code != http.StatusOK {
			t.Errorf("%s %s: %d, want 200", c.method, c.path, rec.Code)
			continue
		}
		calls := e.seen()
		if got := calls[len(calls)-1].uri; got != c.want {
			t.Errorf("%s %s: the pod was asked for %q, want %q", c.method, c.path, got, c.want)
		}
	}
}

// isIdle reports whether the tracker holds a clock for id that has run out.
// (A session the tracker has never heard of starts its period when asked.)
func isIdle(tr *idle.Tracker, id string) bool { return len(tr.Idle([]string{id})) == 1 }

// The upload route has no login, so it must not be a way to start pods or to
// keep them running.
func TestUploadDoesNotWakeASleepingSession(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	if err := e.store.Suspend(ctx, e.id, sessions.StoppedByIdle); err != nil {
		t.Fatal(err)
	}
	sessionstest.SetStatus(t, e.client, e.id, sessionstest.Suspended())

	rec := e.do("PUT", "/api/artifact-uploads/"+uploadToken, "", "file-bytes")
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "MCP call") {
		t.Errorf("upload to a sleeping session: %d %q, want 409 saying how to wake it", rec.Code, rec.Body)
	}
	if s, _ := e.store.Get(ctx, e.id); s.State != sessions.Asleep {
		t.Errorf("state after an anonymous upload = %s, want asleep", s.State)
	}
	if n := len(e.seen()); n != 0 {
		t.Errorf("the upload reached a pod (%d calls)", n)
	}
	e.skew.Add(int64(16 * time.Minute))
	if isIdle(e.tracker, e.id) {
		t.Error("an upload to a sleeping session was recorded as activity")
	}
}

func TestUploadToAnUnknownSessionIsNotTracked(t *testing.T) {
	e := newEnv(t)
	const unknown = "s-aaaaaaaaaa"
	if rec := e.doAt(unknown, "PUT", "/api/artifact-uploads/"+uploadToken, "", "x"); rec.Code != http.StatusNotFound {
		t.Errorf("upload to an unknown session: %d, want 404", rec.Code)
	}
	e.skew.Add(int64(16 * time.Minute))
	if isIdle(e.tracker, unknown) {
		t.Error("an unknown session ID entered the idle tracker")
	}
}

// Only an upload the pod accepted counts as activity: a guessed or spent
// token must not keep a session awake.
func TestUploadCountsAsActivityOnlyWhenThePodAcceptsIt(t *testing.T) {
	e := newEnv(t)
	path := "/api/artifact-uploads/" + uploadToken
	e.tracker.Idle([]string{e.id}) // the sweeper has seen it: its period runs
	e.skew.Add(int64(16 * time.Minute))

	e.respondWith(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "no such upload", http.StatusForbidden) })
	if rec := e.do("PUT", path, "", "x"); rec.Code != http.StatusForbidden {
		t.Fatalf("refused upload: %d, want the pod's 403", rec.Code)
	}
	if !isIdle(e.tracker, e.id) {
		t.Error("an upload the pod refused was recorded as activity")
	}

	e.respond.Store(nil)
	if rec := e.do("PUT", path, "", "x"); rec.Code != http.StatusOK {
		t.Fatalf("upload: %d", rec.Code)
	}
	if isIdle(e.tracker, e.id) {
		t.Error("an accepted upload was not recorded as activity")
	}
}

// A tool call may run for longer than the idle period with no other traffic;
// the pod must not be taken away under it.
func TestInFlightMCPCallHoldsTheSessionAwake(t *testing.T) {
	e := newEnv(t)
	started, release := make(chan struct{}), make(chan struct{})
	e.respondWith(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		_, _ = io.WriteString(w, "done")
	})
	code := make(chan int, 1)
	go func() { code <- e.do("POST", "/mcp", alice, "{}").Code }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the call never reached the pod")
	}

	e.skew.Add(int64(time.Hour))
	if err := idle.Sweep(t.Context(), e.store, e.tracker); err != nil {
		t.Fatal(err)
	}
	if s, _ := e.store.Get(t.Context(), e.id); s.State != sessions.Running {
		t.Errorf("state during a long MCP call = %s, want running", s.State)
	}

	close(release)
	if got := <-code; got != http.StatusOK {
		t.Errorf("long call: %d", got)
	}
	// Once it is over the idle period runs again, from the end of the call.
	if isIdle(e.tracker, e.id) {
		t.Error("session idle the moment its call ended")
	}
	e.skew.Add(int64(16 * time.Minute))
	if !isIdle(e.tracker, e.id) {
		t.Error("session never goes idle after the call")
	}
}

// A cluster that cannot be asked is not a session that is waking up.
func TestStoreFailureIsNotATimeout(t *testing.T) {
	e := newEnv(t)
	// (the owner is known from here on; what fails below is finding the pod)
	if rec := e.do("POST", "/mcp", alice, "{}"); rec.Code != http.StatusOK {
		t.Fatalf("mcp: %d", rec.Code)
	}
	e.clusterDown()
	for _, c := range []struct{ method, path, user string }{
		{"POST", "/mcp", alice},
		{"PUT", "/api/artifact-uploads/" + uploadToken, ""},
	} {
		rec := e.do(c.method, c.path, c.user, "")
		if rec.Code != http.StatusBadGateway || rec.Header().Get("Retry-After") != "" {
			t.Errorf("%s %s: %d (Retry-After %q), want 502", c.method, c.path, rec.Code, rec.Header().Get("Retry-After"))
		}
		if strings.Contains(rec.Body.String(), "etcd") {
			t.Errorf("%s: the cluster's error was shown to the caller: %q", c.path, rec.Body)
		}
	}
}

// Who owns a session is read from the cluster. If it cannot be read, nobody
// is let in: not the owner, not an admin, and not as a sign-in challenge.
func TestAuthorizationFailureFailsClosed(t *testing.T) {
	e := newEnv(t)
	e.clusterDown()
	for _, user := range []string{alice, bob, root} {
		for name, rec := range map[string]*httptest.ResponseRecorder{
			"mcp":        e.do("POST", "/mcp", user, "{}"),
			"vnc-ticket": e.app("POST", "/api/sessions/"+e.id+"/vnc-ticket", user, ""),
		} {
			if rec.Code != http.StatusServiceUnavailable || strings.Contains(rec.Body.String(), "ticket") ||
				strings.Contains(rec.Body.String(), "etcd") {
				t.Errorf("%s as %s with the cluster down: %d %q, want 503", name, user, rec.Code, rec.Body)
			}
		}
	}
	if n := len(e.seen()); n != 0 {
		t.Errorf("%d requests reached the pod", n)
	}
}

// The ticket is a credential: nothing may store the response that carries it.
func TestVNCTicketIsNotCacheable(t *testing.T) {
	e := newEnv(t)
	rec := e.app("POST", "/api/sessions/"+e.id+"/vnc-ticket", alice, "")
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
}

// endless is a request body that never ends on its own; read counts what
// was taken from it.
type endless struct{ read atomic.Int64 }

func (b *endless) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	b.read.Add(int64(len(p)))
	return len(p), nil
}

// The upload route has no login, so what it reads is bounded.
func TestOversizedUploadIsRefused(t *testing.T) {
	e := newEnv(t)
	e.proxy.MaxUploadBytes = 1024
	path := "/api/artifact-uploads/" + uploadToken

	// A declared length over the limit is refused without bothering the pod.
	if rec := e.do("PUT", path, "", strings.Repeat("x", 1025)); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("declared oversized upload: %d, want 413", rec.Code)
	}
	if n := len(e.seen()); n != 0 {
		t.Errorf("an upload refused by its declared length reached the pod (%d calls)", n)
	}

	// An undeclared (chunked) one is cut off at the limit: were it read to
	// the end, this request would never be answered.
	front := httptest.NewServer(e.handler)
	defer front.Close()
	body := &endless{}
	req, err := http.NewRequestWithContext(t.Context(), "PUT", front.URL+path, io.NopCloser(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = e.host
	req.URL.Path = e.base + req.URL.Path
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("chunked oversized upload: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("chunked oversized upload: %d, want 413", resp.StatusCode)
	}
	for _, c := range e.seen() {
		if len(c.body) > 1024 {
			t.Errorf("the pod was sent %d bytes of a 1024-byte limit", len(c.body))
		}
	}

	// An upload within the limit still goes through.
	if rec := e.do("PUT", path, "", strings.Repeat("x", 1024)); rec.Code != http.StatusOK {
		t.Errorf("upload at the limit: %d, want 200", rec.Code)
	}
}

// Shutdown ends viewer connections: the HTTP server neither closes nor waits
// for a hijacked connection, so nothing else would.
func TestShutdownClosesViewerConnections(t *testing.T) {
	e := newEnv(t)
	resp, conn, br := e.websocket(t, "/vnc?ticket="+e.ticket(t, alice))
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade: %d", resp.StatusCode)
	}
	_, _ = io.WriteString(conn, "ping")
	echo := make([]byte, 4)
	if _, err := io.ReadFull(br, echo); err != nil {
		t.Fatalf("echo: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := e.proxy.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	// Shutdown returned, so the handler is done: the connection is closed
	// and the viewer no longer holds the session awake.
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := br.ReadByte(); !errors.Is(err, io.EOF) {
		t.Errorf("read after Shutdown: %v, want EOF", err)
	}
	e.skew.Add(int64(16 * time.Minute))
	if !isIdle(e.tracker, e.id) {
		t.Error("a viewer closed by Shutdown still holds the session awake")
	}

	// With no viewers there is nothing to wait for.
	if err := e.proxy.Shutdown(ctx); err != nil {
		t.Errorf("second Shutdown: %v", err)
	}
}

// A burst of requests to a running session asks the cluster once, and a
// failed request to the pod makes the next one ask again.
func TestRunningSessionIsNotLookedUpPerRequest(t *testing.T) {
	e := newEnv(t)
	e.proxy.Waker.RunningTTL = time.Minute
	gets := countGets(e.client)
	mcp, upload := "/mcp", "/api/artifact-uploads/"+uploadToken
	// One read to learn who owns the session, one to find its pod.
	const reads = 2

	for range 10 {
		if rec := e.do("POST", mcp, alice, "{}"); rec.Code != http.StatusOK {
			t.Fatalf("mcp: %d", rec.Code)
		}
		if rec := e.do("PUT", upload, "", "x"); rec.Code != http.StatusOK {
			t.Fatalf("upload: %d", rec.Code)
		}
	}
	if n := gets.Load(); n != reads {
		t.Errorf("20 requests to a running session read it %d times, want %d", n, reads)
	}

	e.upstream.Close() // the pod is gone
	if rec := e.do("POST", mcp, alice, "{}"); rec.Code != http.StatusBadGateway {
		t.Fatalf("pod gone: %d, want 502", rec.Code)
	}
	before := gets.Load()
	_ = e.do("POST", mcp, alice, "{}")
	if n := gets.Load() - before; n != 1 {
		t.Errorf("request after a failed one read the session %d times, want 1 (the remembered pod must be dropped)", n)
	}
}

// A pod's redirect is to a path of its own. Under the sessions' host that
// path is below the session's prefix, and the redirect must say so, or the
// client would be sent to the top of the host. Redirects to elsewhere are
// handed on as they are.
func TestRedirectsStayUnderTheSession(t *testing.T) { eachForm(t, testRedirectsStayUnderTheSession) }

func testRedirectsStayUnderTheSession(t *testing.T, e *env) {
	for loc, want := range map[string]string{
		"/mcp":                              e.base + "/mcp",
		"/mcp/sse?x=1#f":                    e.base + "/mcp/sse?x=1#f",
		"sse":                               e.base + "/mcp/sse", // relative to the pod's /mcp/x
		"../api/exec":                       e.base + "/api/exec",
		"../../../../etc/passwd":            e.base + "/etc/passwd", // cannot climb out of the prefix
		"/mcp/a%2Fb":                        e.base + "/mcp/a%2Fb",
		"https://elsewhere.example.com/mcp": "https://elsewhere.example.com/mcp",
		"//elsewhere.example.com/mcp":       "//elsewhere.example.com/mcp",
	} {
		if e.base == "" && !strings.Contains(loc, "elsewhere") {
			want = loc // a session's own host has no prefix to add: untouched
		}
		e.respondWith(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", loc)
			w.WriteHeader(http.StatusTemporaryRedirect)
		})
		rec := e.do("POST", "/mcp/x", alice, "{}")
		if got := rec.Header().Get("Location"); rec.Code != http.StatusTemporaryRedirect || got != want {
			t.Errorf("pod redirects to %q: %d, Location %q; want %q", loc, rec.Code, got, want)
		}
	}
}

// The backend itself never redirects on a session's routes: what the mux
// would "clean" (and redirect to, without the session's prefix) is not found.
func TestSessionPathsAreNotRedirected(t *testing.T) { eachForm(t, testSessionPathsAreNotRedirected) }

func testSessionPathsAreNotRedirected(t *testing.T, e *env) {
	for _, path := range []string{"//mcp", "/mcp//sse", "/mcp/../mcp", "/./mcp", "/mcp/.", "/api//artifact-uploads/" + uploadToken, ""} {
		for _, method := range []string{"GET", "POST", "PUT"} {
			host, full := e.where(e.id, path)
			if full == "" { // a session's own host always has a path
				continue
			}
			rec := httptest.NewRecorder()
			e.handler.ServeHTTP(rec, request(method, host, full, alice, ""))
			if rec.Code != http.StatusNotFound || rec.Header().Get("Location") != "" {
				t.Errorf("%s %s: %d, Location %q; want 404", method, full, rec.Code, rec.Header().Get("Location"))
			}
		}
	}
	if calls := e.seen(); len(calls) != 0 {
		t.Fatalf("they reached the pod: %+v", calls)
	}
}

// Every session is served from one origin, so what one session's pod answers
// must never be shown as a page of it: a browser that navigates to a session
// route, or loads one as a script, image or frame, is refused before the pod
// is asked. (Browsers say which it is in Sec-Fetch-Mode; an MCP client that
// runs in a page uses fetch, which is "cors".)
func TestBrowsersCannotOpenSessionRoutesAsPages(t *testing.T) {
	eachForm(t, testBrowsersCannotOpenSessionRoutesAsPages)
}

func testBrowsersCannotOpenSessionRoutesAsPages(t *testing.T, e *env) {
	send := func(method, path, user, mode string) *httptest.ResponseRecorder {
		host, full := e.where(e.id, path)
		req := request(method, host, full, user, "{}")
		req.Header.Set("Sec-Fetch-Mode", mode)
		rec := httptest.NewRecorder()
		e.handler.ServeHTTP(rec, req)
		return rec
	}
	for _, mode := range []string{"navigate", "no-cors", "nested-navigate", "NAVIGATE", "something-new"} {
		for _, c := range []struct{ method, path, user string }{
			{"GET", "/mcp", alice}, {"POST", "/mcp", alice}, {"GET", "/mcp/sse", alice},
			{"PUT", "/api/artifact-uploads/" + uploadToken, ""},
		} {
			if rec := send(c.method, c.path, c.user, mode); rec.Code != http.StatusForbidden {
				t.Errorf("%s %s as %s: %d, want 403", c.method, c.path, mode, rec.Code)
			}
		}
	}
	if calls := e.seen(); len(calls) != 0 {
		t.Fatalf("they reached the pod: %+v", calls)
	}
	// fetch from a page, and clients that are not browsers, are served.
	for _, mode := range []string{"cors", "same-origin", "CORS", ""} {
		if rec := send("POST", "/mcp", alice, mode); rec.Code != http.StatusOK {
			t.Errorf("POST /mcp as %q: %d, want 200", mode, rec.Code)
		}
	}
}

// The sessions' host is all sessions: a path there that names none is not
// found, without asking the cluster, and is never the app's.
func TestSessionsHostWithoutASession(t *testing.T) {
	e := newEnv(t)
	fake := e.client.(*dynfake.FakeDynamicClient)
	fake.ClearActions()
	for _, path := range []string{
		"/", "/mcp", "/vnc", "/index.html", "/api/sessions", "/api/me", "/healthz", "/config.js",
		"/api/artifact-uploads/" + uploadToken, "/s/" + e.id + "/mcp", "//" + e.id + "/mcp",
		"/" + strings.ToUpper(e.id) + "/mcp", "/s%2D" + e.id[2:] + "/mcp", "/" + e.id + "x/mcp",
	} {
		for _, method := range []string{"GET", "POST", "PUT"} {
			rec := httptest.NewRecorder()
			e.handler.ServeHTTP(rec, request(method, sessionsHost, path, alice, ""))
			if rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "the app") {
				t.Errorf("%s %s: %d %q, want 404", method, path, rec.Code, rec.Body)
			}
		}
	}
	if n := len(fake.Actions()); n != 0 {
		t.Errorf("they caused %d cluster requests", n)
	}
	if len(e.seen()) != 0 {
		t.Error("one of them reached a pod")
	}
}

// A deployment that never had a host per session has no such hosts: they are
// the app's, like any other host.
func TestNoLegacyHostsUnlessConfigured(t *testing.T) {
	e := newLegacyEnv(t)
	e.proxy.LegacyURLs = nil // read with each request
	if rec := e.do("POST", "/mcp", alice, "{}"); rec.Body.String() != "the app" {
		t.Errorf("POST /mcp at a session's old host: %d %q, want the app's answer", rec.Code, rec.Body)
	}
	if len(e.seen()) != 0 {
		t.Error("it reached the pod")
	}
}
