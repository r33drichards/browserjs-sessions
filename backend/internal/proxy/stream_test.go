package proxy

import (
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/r33drichards/computer-use/backend/internal/sessions"
	"github.com/r33drichards/computer-use/backend/internal/sessions/sessionstest"
)

// A GET on the MCP endpoint is the server-to-client event stream, which
// some clients hold open for as long as they are configured, used or not.
// It is not use of the session: only calls (POST, DELETE) are.

// asleep puts the session to sleep the way the idle sweeper does.
func (e *env) asleep(t *testing.T) {
	t.Helper()
	if err := e.store.Suspend(t.Context(), e.id, sessions.StoppedByIdle); err != nil {
		t.Fatal(err)
	}
	sessionstest.SetStatus(t, e.client, e.id, sessionstest.Suspended())
}

func TestEventStreamDoesNotWakeASleepingSession(t *testing.T) {
	e := newEnv(t)
	e.asleep(t)

	for _, c := range []struct{ method, path, user string }{
		{"GET", "/mcp", alice}, {"GET", "/mcp", root}, {"HEAD", "/mcp", alice}, {"GET", "/mcp/sse", alice},
	} {
		begin := time.Now()
		rec := e.do(c.method, c.path, c.user, "")
		if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "POST, DELETE" {
			t.Errorf("%s %s as %s: %d (Allow %q), want 405 allowing POST and DELETE",
				c.method, c.path, c.user, rec.Code, rec.Header().Get("Allow"))
		}
		if d := time.Since(begin); d > 500*time.Millisecond {
			t.Errorf("%s %s waited %s for the session", c.method, c.path, d)
		}
	}
	if s, _ := e.store.Get(t.Context(), e.id); s.State != sessions.Asleep {
		t.Errorf("state after the event stream was asked for = %s, want asleep", s.State)
	}
	if n := len(e.seen()); n != 0 {
		t.Errorf("%d requests reached a pod", n)
	}
	if at := e.lastActive(t, e.id); !at.IsZero() {
		t.Errorf("asking a sleeping session for its event stream was recorded as activity, at %s", at)
	}

	// A call is what wakes it.
	_ = e.do("POST", "/mcp", alice, "{}")
	if s, _ := e.store.Get(t.Context(), e.id); s.State == sessions.Asleep {
		t.Error("a POST did not wake the session")
	}
}

// Whatever keeps a session from running, its event stream is refused at
// once rather than waited for.
func TestEventStreamIsRefusedUnlessTheSessionRuns(t *testing.T) {
	for name, stop := range map[string]func(t *testing.T, e *env){
		"asleep": func(t *testing.T, e *env) { e.asleep(t) },
		"stopped by its user": func(t *testing.T, e *env) {
			_ = e.store.Suspend(t.Context(), e.id, sessions.StoppedByUser)
			sessionstest.SetStatus(t, e.client, e.id, sessionstest.Suspended())
		},
		"starting": func(t *testing.T, e *env) { sessionstest.SetStatus(t, e.client, e.id, map[string]any{}) },
	} {
		e := newEnv(t)
		stop(t, e)
		before, _ := e.store.Get(t.Context(), e.id)
		begin := time.Now()
		if rec := e.do("GET", "/mcp", alice, ""); rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: GET /mcp = %d, want 405", name, rec.Code)
		}
		if d := time.Since(begin); d > 500*time.Millisecond {
			t.Errorf("%s: GET /mcp waited %s for the session", name, d)
		}
		if after, _ := e.store.Get(t.Context(), e.id); after.State != before.State {
			t.Errorf("%s: GET /mcp changed the state from %s to %s", name, before.State, after.State)
		}
		if n := len(e.seen()); n != 0 {
			t.Errorf("%s: %d requests reached a pod", name, n)
		}
	}
}

// The stream is the session's, like the rest of the endpoint: who may not
// call may not listen, and learns nothing about the session's state.
func TestEventStreamIsAuthorized(t *testing.T) {
	for name, prepare := range map[string]func(t *testing.T, e *env){
		"running": func(*testing.T, *env) {},
		"asleep":  func(t *testing.T, e *env) { e.asleep(t) },
	} {
		e := newEnv(t)
		prepare(t, e)
		for user, want := range map[string]int{"": http.StatusForbidden, "bad": http.StatusForbidden, bob: http.StatusNotFound} {
			if rec := e.do("GET", "/mcp", user, ""); rec.Code != want {
				t.Errorf("%s session, GET /mcp as %q: %d, want %d", name, user, rec.Code, want)
			}
		}
		if rec := e.doAt("s-missing222", "GET", "/mcp", alice, ""); rec.Code != http.StatusNotFound {
			t.Errorf("GET /mcp of a missing session: %d, want 404", rec.Code)
		}
		if n := len(e.seen()); n != 0 {
			t.Errorf("%s session: %d refused requests reached a pod", name, n)
		}
	}
}

