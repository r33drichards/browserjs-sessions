// Package idle decides which sessions have gone unused long enough to sleep.
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
			t.last[id] = t.now()
		})
	}
}

// Forget drops a deleted session.
func (t *Tracker) Forget(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.last, id)
	delete(t.open, id)
}

// Idle returns which of the given running sessions should be put to sleep.
// A session seen for the first time starts its idle period now.
func (t *Tracker) Idle(running []string) []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
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
