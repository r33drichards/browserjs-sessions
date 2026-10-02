package scenarios

import (
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/r33drichards/computer-use/backend/internal/billing"
	"github.com/r33drichards/computer-use/backend/internal/billing/billingtest"
	"github.com/r33drichards/computer-use/backend/internal/sessions"
)

// exhausted is the start of scenario 2: an active account with one running
// session and a balance of 3000 micro-dollars, BILLING_GRACE 5 m and
// BILLING_DRAIN_TIMEOUT 10 m, taken through steps 1 and 2: the balance
// runs out, and nothing is stopped yet.
func exhausted(t *testing.T) (w *world, id string, exhaustedAt time.Time) {
	t.Helper()
	w = newWorld(t, func(c *billing.Config) {
		c.Grace, c.DrainTimeout = 5*time.Minute, 10*time.Minute
		c.SignupCredit = false // the balance is the 3000 granted below, and no more
	})
	w.saveCard(user, billingtest.Card{Fingerprint: "fpA"})
	w.grant(user, 3000)
	w.tick()
	if b := w.billingOf(user); b.State != billing.StateActive || b.BalanceMicros != 3000 {
		t.Fatalf("setup: state %s, balance %d", b.State, b.BalanceMicros)
	}
	id = w.create(user)

	// 1: a minute awake costs 3333: the balance is gone.
	w.clock.Advance(time.Minute)
	w.tick()
	b := w.billingOf(user)
	if b.Level != billing.LevelExhausted || b.ExhaustedAt == nil || b.BalanceMicros != 0 {
		t.Fatalf("1: level %s, exhaustedAt %v, balance %d", b.Level, b.ExhaustedAt, b.BalanceMicros)
	}
	if b.SleepAt == nil || !b.SleepAt.Equal(b.ExhaustedAt.Add(5*time.Minute)) {
		t.Errorf("1: sleepAt %v, want exhaustedAt + the grace", b.SleepAt)
	}
	w.sweep()
	if s := w.session(id); s.State != sessions.Running || s.Draining != "" || w.sessions.Snapshots(id) != 0 {
		t.Fatalf("1: the sweep acted inside the grace: %s, draining %q", s.State, s.Draining)
	}
	if res := w.mcp(user, id); res.Code != http.StatusOK || w.podCalls.Load() != 1 {
		t.Fatalf("1: a call inside the grace was not forwarded: %d %s", res.Code, res.body())
	}

	// 2: but nothing new can be started.
	refusedWith(t, "2", w.api(user, "POST", "/api/sessions", `{}`), http.StatusPaymentRequired, billing.CodeOutOfCredit)
	return w, id, *b.ExhaustedAt
}

// draining takes exhausted through steps 3 and 4: the grace is over, a call
// is in flight, the sweep marks the session and takes no snapshot; new
// calls are refused.
func draining(t *testing.T) (w *world, id string) {
	t.Helper()
	w, id, exhaustedAt := exhausted(t)

	// 3: the grace is over, with a call in flight.
	w.clock.Set(exhaustedAt.Add(5 * time.Minute))
	w.calls.SetCalls(id, 1)
	forwarded := w.podCalls.Load()
	w.sweep()
	if s := w.session(id); s.State != sessions.Running || s.Draining != sessions.StoppedByCredit || w.sessions.Snapshots(id) != 0 {
		t.Fatalf("3: %s, draining %q, %d snapshots; want running, credit, none", s.State, s.Draining, w.sessions.Snapshots(id))
	}
	if w.calls.Closed(id) == 0 {
		t.Fatalf("3: the session's streams were not closed")
	}
	if v := w.view(user, id); v.State != "running" || v.Draining != "credit" {
		t.Fatalf("3: GET shows %+v", v)
	}

	// 4: a new call is refused, not forwarded.
	mcpRefused(t, "4", w.mcp(user, id), http.StatusPaymentRequired, "out of credit")
	if w.podCalls.Load() != forwarded {
		t.Fatalf("4: the call reached the pod")
	}
	return w, id
}

func asleepForCredit(t *testing.T, step string, w *world, id string) {
	t.Helper()
	if s := w.session(id); w.sessions.Snapshots(id) != 1 || s.State != sessions.Asleep || s.StoppedBy != sessions.StoppedByCredit || s.Draining != "" {
		t.Fatalf("%s: %d snapshots, %s, stopped by %q, draining %q; want one, asleep, credit, no mark",
			step, w.sessions.Snapshots(id), s.State, s.StoppedBy, s.Draining)
	}
}

