package idle_test

import (
	"testing"
	"time"

	"github.com/r33drichards/browserjs-sessions/backend/internal/idle"
	"github.com/r33drichards/browserjs-sessions/backend/internal/sessions"
	"github.com/r33drichards/browserjs-sessions/backend/internal/sessions/sessionstest"
)

func TestSweepSuspendsOnlyIdleRunningSessions(t *testing.T) {
	ctx := t.Context()
	store, client := sessionstest.New(t)
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	tracker := idle.New(15*time.Minute, func() time.Time { return now })

	busy, _ := store.Create(ctx, "busy", "u")
	quiet, _ := store.Create(ctx, "quiet", "u")
	starting, _ := store.Create(ctx, "starting", "u") // never ready
	for _, id := range []string{busy.ID, quiet.ID} {
		sessionstest.SetStatus(t, client, id, sessionstest.Ready("10.0.0.1"))
	}

	if err := idle.Sweep(ctx, store, tracker); err != nil { // first sight starts the clock
		t.Fatal(err)
	}
	now = now.Add(16 * time.Minute)
	tracker.Touch(busy.ID)
	if err := idle.Sweep(ctx, store, tracker); err != nil {
		t.Fatal(err)
	}

	state := func(id string) sessions.State { s, _ := store.Get(ctx, id); return s.State }
	if state(busy.ID) != sessions.Running {
		t.Errorf("busy session was suspended")
	}
	if state(quiet.ID) != sessions.Stopping { // fake cluster never finishes the suspend
		t.Errorf("quiet session state = %s", state(quiet.ID))
	}
	if state(starting.ID) != sessions.Starting {
		t.Errorf("a session that is still starting was suspended")
	}
}

func TestSweepForgetsDeletedSessions(t *testing.T) {
	ctx := t.Context()
	store, client := sessionstest.New(t)
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	tracker := idle.New(15*time.Minute, func() time.Time { return now })

	kept, _ := store.Create(ctx, "kept", "u")
	gone, _ := store.Create(ctx, "gone", "u")
	sessionstest.SetStatus(t, client, kept.ID, sessionstest.Ready("10.0.0.1"))
	tracker.Touch(kept.ID)
	tracker.Touch(gone.ID)

	now = now.Add(16 * time.Minute)
	if err := store.Delete(ctx, gone.ID); err != nil {
		t.Fatal(err)
	}
	if err := idle.Sweep(ctx, store, tracker); err != nil {
		t.Fatal(err)
	}

	// The deleted session was forgotten: seeing its ID again starts a fresh
	// idle period instead of counting from the old activity.
	if got := tracker.Idle([]string{gone.ID}); len(got) != 0 {
		t.Errorf("deleted session still tracked: %v", got)
	}
	// Sessions that still exist keep their clock (and this one was suspended).
	if s, _ := store.Get(ctx, kept.ID); s.State != sessions.Stopping {
		t.Errorf("kept session state = %s, want stopping", s.State)
	}
}
