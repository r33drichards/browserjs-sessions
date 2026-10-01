package proxy

import (
	"context"
	"errors"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	dynfake "k8s.io/client-go/dynamic/fake"

	"github.com/r33drichards/browserjs-sessions/backend/internal/sessions"
	"github.com/r33drichards/browserjs-sessions/backend/internal/sessions/sessionstest"
)

func TestEnsureAwake(t *testing.T) {
	ctx := t.Context()
	store, client := sessionstest.New(t)
	w := &Waker{Store: store, Timeout: 2 * time.Second, Poll: 10 * time.Millisecond}

	s, err := store.Create(ctx, "a", "user-1")
	if err != nil {
		t.Fatal(err)
	}

	// Already running: returned as is.
	sessionstest.SetStatus(t, client, s.ID, sessionstest.Ready("10.0.0.7"))
	got, err := w.EnsureAwake(ctx, s.ID)
	if err != nil || got.PodIP != "10.0.0.7" {
		t.Fatalf("running: %+v, %v", got, err)
	}

	// Asleep: resumed, and the call returns once the controller reports ready.
	_ = store.Suspend(ctx, s.ID, sessions.StoppedByIdle)
	sessionstest.SetStatus(t, client, s.ID, sessionstest.Suspended())
	controller := readyOnceResumed(ctx, store, client, s.ID, "10.0.0.8")
	before := writes(client)
	got, err = w.EnsureAwake(ctx, s.ID)
	if err != nil || got.PodIP != "10.0.0.8" {
		t.Fatalf("asleep: %+v, %v", got, err)
	}
	if err := <-controller; err != nil {
		t.Fatal(err)
	}
	// One resume, plus the controller's status write: not one per poll.
	if n := writes(client) - before; n != 2 {
		t.Errorf("waking took %d writes, want 2 (resume, status)", n)
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

// readyOnceResumed plays the controller: it waits for the session to be
// resumed, then reports it ready on podIP. The channel yields its outcome.
func readyOnceResumed(ctx context.Context, store *sessions.Store, client dynamic.Interface, id, podIP string) <-chan error {
	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		for {
			if cur, err := store.Get(ctx, id); err != nil {
				done <- err
				return
			} else if cur.State == sessions.Starting {
				done <- sessionstest.TrySetStatus(client, id, sessionstest.Ready(podIP))
				return
			}
			select {
			case <-ctx.Done():
				done <- errors.New("the session was never resumed")
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
	}()
	return done
}

func writes(client dynamic.Interface) int {
	n := 0
	for _, a := range client.(*dynfake.FakeDynamicClient).Actions() {
		if v := a.GetVerb(); v == "patch" || v == "update" {
			n++
		}
	}
	return n
}

func TestEnsureAwakeFailedSession(t *testing.T) {
	ctx := t.Context()
	store, client := sessionstest.New(t)
	w := &Waker{Store: store, Timeout: 2 * time.Second, Poll: 10 * time.Millisecond}
	s, _ := store.Create(ctx, "a", "user-1")
	sessionstest.SetStatus(t, client, s.ID, map[string]any{"conditions": []any{
		map[string]any{"type": "Ready", "status": "False", "reason": "InvalidConfiguration", "message": "bad image"},
	}})
	if _, err := w.EnsureAwake(ctx, s.ID); !errors.Is(err, ErrFailed) {
		t.Errorf("err = %v, want ErrFailed", err)
	}
}

// The waker saw the session asleep, and the user stopped it before the
// resume was written: it must stay stopped, and the caller is told so.
func TestEnsureAwakeDoesNotResumeAUserStop(t *testing.T) {
	ctx := t.Context()
	store, client := sessionstest.New(t)
	w := &Waker{Store: store, Timeout: 300 * time.Millisecond, Poll: 10 * time.Millisecond}
	s, _ := store.Create(ctx, "a", "user-1")
	if err := store.Suspend(ctx, s.ID, sessions.StoppedByIdle); err != nil {
		t.Fatal(err)
	}
	sessionstest.SetStatus(t, client, s.ID, sessionstest.Suspended())

	sessionstest.RaceNextGet(t, client, s.ID, sessionstest.UserStop)
	if _, err := w.EnsureAwake(ctx, s.ID); !errors.Is(err, ErrStopped) {
		t.Errorf("err = %v, want ErrStopped", err)
	}
	obj, err := client.Resource(sessions.SandboxGVR).Namespace(sessionstest.Namespace).Get(ctx, s.ID, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	mode, _, _ := unstructured.NestedString(obj.Object, "spec", "operatingMode")
	if by := obj.GetAnnotations()[sessions.AnnStoppedBy]; mode != "Suspended" || by != sessions.StoppedByUser {
		t.Errorf("mode %s, stopped-by %q: the waker resumed a session its user had just stopped", mode, by)
	}
	if got, _ := store.Get(ctx, s.ID); got.State != sessions.Stopped {
		t.Errorf("state = %s, want stopped", got.State)
	}
}

// A caller that goes away is not a session that failed to start.
func TestEnsureAwakeCallerCancelled(t *testing.T) {
	store, _ := sessionstest.New(t)
	w := &Waker{Store: store, Timeout: 5 * time.Second, Poll: 10 * time.Millisecond}
	s, _ := store.Create(t.Context(), "a", "user-1") // never becomes ready

	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(30*time.Millisecond, cancel)
	if _, err := w.EnsureAwake(ctx, s.ID); !errors.Is(err, context.Canceled) || errors.Is(err, ErrNotReady) {
		t.Errorf("cancelled: err = %v, want context.Canceled", err)
	}

	ctx, cancel = context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if _, err := w.EnsureAwake(ctx, s.ID); !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrNotReady) {
		t.Errorf("caller's deadline: err = %v, want context.DeadlineExceeded", err)
	}
}

// A Waker with no settings waits with sensible defaults instead of giving up
// at once or spinning on the API server.
func TestEnsureAwakeDefaults(t *testing.T) {
	ctx := t.Context()
	store, client := sessionstest.New(t)
	w := &Waker{Store: store}
	s, _ := store.Create(ctx, "a", "user-1")
	_ = store.Suspend(ctx, s.ID, sessions.StoppedByIdle)
	sessionstest.SetStatus(t, client, s.ID, sessionstest.Suspended())

	controller := readyOnceResumed(ctx, store, client, s.ID, "10.0.0.9")
	gets := func() int {
		n := 0
		for _, a := range client.(*dynfake.FakeDynamicClient).Actions() {
			if a.GetVerb() == "get" {
				n++
			}
		}
		return n
	}
	before := gets()
	got, err := w.EnsureAwake(ctx, s.ID)
	if err != nil || got.PodIP != "10.0.0.9" {
		t.Fatalf("zero-valued waker: %+v, %v", got, err)
	}
	if err := <-controller; err != nil {
		t.Fatal(err)
	}
	// The controller helper polls every 5ms; the waker must not be spinning.
	if n := gets() - before; n > 500 {
		t.Errorf("%d reads while waiting: the waker is busy-looping", n)
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