// TestExhaustionDrain is scenario 2 of docs/contracts/billing/testing.md:
// credit runs out with a call in flight. The call finishes; only then is
// the session snapshotted and put to sleep.
func TestExhaustionDrain(t *testing.T) {
	t.Run("5: the call ends, then the session sleeps", func(t *testing.T) {
		w, id := draining(t)
		w.calls.SetCalls(id, 0)
		w.sweep()
		asleepForCredit(t, "5", w, id)
		if v := w.view(user, id); v.State != "asleep" || v.StoppedBy != "credit" || v.Draining != "" {
			t.Fatalf("5: GET shows %+v", v)
		}
		// Asleep for credit, it is not woken by a call either.
		mcpRefused(t, "5", w.mcp(user, id), http.StatusPaymentRequired, "out of credit")
		if w.sessions.Wakes() != 0 {
			t.Fatalf("5: Wake was called")
		}

		// 7: credit arrives. The session stays asleep; resume now works.
		w.buy(user, "cu_credit_5_v1")
		w.tick()
		w.sweep()
		if s := w.session(id); s.State != sessions.Asleep {
			t.Fatalf("7: credit arriving woke the session: %s", s.State)
		}
		res := w.api(user, "PATCH", "/api/sessions/"+id, `{"action":"resume"}`)
		if res.Code != http.StatusOK || w.session(id).State != sessions.Running {
			t.Fatalf("7: resume = %d %s; the session is %s", res.Code, res.body(), w.session(id).State)
		}
	})

	t.Run("5': the call never ends; the drain times out", func(t *testing.T) {
		w, id := draining(t)
		w.clock.Advance(9 * time.Minute)
		w.sweep()
		if s := w.session(id); s.State != sessions.Running || w.sessions.Snapshots(id) != 0 {
			t.Fatalf("5': put to sleep before the timeout: %s", s.State)
		}
		w.clock.Advance(time.Minute) // BILLING_DRAIN_TIMEOUT since the mark
		w.sweep()
		asleepForCredit(t, "5'", w, id)
	})

	t.Run("6: credit arrives during the drain; the stop is called off", func(t *testing.T) {
		w, id := draining(t)
		forwarded := w.podCalls.Load()
		w.buy(user, "cu_credit_5_v1")
		w.tick()
		w.sweep()
		if s := w.session(id); s.State != sessions.Running || s.Draining != "" || w.sessions.Snapshots(id) != 0 {
			t.Fatalf("6: %s, draining %q, %d snapshots; want running, no mark, none", s.State, s.Draining, w.sessions.Snapshots(id))
		}
		if res := w.mcp(user, id); res.Code != http.StatusOK || w.podCalls.Load() != forwarded+1 {
			t.Fatalf("6: a new call was not forwarded: %d %s", res.Code, res.body())
		}
		// And nothing stops it later.
		w.calls.SetCalls(id, 0)
		w.sweep()
		if s := w.session(id); s.State != sessions.Running {
			t.Fatalf("6: the session was stopped after its credit came back: %s", s.State)
		}
	})

	// The same with nothing said by the test: the call is a real one,
	// held open in the pod, and counted by the proxy itself.
	t.Run("a real call in flight finishes before the sleep", func(t *testing.T) {
		w, id, exhaustedAt := exhausted(t)
		arrived, release := make(chan struct{}), make(chan struct{})
		// Whatever happens, the pod answers before the test ends.
		var once sync.Once
		done := func() { once.Do(func() { close(release) }) }
		t.Cleanup(done)
		slow := http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
			close(arrived)
			<-release
			rw.Header().Set("Content-Type", "application/json")
			_, _ = rw.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"done":true}}`))
		})
		w.pod.Store(&slow)
		answered := make(chan response, 1)
		go func() { answered <- w.mcp(user, id) }()
		<-arrived
		if got := w.proxy.Calls(id); got != 1 {
			t.Fatalf("the proxy counts %d calls in flight, want 1", got)
		}

		w.clock.Set(exhaustedAt.Add(5 * time.Minute))
		w.sweep()
		if s := w.session(id); s.State != sessions.Running || s.Draining != sessions.StoppedByCredit || w.sessions.Snapshots(id) != 0 {
			t.Fatalf("with the call in flight: %s, draining %q, %d snapshots", s.State, s.Draining, w.sessions.Snapshots(id))
		}
		w.pod.Store(nil)
		mcpRefused(t, "a new call", w.mcp(user, id), http.StatusPaymentRequired, "out of credit")

		done()
		res := <-answered
		if res.Code != http.StatusOK || res.body() != `{"jsonrpc":"2.0","id":1,"result":{"done":true}}` {
			t.Fatalf("the call in flight did not finish: %d %s", res.Code, res.body())
		}
		if got := w.proxy.Calls(id); got != 0 {
			t.Fatalf("the proxy still counts %d calls in flight", got)
		}
		w.sweep()
		asleepForCredit(t, "after the call", w, id)
	})

	t.Run("a session that is still starting is suspended at once", func(t *testing.T) {
		w, id, exhaustedAt := exhausted(t)
		w.sessions.SetState(id, sessions.Starting)
		w.clock.Set(exhaustedAt.Add(5 * time.Minute))
		w.calls.SetCalls(id, 1)
		w.sweep()
		if s := w.session(id); s.State != sessions.Asleep || s.StoppedBy != sessions.StoppedByCredit || w.sessions.Snapshots(id) != 0 {
			t.Fatalf("%s, stopped by %q, %d snapshots; want asleep, credit, no snapshot", s.State, s.StoppedBy, w.sessions.Snapshots(id))
		}
	})
}
