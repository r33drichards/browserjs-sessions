package proxy

import (
	"encoding/base64"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/r33drichards/computer-use/backend/internal/idle"
	"github.com/r33drichards/computer-use/backend/internal/sessions"
	"github.com/r33drichards/computer-use/backend/internal/sessions/sessionstest"
)

// The backend as more than one replica. Each env here is a replica: a
// Proxy, a Waker and a Tracker of its own, over one cluster. Nothing passes
// between them but what is written on the sessions.

// A ticket is issued by the replica the UI's request reached and redeemed
// by the one the websocket reached.
func TestATicketFromOneReplicaOpensTheScreenAtAnother(t *testing.T) {
	eachForm(t, func(t *testing.T, a *env) {
		b := a.replica("b", ticketKey)
		ticket := a.ticket(t, alice)

		resp, _, _ := b.websocket(t, "/vnc?ticket="+ticket)
		if resp.StatusCode != http.StatusSwitchingProtocols {
			t.Fatalf("a's ticket at b: %d, want 101", resp.StatusCode)
		}
		// Used up where it was redeemed.
		if rec := b.doUpgrade(b.id, "/vnc?ticket="+ticket); rec.Code != http.StatusUnauthorized {
			t.Errorf("the ticket again at b: %d, want 401", rec.Code)
		}
		// An admin's, for a session that is not theirs.
		if resp, _, _ := b.websocket(t, "/vnc?ticket="+a.ticket(t, root)); resp.StatusCode != http.StatusSwitchingProtocols {
			t.Errorf("an admin's ticket at b: %d, want 101", resp.StatusCode)
		}
	})
}

// What makes a ticket good is the key and nothing a replica remembers.
func TestATicketIsOnlyAsGoodAsItsSignature(t *testing.T) {
	a := newEnv(t)
	refused := func(name string, e *env, id, ticket string) {
		t.Helper()
		if rec := e.doUpgrade(id, "/vnc?ticket="+ticket); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: %d, want 401", name, rec.Code)
		}
	}

	// A replica with another key (API_SIGNING_KEY not shared).
	stranger := a.replica("c", []byte("another key, of the same length!"))
	refused("a ticket signed with another key", stranger, a.id, a.ticket(t, alice))

	// Changed in any part.
	ticket := a.ticket(t, alice)
	payload, signature, _ := strings.Cut(ticket, ".")
	raw, _ := base64.RawURLEncoding.DecodeString(payload)
	other, err := a.store.Create(t.Context(), "b", bob)
	if err != nil {
		t.Fatal(err)
	}
	sessionstest.SetStatus(t, a.client, other.ID, sessionstest.Ready("10.0.0.8"))
	forged := base64.RawURLEncoding.EncodeToString([]byte(strings.Replace(string(raw), a.id, other.ID, 1)))
	refused("a ticket rewritten for another session", a, other.ID, forged+"."+signature)
	refused("a ticket with its signature cut short", a, a.id, payload+"."+signature[:len(signature)-2])
	refused("a ticket with no signature", a, a.id, payload)
	refused("a ticket for another session, as issued", a, other.ID, ticket)

	// Expired: good for ten seconds.
	ticket = a.ticket(t, alice)
	a.skew.Add(int64(ticketTTL))
	refused("a ticket past its ten seconds", a, a.id, ticket)

	// The ticket never names its user in the clear: it travels in a URL.
	if strings.Contains(string(raw), "alice") || strings.Contains(string(raw), "@") {
		t.Errorf("the ticket carries its user's address: %q", raw)
	}
}

// A viewer open at one replica keeps the session awake under the sweep of
// another, for as long as it is open.
func TestAViewerAtOneReplicaHoldsTheSessionUnderAnothersSweep(t *testing.T) {
	a := newEnv(t)
	b := a.replica("b", ticketKey)

	resp, conn, br := b.websocket(t, "/vnc?ticket="+a.ticket(t, alice))
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade: %d", resp.StatusCode)
	}
	_, _ = io.WriteString(conn, "ping")
	echo := make([]byte, 4)
	if _, err := io.ReadFull(br, echo); err != nil || string(echo) != "ping" {
		t.Fatalf("echo through the proxy: %q, %v", echo, err)
	}

	// An hour passes, b's heartbeat with it; a sweeps.
	for range 6 {
		a.skew.Add(int64(10 * time.Minute))
		b.tracker.Beat(t.Context())
		if err := a.sweep(); err != nil {
			t.Fatal(err)
		}
		if s, _ := a.store.Get(t.Context(), a.id); s.State != sessions.Running {
			t.Fatalf("put to sleep with a viewer open at another replica: %s", s.State)
		}
	}

	// The viewer leaves; one idle period later the session sleeps.
	_ = conn.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		a.skew.Add(int64(16 * time.Minute))
		b.tracker.Beat(t.Context())
		if err := a.sweep(); err != nil {
			t.Fatal(err)
		}
		if s, _ := a.store.Get(t.Context(), a.id); s.State != sessions.Running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the session never slept after the viewer left")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A call in flight at one replica holds the session under the sweep of