func TestEventStreamOfARunningSessionIsProxied(t *testing.T) {
	e := newEnv(t)
	for _, user := range []string{alice, root} {
		rec := e.do("GET", "/mcp", user, "")
		if rec.Code != http.StatusOK || rec.Body.String() != "upstream-ok" {
			t.Fatalf("GET /mcp as %s: %d %q", user, rec.Code, rec.Body)
		}
	}
	if c := e.seen()[0]; c.method != "GET" || c.path != "/mcp" || c.host != "localhost:8080" {
		t.Errorf("upstream saw %+v", c)
	}
	// A cluster that cannot be asked is neither a refusal nor a stream.
	e.clusterDown()
	if rec := e.do("GET", "/mcp", alice, ""); rec.Code != http.StatusBadGateway {
		t.Errorf("GET /mcp with the cluster down: %d, want 502", rec.Code)
	}
}

// An open stream with nothing else going on does not keep the pod: the
// session goes to sleep under it.
func TestEventStreamDoesNotHoldTheSessionAwake(t *testing.T) {
	e := newEnv(t)
	started, release := make(chan struct{}), make(chan struct{})
	e.respondWith(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		close(started)
		<-release
	})
	code := make(chan int, 1)
	go func() { code <- e.do("GET", "/mcp", alice, "").Code }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream never reached the pod")
	}

	e.skew.Add(int64(16 * time.Minute))
	if !e.isIdle(e.id) {
		t.Error("an open event stream counted as activity")
	}
	if err := e.sweep(); err != nil {
		t.Fatal(err)
	}
	if s, _ := e.store.Get(t.Context(), e.id); s.State == sessions.Running {
		t.Error("an open event stream kept the session awake past its idle period")
	}

	close(release)
	if got := <-code; got != http.StatusOK {
		t.Errorf("stream: %d", got)
	}
	// Nor does its closing count for anything.
	e.skew.Add(int64(16 * time.Minute))
	if e.isIdle(e.id) {
		t.Error("the end of an event stream was recorded as activity")
	}
}

// Opening the stream, over and over, is not a way to keep a session.
func TestEventStreamIsNotActivity(t *testing.T) {
	e := newEnv(t)
	e.skew.Add(int64(16 * time.Minute))
	for _, method := range []string{"GET", "HEAD"} {
		if rec := e.do(method, "/mcp", alice, ""); rec.Code != http.StatusOK {
			t.Fatalf("%s /mcp: %d", method, rec.Code)
		}
		if !e.isIdle(e.id) {
			t.Errorf("%s /mcp was recorded as activity", method)
		}
	}
	// A call is.
	for _, method := range []string{"POST", "DELETE"} {
		e.skew.Add(int64(16 * time.Minute))
		if !e.isIdle(e.id) {
			t.Fatalf("before %s: the session is not idle", method)
		}
		if rec := e.do(method, "/mcp", alice, "{}"); rec.Code != http.StatusOK {
			t.Fatalf("%s /mcp: %d", method, rec.Code)
		}
		if e.isIdle(e.id) {
			t.Errorf("%s /mcp was not recorded as activity", method)
		}
	}
}

// A call still holds the session for as long as it runs, stream or no stream.
func TestCallsStillWakeAndHoldTheSession(t *testing.T) {
	e := newEnv(t)
	started, release := make(chan struct{}), make(chan struct{})
	e.respondWith(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		_, _ = io.WriteString(w, "done")
	})
	code := make(chan int, 1)
	go func() { code <- e.do("DELETE", "/mcp", alice, "").Code }()
	<-started
	e.skew.Add(int64(time.Hour))
	if err := e.sweep(); err != nil {
		t.Fatal(err)
	}
	if s, _ := e.store.Get(t.Context(), e.id); s.State != sessions.Running {
		t.Errorf("state during a long DELETE = %s, want running", s.State)
	}
	close(release)
	if got := <-code; got != http.StatusOK {
		t.Errorf("DELETE: %d", got)
	}
}
