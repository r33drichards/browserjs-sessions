package proxy

import (
	"net/http"
	"testing"
	"time"

	"github.com/r33drichards/computer-use/backend/internal/sessions/sessionstest"
)

func (e *env) browserStarts() int {
	n := 0
	for _, c := range e.seen() {
		if c.uri == "/browser/start" {
			n++
		}
	}
	return n
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A new session's pod is asked once, as its own server expects to be asked.
func TestStartBrowserAsksThePodOnce(t *testing.T) {
	e := newEnv(t)
	e.respondWith(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusAccepted) })
	before, err := e.store.Get(t.Context(), e.id)
	if err != nil {
		t.Fatal(err)
	}
	e.proxy.StartBrowser(e.id)
	waitFor(t, "the pod to be asked", func() bool { return e.browserStarts() == 1 })
	time.Sleep(50 * time.Millisecond)
	if n := e.browserStarts(); n != 1 {
		t.Fatalf("the pod was asked %d times", n)
	}
	// It is the backend's doing, not use of the session: nothing is
	// recorded that would keep it from sleeping.
	after, err := e.store.Get(t.Context(), e.id)
	if err != nil {
		t.Fatal(err)
	}
	if !after.LastActive.Equal(before.LastActive) || e.proxy.Idle.Calls(e.id) != 0 {
		t.Errorf("activity recorded: last active %v -> %v, %d calls", before.LastActive, after.LastActive, e.proxy.Idle.Calls(e.id))
	}
	var got upstreamCall
	for _, c := range e.seen() {
		if c.uri == "/browser/start" {
			got = c
		}
	}
	if got.method != "POST" || got.host != "localhost:8081" || got.header.Get("Content-Type") != "application/json" || got.header.Get("Origin") != "" {
		t.Errorf("pod got %s %s, Host %s, %v", got.method, got.uri, got.host, got.header)
	}
}

// A session whose pod is not up yet is asked once it is; one that is not
// running is not woken.
func TestStartBrowserWaitsForThePod(t *testing.T) {
	e := newEnv(t)
	e.respondWith(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusAccepted) })
	sessionstest.SetStatus(t, e.client, e.id, map[string]any{})
	e.proxy.StartBrowser(e.id)
	time.Sleep(100 * time.Millisecond)
	if n := e.browserStarts(); n != 0 {
		t.Fatalf("a session without a pod: %d requests", n)
	}
	sessionstest.SetStatus(t, e.client, e.id, sessionstest.Ready("10.0.0.7"))
	waitFor(t, "the pod to be asked once it runs", func() bool { return e.browserStarts() == 1 })
}

// A pod that is not answering yet is asked again; one whose image has no
// such endpoint is not.
func TestStartBrowserRetriesUntilAnswered(t *testing.T) {
	e := newEnv(t)
	fails := 2
	e.respondWith(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/browser/start" && fails > 0 {
			fails--
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	})
	if err := e.proxy.startBrowserWithPoll(t.Context(), e.id, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if n := e.browserStarts(); n != 3 {
		t.Errorf("asked %d times, want 3", n)
	}

	old := newEnv(t)
	old.respondWith(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	if err := old.proxy.startBrowserWithPoll(t.Context(), old.id, time.Millisecond); err != errNoBrowserStart {
		t.Errorf("an image without the endpoint: %v", err)
	}
	if n := old.browserStarts(); n != 1 {
		t.Errorf("an image without the endpoint was asked %d times", n)
	}
}