// another from the moment it is taken in: nothing has to beat first.
func TestACallAtOneReplicaHoldsTheSessionUnderAnothersSweep(t *testing.T) {
	a := newEnv(t)
	b := a.replica("b", ticketKey)
	started, release := make(chan struct{}), make(chan struct{})
	a.respondWith(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		_, _ = io.WriteString(w, "done")
	})

	a.skew.Add(int64(16 * time.Minute)) // idle, as far as anything says
	code := make(chan int, 1)
	go func() { code <- b.do("POST", "/mcp", alice, "{}").Code }()
	<-started

	// The sweep comes with no heartbeat of b's in between.
	if err := idle.Sweep(t.Context(), a.store, rule, a.now); err != nil {
		t.Fatal(err)
	}
	s, _ := a.store.Get(t.Context(), a.id)
	if s.State != sessions.Running {
		t.Fatalf("put to sleep under a call in flight at another replica: %s", s.State)
	}
	if !s.InFlightElsewhere("test", a.now()) {
		t.Errorf("the call is not written on the session: %v", s.InFlight)
	}
	if a.proxy.Calls(a.id) != 0 || b.proxy.Calls(a.id) != 1 {
		t.Errorf("calls in flight: a %d, b %d; want 0 and 1", a.proxy.Calls(a.id), b.proxy.Calls(a.id))
	}
	close(release)
	if got := <-code; got != http.StatusOK {
		t.Fatalf("the call: %d", got)
	}
}

// One replica put the session to sleep; a call arrives at another, which
// remembers the session as running. It sees that it is not, in the answer
// to its own write, and wakes it rather than proxy to a pod that is gone.
func TestACallAtAReplicaThatMissedTheSleepWakesTheSession(t *testing.T) {
	a := newEnv(t)
	b := a.replica("b", ticketKey)
	b.proxy.Waker.RunningTTL = time.Hour // b remembers what it saw
	if rec := b.do("POST", "/mcp", alice, "{}"); rec.Code != http.StatusOK {
		t.Fatalf("first call: %d", rec.Code)
	}

	a.skew.Add(int64(16 * time.Minute))
	if err := a.sweep(); err != nil {
		t.Fatal(err)
	}
	sessionstest.SetStatus(t, a.client, a.id, sessionstest.Suspended())
	if s, _ := a.store.Get(t.Context(), a.id); s.State != sessions.Asleep {
		t.Fatalf("state = %s, want asleep", s.State)
	}

	controller := readyOnceResumed(t.Context(), a.store, a.client, a.id, "10.0.0.9")
	if rec := b.do("POST", "/mcp", alice, "{}"); rec.Code != http.StatusOK {
		t.Fatalf("a call after another replica's sleep: %d %s", rec.Code, rec.Body)
	}
	if err := <-controller; err != nil {
		t.Fatal(err)
	}
	if s, _ := a.store.Get(t.Context(), a.id); s.State != sessions.Running || s.LastActive.IsZero() {
		t.Fatalf("after the call: %s, last active %s; want running, with its clock started", s.State, s.LastActive)
	}
}

// Two replicas are asked for a sleeping session at once. Each wakes it; the
// session is resumed once and both calls are served.
func TestTwoReplicasWakingOneSessionAtOnce(t *testing.T) {
	a := newEnv(t)
	b := a.replica("b", ticketKey)
	a.asleep(t)
	for _, e := range []*env{a, b} {
		e.proxy.Waker.Timeout = 5 * time.Second
	}
	controller := readyOnceResumed(t.Context(), a.store, a.client, a.id, "10.0.0.9")

	var wg sync.WaitGroup
	codes := make([]int, 8)
	for i := range codes {
		e := []*env{a, b}[i%2]
		wg.Go(func() { codes[i] = e.do("POST", "/mcp", alice, "{}").Code })
	}
	wg.Wait()
	if err := <-controller; err != nil {
		t.Fatal(err)
	}
	for i, code := range codes {
		if code != http.StatusOK {
			t.Errorf("call %d: %d", i, code)
		}
	}
	if s, _ := a.store.Get(t.Context(), a.id); s.State != sessions.Running || s.StoppedBy != "" {
		t.Fatalf("after the calls: %s, stopped by %q; want running", s.State, s.StoppedBy)
	}
	if len(a.seen()) != len(codes) {
		t.Errorf("%d calls reached the pod, want %d", len(a.seen()), len(codes))
	}
}
