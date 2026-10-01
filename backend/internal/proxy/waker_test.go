package proxy

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	dynfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

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

// countGets counts the reads of Sandboxes the fake cluster serves from now on.
func countGets(client dynamic.Interface) *atomic.Int64 {
	n := &atomic.Int64{}
	client.(*dynfake.FakeDynamicClient).PrependReactor("get", sessions.SandboxGVR.Resource,
		func(k8stesting.Action) (bool, runtime.Object, error) {
			n.Add(1)
			return false, nil, nil
		})
	return n
}

// onResume returns a channel that is closed when the session's Sandbox is
// first written with operatingMode Running.
func onResume(client dynamic.Interface) <-chan struct{} {
	resumed := make(chan struct{})
	var once sync.Once
	client.(*dynfake.FakeDynamicClient).PrependReactor("update", sessions.SandboxGVR.Resource,
		func(action k8stesting.Action) (bool, runtime.Object, error) {
			obj := action.(k8stesting.UpdateAction).GetObject().(*unstructured.Unstructured)
			if mode, _, _ := unstructured.NestedString(obj.Object, "spec", "operatingMode"); mode == "Running" {
				once.Do(func() { close(resumed) })
			}
			return false, nil, nil
		})
	return resumed
}

func asleep(t *testing.T, store *sessions.Store, client dynamic.Interface) string {
	t.Helper()
	s, err := store.Create(t.Context(), "a", "user-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Suspend(t.Context(), s.ID, sessions.StoppedByIdle); err != nil {
		t.Fatal(err)
	}
	sessionstest.SetStatus(t, client, s.ID, sessionstest.Suspended())
	return s.ID
}

// However many requests wait for a sleeping session, the cluster sees one
// wake and one poll loop.
func TestConcurrentWaitersShareOneWake(t *testing.T) {
	ctx := t.Context()
	store, client := sessionstest.New(t)
	w := &Waker{Store: store, Timeout: 5 * time.Second, Poll: 20 * time.Millisecond, RunningTTL: time.Minute}
	id := asleep(t, store, client)
	resumed := onResume(client)
	before := writes(client)
	gets := countGets(client)

	const waiters = 25
	var wg sync.WaitGroup
	errs := make(chan error, waiters)
	for range waiters {
		wg.Go(func() {
			s, err := w.EnsureAwake(ctx, id)
			if err == nil && s.PodIP != "10.0.0.8" {
				err = fmt.Errorf("pod IP %q", s.PodIP)
			}
			errs <- err
		})
	}
	select {
	case <-resumed:
	case <-time.After(5 * time.Second):
		t.Fatal("the session was never resumed")
	}
	time.Sleep(100 * time.Millisecond) // a few polls go by before the pod is up
	polled := gets.Load()
	sessionstest.SetStatus(t, client, id, sessionstest.Ready("10.0.0.8")) // one read, one write
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("waiter: %v", err)
		}
	}

	if n := writes(client) - before; n != 2 {
		t.Errorf("%d writes, want 2 (one resume, the controller's status)", n)
	}
	// One loop: a read to see it asleep, one to resume it, then one per poll
	// (100ms at 20ms each, with slack for a slow machine).
	if polled > 12 {
		t.Errorf("%d reads while %d requests waited 100ms: each one is polling for itself", polled, waiters)
	}
	// The answer is remembered: latecomers do not ask again.
	after := gets.Load()
	for range waiters {
		if _, err := w.EnsureAwake(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	if n := gets.Load() - after; n != 0 {
		t.Errorf("%d reads for a session just seen running, want 0", n)
	}
}

// The wait is shared, so it must not belong to any one caller: the first to
// ask may hang up without failing the rest.
func TestCancelledWaiterDoesNotFailTheOthers(t *testing.T) {
	store, client := sessionstest.New(t)
	w := &Waker{Store: store, Timeout: 5 * time.Second, Poll: 10 * time.Millisecond}
	id := asleep(t, store, client)
	resumed := onResume(client)

	first, hangUp := context.WithCancel(t.Context())
	firstErr := make(chan error, 1)
	go func() { _, err := w.EnsureAwake(first, id); firstErr <- err }()
	<-resumed // the first caller's wait is under way

	type result struct {
		s   sessions.Session
		err error
	}
	second := make(chan result, 1)
	go func() { s, err := w.EnsureAwake(t.Context(), id); second <- result{s, err} }()
	time.Sleep(30 * time.Millisecond) // let it join the wait

	hangUp()
	if err := <-firstErr; !errors.Is(err, context.Canceled) {
		t.Errorf("caller that hung up: err = %v, want context.Canceled", err)
	}
	time.Sleep(30 * time.Millisecond)
	sessionstest.SetStatus(t, client, id, sessionstest.Ready("10.0.0.8"))
	select {
	case r := <-second:
		if r.err != nil || r.s.PodIP != "10.0.0.8" {
			t.Errorf("caller that kept waiting: %+v, %v", r.s, r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the second caller never got an answer")
	}
}

func TestRunningIsRememberedBriefly(t *testing.T) {
	ctx := t.Context()
	store, client := sessionstest.New(t)
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	w := &Waker{Store: store, RunningTTL: 2 * time.Second, now: func() time.Time { return now }}
	s, _ := store.Create(ctx, "a", "user-1")
	sessionstest.SetStatus(t, client, s.ID, sessionstest.Ready("10.0.0.7"))
	gets := countGets(client)
	ask := func(want int64, when string) {
		t.Helper()
		for _, find := range []func(context.Context, string) (sessions.Session, error){w.EnsureAwake, w.Running} {
			if got, err := find(ctx, s.ID); err != nil || got.PodIP != "10.0.0.7" {
				t.Fatalf("%s: %+v, %v", when, got, err)
			}
		}
		if n := gets.Load(); n != want {
			t.Errorf("%s: %d reads so far, want %d", when, n, want)
		}
	}

	ask(1, "first sight")
	now = now.Add(1900 * time.Millisecond)
	ask(1, "within the TTL")
	now = now.Add(200 * time.Millisecond)
	ask(2, "after the TTL")
	w.Invalidate(s.ID)
	ask(3, "after Invalidate")

	// What is not running is never remembered, and Running does not wake it.
	_ = store.Suspend(ctx, s.ID, sessions.StoppedByIdle)
	sessionstest.SetStatus(t, client, s.ID, sessionstest.Suspended())
	now = now.Add(3 * time.Second)
	before := gets.Load()
	for range 2 {
		if _, err := w.Running(ctx, s.ID); !errors.Is(err, ErrNotRunning) {
			t.Errorf("Running on a sleeping session: %v, want ErrNotRunning", err)
		}
	}
	if n := gets.Load() - before; n != 2 {
		t.Errorf("%d reads, want 2: a sleeping session is looked up each time", n)
	}
	if got, _ := store.Get(ctx, s.ID); got.State != sessions.Asleep {
		t.Errorf("state after Running = %s, want asleep", got.State)
	}
	if _, err := w.Running(ctx, "s-missing000"); !errors.Is(err, sessions.ErrNotFound) {
		t.Errorf("Running on a missing session: %v, want ErrNotFound", err)
	}
}
