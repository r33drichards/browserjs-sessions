package idle_test

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"

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
	quiet, _ := store.Create(ctx, "quiet", "u2")
	starting, err := store.Create(ctx, "starting", "u") // never ready
	if err != nil {
		t.Fatal(err)
	}
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

// The user's stop and the idle sweep can land in either order; either way
// the session ends up stopped by the user, so it is not woken on demand.
func TestSweepNeverTakesOverAUserStop(t *testing.T) {
	ctx := t.Context()
	setup := func(t *testing.T) (*sessions.Store, dynamic.Interface, *idle.Tracker, string) {
		store, client := sessionstest.New(t)
		now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
		tracker := idle.New(15*time.Minute, func() time.Time { return now })
		s, err := store.Create(ctx, "a", "u")
		if err != nil {
			t.Fatal(err)
		}
		sessionstest.SetStatus(t, client, s.ID, sessionstest.Ready("10.0.0.1"))
		tracker.Touch(s.ID)
		now = now.Add(16 * time.Minute) // idle long enough to be put to sleep
		return store, client, tracker, s.ID
	}
	check := func(t *testing.T, store *sessions.Store, client dynamic.Interface, id string) {
		t.Helper()
		obj, err := client.Resource(sessions.SandboxGVR).Namespace(sessionstest.Namespace).Get(ctx, id, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if by := obj.GetAnnotations()[sessions.AnnStoppedBy]; by != sessions.StoppedByUser {
			t.Errorf("stopped-by = %q, want user", by)
		}
		sessionstest.SetStatus(t, client, id, sessionstest.Suspended())
		if s, _ := store.Get(ctx, id); s.State != sessions.Stopped {
			t.Errorf("state = %s, want stopped", s.State)
		}
	}

	t.Run("the user stops while the sweep is deciding", func(t *testing.T) {
		store, client, tracker, id := setup(t)
		// The sweep lists the session as running; the stop lands before the
		// sweep's suspend is written.
		sessionstest.RaceNextGet(t, client, id, sessionstest.UserStop)
		if err := idle.Sweep(ctx, store, tracker); err != nil {
			t.Fatal(err)
		}
		check(t, store, client, id)
	})

	t.Run("the sweep suspends, then the user stops", func(t *testing.T) {
		store, client, tracker, id := setup(t)
		if err := idle.Sweep(ctx, store, tracker); err != nil {
			t.Fatal(err)
		}
		if err := store.Suspend(ctx, id, sessions.StoppedByUser); err != nil {
			t.Fatal(err)
		}
		if err := idle.Sweep(ctx, store, tracker); err != nil {
			t.Fatal(err)
		}
		check(t, store, client, id)
	})
}
