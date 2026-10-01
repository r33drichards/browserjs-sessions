package idle

import (
	"slices"
	"testing"
	"time"
)

func TestIdle(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	tr := New(15*time.Minute, func() time.Time { return now })

	tr.Touch("a")
	tr.Touch("b")
	now = now.Add(10 * time.Minute)
	tr.Touch("b")
	now = now.Add(6 * time.Minute) // a idle 16m, b idle 6m

	// c has never been seen (e.g. the backend restarted): it gets a full
	// idle period from first sight, not an immediate suspend.
	if got := tr.Idle([]string{"a", "b", "c"}); !slices.Equal(got, []string{"a"}) {
		t.Errorf("Idle = %v, want [a]", got)
	}
	now = now.Add(15 * time.Minute)
	if got := tr.Idle([]string{"b", "c"}); !slices.Equal(got, []string{"b", "c"}) {
		t.Errorf("Idle = %v, want [b c]", got)
	}
}

func TestOpenConnectionKeepsSessionAwake(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	tr := New(15*time.Minute, func() time.Time { return now })

	done := tr.Open("a") // e.g. a VNC viewer
	now = now.Add(time.Hour)
	if got := tr.Idle([]string{"a"}); len(got) != 0 {
		t.Errorf("session with an open connection reported idle: %v", got)
	}
	done()
	done() // closing twice must not go negative
	if got := tr.Idle([]string{"a"}); len(got) != 0 {
		t.Errorf("idle immediately after close: %v (closing counts as activity)", got)
	}
	now = now.Add(16 * time.Minute)
	if got := tr.Idle([]string{"a"}); !slices.Equal(got, []string{"a"}) {
		t.Errorf("Idle = %v, want [a]", got)
	}
}

func TestForget(t *testing.T) {
	now := time.Now()
	tr := New(time.Minute, func() time.Time { return now })
	tr.Touch("a")
	tr.Forget("a")
	now = now.Add(2 * time.Minute)
	if got := tr.Idle([]string{"a"}); len(got) != 0 {
		t.Errorf("forgotten session treated as known: %v", got)
	}
}

func TestRetain(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	tr := New(15*time.Minute, func() time.Time { return now })

	tr.Touch("kept")
	tr.Touch("gone")
	done := tr.Open("gone-open") // a connection left over from a deleted session
	tr.Retain([]string{"kept", "never-seen"})

	if _, ok := tr.last["kept"]; !ok || len(tr.last) != 1 || len(tr.open) != 0 {
		t.Errorf("after Retain: last = %v, open = %v; want only kept", tr.last, tr.open)
	}
	done() // closing a connection of a dropped session must not leave a count behind
	if len(tr.open) != 0 {
		t.Errorf("open = %v after closing a dropped session's connection", tr.open)
	}
	tr.Retain([]string{"kept"})
	now = now.Add(16 * time.Minute)
	// kept is still on its old clock; the dropped ones start over.
	if got := tr.Idle([]string{"kept", "gone", "gone-open"}); !slices.Equal(got, []string{"kept"}) {
		t.Errorf("Idle = %v, want [kept]", got)
	}
}

// A session that slept and was resumed gets a full idle period: its clock
// must not still be running from before it went to sleep.
func TestResumedSessionStartsAFreshIdlePeriod(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	tr := New(15*time.Minute, func() time.Time { return now })

	tr.Touch("a")
	now = now.Add(16 * time.Minute)
	if got := tr.Idle([]string{"a"}); !slices.Equal(got, []string{"a"}) {
		t.Fatalf("Idle = %v, want [a]", got)
	}
	// It is put to sleep; sweeps go by while it is not running.
	now = now.Add(time.Hour)
	if got := tr.Idle(nil); len(got) != 0 {
		t.Fatalf("Idle = %v", got)
	}
	// The user resumes it (nothing touches the tracker on that path).
	if got := tr.Idle([]string{"a"}); len(got) != 0 {
		t.Errorf("a just-resumed session was reported idle: %v", got)
	}
	now = now.Add(16 * time.Minute)
	if got := tr.Idle([]string{"a"}); !slices.Equal(got, []string{"a"}) {
		t.Errorf("Idle = %v, want [a]", got)
	}
}

// A viewer attached to a session that is not running yet (it is waking)
// still holds it awake once it runs.
func TestOpenConnectionOnAWakingSession(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	tr := New(15*time.Minute, func() time.Time { return now })
	done := tr.Open("a")
	tr.Idle(nil) // a sweep while it is still starting
	now = now.Add(time.Hour)
	if got := tr.Idle([]string{"a"}); len(got) != 0 {
		t.Errorf("session with a viewer reported idle: %v", got)
	}
	done()
	now = now.Add(16 * time.Minute)
	if got := tr.Idle([]string{"a"}); !slices.Equal(got, []string{"a"}) {
		t.Errorf("Idle = %v, want [a]", got)
	}
}

func TestClosingAConnectionOfAForgottenSession(t *testing.T) {
	tr := New(time.Minute, time.Now)
	done := tr.Open("a")
	tr.Forget("a")
	done()
	if len(tr.last) != 0 || len(tr.open) != 0 {
		t.Errorf("a deleted session is tracked again: last = %v, open = %v", tr.last, tr.open)
	}
}
