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
