// Package idle decides which sessions have gone unused long enough to sleep.
//
// The tracker's state lives in this process only. The backend must run as
// exactly one replica: a second one would not see the activity the first
// proxied, and would put sessions to sleep while they are in use.
package idle

import (
	"sync"
	"time"
)

type Tracker struct {
	after time.Duration
	now   func() time.Time

	mu   sync.Mutex
	last map[string]time.Time
	open map[string]int
}

func New(after time.Duration, now func() time.Time) *Tracker {
	return &Tracker{after: after, now: now, last: map[string]time.Time{}, open: map[string]int{}}
}

// Touch records activity on a session (an MCP call, an upload).
func (t *Tracker) Touch(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.last[id] = t.now()
}

// Open marks a long-lived connection (a VNC viewer). The session cannot go
// idle until the returned func is called.
func (t *Tracker) Open(id string) (done func()) {
	t.mu.Lock()
	t.open[id]++
	t.last[id] = t.now()
	t.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			t.mu.Lock()
			defer t.mu.Unlock()
			if t.open[id]--; t.open[id] <= 0 {
				delete(t.open, id)
			}
			// Closing counts as activity, unless the session was dropped
			// (deleted) while the connection was open.
			if _, tracked := t.last[id]; tracked {
				t.last[id] = t.now()
			}
		})
	}
}

// Reset ends a session's idle period without touching its open connections:
// the next time it is seen running, a fresh period starts. The sweeper calls
// it for a session it has just put to sleep, which may be running again (its
// user resumed it) before a sweep ever sees it asleep.
func (t *Tracker) Reset(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.last, id)
}

// Forget drops a deleted session.
func (t *Tracker) Forget(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.last, id)
	delete(t.open, id)
}

// Retain drops every tracked session that is not in ids, so sessions that
// no longer exist do not accumulate.
func (t *Tracker) Retain(ids []string) {
	keep := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		keep[id] = struct{}{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for id := range t.last {
		if _, ok := keep[id]; !ok {
			delete(t.last, id)
		}
	}
	for id := range t.open {
		if _, ok := keep[id]; !ok {
			delete(t.open, id)
		}
	}
}

// Idle returns which of the given running sessions should be put to sleep.
// A session seen for the first time starts its idle period now, and so does
// one that stopped running in between (it slept, or was stopped and resumed):
// its old clock is dropped, unless a connection is still holding it open.
func (t *Tracker) Idle(running []string) []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	isRunning := make(map[string]struct{}, len(running))
	for _, id := range running {
		isRunning[id] = struct{}{}
	}
	for id := range t.last {
		if _, ok := isRunning[id]; !ok && t.open[id] == 0 {
			delete(t.last, id)
		}
	}
	var out []string
	for _, id := range running {
		last, known := t.last[id]
		if !known {
			t.last[id] = now
			continue
		}
		if t.open[id] == 0 && now.Sub(last) >= t.after {
			out = append(out, id)
		}
	}
	return out
}
