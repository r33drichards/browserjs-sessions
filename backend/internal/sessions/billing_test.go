package sessions_test

import (
	"errors"
	"testing"

	"github.com/r33drichards/computer-use/backend/internal/sessions"
	"github.com/r33drichards/computer-use/backend/internal/sessions/sessionstest"
)

// Billing's reasons for a sleep (docs/contracts/billing/enforcement.md): a
// session asleep for credit or for want of a payment method wakes like an
// idle one; one whose owner is blocked stays stopped.
func TestSleepRecordsItsReason(t *testing.T) {
	ctx := t.Context()
	for reason, want := range map[string]struct {
		state sessions.State
		wakes bool
	}{
		sessions.StoppedByIdle:          {sessions.Asleep, true},
		sessions.StoppedByCredit:        {sessions.Asleep, true},
		sessions.StoppedByPaymentMethod: {sessions.Asleep, true},
		sessions.StoppedByBlocked:       {sessions.Stopped, false},
	} {
		t.Run(reason, func(t *testing.T) {
			store, client := sessionstest.New(t)
			s, _ := store.Create(ctx, "a", "user-1")
			sessionstest.SetStatus(t, client, s.ID, sessionstest.Ready("10.0.0.1"))
			if err := store.SetDraining(ctx, s.ID, sessions.StoppedByCredit); err != nil {
				t.Fatal(err)
			}
			if err := store.Sleep(ctx, s.ID, reason, nil); err != nil {
				t.Fatal(err)
			}
			obj := raw(t, client, s.ID)
			if got := obj.GetAnnotations()[sessions.AnnStoppedBy]; got != reason || mode(obj) != "Suspended" {
				t.Fatalf("after sleep: stopped-by %q, mode %s", got, mode(obj))
			}
			// The draining mark goes with the sleep.
			if _, marked := obj.GetAnnotations()[sessions.AnnDraining]; marked {
				t.Errorf("still marked draining: %v", obj.GetAnnotations())
			}
			sessionstest.SetStatus(t, client, s.ID, sessionstest.Suspended())
			got, err := store.Get(ctx, s.ID)
			if err != nil || got.State != want.state || got.StoppedBy != reason {
				t.Errorf("session = %s stopped by %q (%v), want %s by %q", got.State, got.StoppedBy, err, want.state, reason)
			}
			err = store.Wake(ctx, s.ID)
			if want.wakes && (err != nil || mode(raw(t, client, s.ID)) != "Running") {
				t.Errorf("wake: %v, mode %s", err, mode(raw(t, client, s.ID)))
			}
			if !want.wakes && !errors.Is(err, sessions.ErrStateChanged) {
				t.Errorf("wake: %v, want ErrStateChanged", err)
			}
			// Its user can always resume it; whether the account allows
			// that is asked before, not here.
			if err := store.Resume(ctx, s.ID); err != nil || mode(raw(t, client, s.ID)) != "Running" {
				t.Errorf("resume: %v", err)
			}
		})
	}

	t.Run("an unknown reason is refused", func(t *testing.T) {
		store, client := sessionstest.New(t)
		s, _ := store.Create(ctx, "a", "user-1")
		if err := store.Sleep(ctx, s.ID, "boredom", nil); err == nil {
			t.Fatal("slept for no known reason")
		}
		if err := store.Sleep(ctx, s.ID, sessions.StoppedByUser, nil); err == nil {
			t.Fatal("a user's stop is not a sleep")
		}
		if mode(raw(t, client, s.ID)) != "Running" {
			t.Error("the session was suspended")
		}
	})
}

func TestSetDraining(t *testing.T) {
	ctx := t.Context()
	store, client := sessionstest.New(t)
	s, _ := store.Create(ctx, "a", "user-1")
	sessionstest.SetStatus(t, client, s.ID, sessionstest.Ready("10.0.0.1"))

	if got, _ := store.Get(ctx, s.ID); got.Draining != "" || !got.DrainingSince.IsZero() {
		t.Fatalf("a new session is draining: %+v", got)
	}
	if err := store.SetDraining(ctx, s.ID, sessions.StoppedByCredit); err != nil {
		t.Fatal(err)
	}
	marked, _ := store.Get(ctx, s.ID)
	if marked.Draining != sessions.StoppedByCredit || marked.DrainingSince.IsZero() || marked.State != sessions.Running {
		t.Fatalf("after the mark: %+v", marked)
	}
	// Marking again for the same reason keeps the time and writes nothing.
	before := writes(client)
	if err := store.SetDraining(ctx, s.ID, sessions.StoppedByCredit); err != nil || writes(client) != before {
		t.Errorf("second mark: %v, %d writes", err, writes(client)-before)
	}
	for _, reason := range []string{"boredom", sessions.StoppedByIdle, sessions.StoppedByUser} {
		if err := store.SetDraining(ctx, s.ID, reason); err == nil {
			t.Errorf("marked draining for %q", reason)
		}
	}
	if err := store.SetDraining(ctx, s.ID, ""); err != nil {
		t.Fatal(err)
	}
	obj := raw(t, client, s.ID)
	if _, ok := obj.GetAnnotations()[sessions.AnnDraining]; ok {
		t.Errorf("mark not removed: %v", obj.GetAnnotations())
	}
	if _, ok := obj.GetAnnotations()[sessions.AnnDrainingSince]; ok {
		t.Errorf("time not removed: %v", obj.GetAnnotations())
	}

	// A suspended session has nothing to drain.
	if err := store.Suspend(ctx, s.ID, sessions.StoppedByUser); err != nil {
		t.Fatal(err)
	}
	if err := store.SetDraining(ctx, s.ID, sessions.StoppedByCredit); !errors.Is(err, sessions.ErrStateChanged) {
		t.Errorf("err = %v, want ErrStateChanged", err)
	}
	if err := store.SetDraining(ctx, "s-aaaaaaaaaa", sessions.StoppedByCredit); !errors.Is(err, sessions.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}
