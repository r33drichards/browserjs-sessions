package proxy

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/r33drichards/browserjs-sessions/backend/internal/sessions"
	"github.com/r33drichards/browserjs-sessions/backend/internal/sessions/sessionstest"
)

func TestEnsureAwake(t *testing.T) {
	ctx := t.Context()
	store, client := sessionstest.New(t)
	w := &Waker{Store: store, Timeout: 2 * time.Second, Poll: 10 * time.Millisecond}

	s, _ := store.Create(ctx, "a", "user-1")

	// Already running: returned as is.
	sessionstest.SetStatus(t, client, s.ID, sessionstest.Ready("10.0.0.7"))
	got, err := w.EnsureAwake(ctx, s.ID)
	if err != nil || got.PodIP != "10.0.0.7" {
		t.Fatalf("running: %+v, %v", got, err)
	}

	// Asleep: resumed, and the call returns once the controller reports ready.
	_ = store.Suspend(ctx, s.ID, sessions.StoppedByIdle)
	sessionstest.SetStatus(t, client, s.ID, sessionstest.Suspended())
	go func() {
		// Play the controller: wait for the resume, then report ready.
		for {
			if cur, _ := store.Get(context.Background(), s.ID); cur.State == sessions.Starting {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		sessionstest.SetStatus(t, client, s.ID, sessionstest.Ready("10.0.0.8"))
	}()
	got, err = w.EnsureAwake(ctx, s.ID)
	if err != nil || got.PodIP != "10.0.0.8" {
		t.Fatalf("asleep: %+v, %v", got, err)
	}

	// Stopped by the user: not woken.
	_ = store.Suspend(ctx, s.ID, sessions.StoppedByUser)
	sessionstest.SetStatus(t, client, s.ID, sessionstest.Suspended())
	if _, err := w.EnsureAwake(ctx, s.ID); !errors.Is(err, ErrStopped) {
		t.Errorf("stopped: err = %v, want ErrStopped", err)
	}

	if _, err := w.EnsureAwake(ctx, "s-missing000"); !errors.Is(err, sessions.ErrNotFound) {
		t.Errorf("missing: err = %v", err)
	}
}

func TestEnsureAwakeTimesOut(t *testing.T) {
	ctx := t.Context()
	store, _ := sessionstest.New(t)
	w := &Waker{Store: store, Timeout: 50 * time.Millisecond, Poll: 10 * time.Millisecond}
	s, _ := store.Create(ctx, "a", "user-1") // never becomes ready
	if _, err := w.EnsureAwake(ctx, s.ID); !errors.Is(err, ErrNotReady) {
		t.Errorf("err = %v, want ErrNotReady", err)
	}
}
