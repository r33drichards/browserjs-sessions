package sessions_test

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"

	"github.com/r33drichards/computer-use/backend/internal/sessions"
	"github.com/r33drichards/computer-use/backend/internal/sessions/sessionstest"
)

func annotations(t *testing.T, client dynamic.Interface, id string) map[string]string {
	t.Helper()
	obj, err := client.Resource(sessions.SandboxGVR).Namespace(sessionstest.Namespace).Get(t.Context(), id, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return obj.GetAnnotations()
}

func recent(at time.Time) bool { return time.Since(at) >= 0 && time.Since(at) < time.Minute }

// What is said of a session's use is annotations on its Sandbox, and the
// Session read back says the same.
func TestMarkWritesActivityOnTheSession(t *testing.T) {
	ctx := t.Context()
	store, client := sessionstest.New(t)
	s, err := store.Create(ctx, "a", "u")
	if err != nil {
		t.Fatal(err)
	}
	sessionstest.SetStatus(t, client, s.ID, sessionstest.Ready("10.0.0.1"))

	at := time.Date(2026, 10, 2, 12, 0, 0, 500e6, time.UTC)
	until := at.Add(45 * time.Second)
	got, err := store.Mark(ctx, s.ID, sessions.Activity{Active: at, Replica: "ab12", InFlightUntil: until})
	if err != nil {
		t.Fatal(err)
	}
	if !got.LastActive.Equal(at) || !got.InFlight["ab12"].Equal(until) || got.State != sessions.Running {
		t.Fatalf("Mark returned %+v", got)
	}
	ann := annotations(t, client, s.ID)
	if ann[sessions.AnnLastActive] != "2026-10-02T12:00:00.5Z" || ann[sessions.AnnInFlightPrefix+"ab12"] != "2026-10-02T12:00:45.5Z" {
		t.Fatalf("annotations = %v", ann)
	}

	// Another replica's mark is its own; neither removes the other's.
	if _, err := store.Mark(ctx, s.ID, sessions.Activity{Replica: "cd34", InFlightUntil: until.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	read, _ := store.Get(ctx, s.ID)
	if len(read.InFlight) != 2 || !read.LastActive.Equal(at) {
		t.Fatalf("after a second replica's mark: %+v, last active %s", read.InFlight, read.LastActive)
	}
	for self, want := range map[string]bool{"ab12": true, "cd34": true, "ef56": true} {
		if got := read.InFlightElsewhere(self, at); got != want {
			t.Errorf("InFlightElsewhere(%s) = %v, want %v", self, got, want)
		}
	}
	if read.InFlightElsewhere("cd34", until) {
		t.Error("a mark that has run out still counts")
	}
	if !read.InFlightElsewhere("ab12", until) {
		t.Error("a mark that has not run out does not count")
	}

	// A name that cannot be part of an annotation key is not written.
	if _, err := store.Mark(ctx, s.ID, sessions.Activity{Replica: "not/a name", InFlightUntil: until}); err != nil {
		t.Fatal(err)
	}
	if got := annotations(t, client, s.ID); len(got) != len(ann)+1 {
		t.Errorf("annotations after a mark with a bad replica name: %v", got)
	}

	if err := store.ForgetInFlight(ctx, s.ID, []string{"ab12", "cd34"}); err != nil {
		t.Fatal(err)
	}
	if read, _ = store.Get(ctx, s.ID); len(read.InFlight) != 0 || !read.LastActive.Equal(at) {
		t.Fatalf("after ForgetInFlight: %+v", read)
	}
	if _, err := store.Mark(ctx, "s-aaaaaaaaaa", sessions.Activity{Active: at}); err != sessions.ErrNotFound {
		t.Errorf("Mark of a session that does not exist: %v", err)
	}
}

// A moment in the past is written only if it is later than what is there:
// last-active never moves backwards.
func TestMarkActiveSinceOnlyMovesForward(t *testing.T) {
	ctx := t.Context()
	store, client := sessionstest.New(t)
	s, _ := store.Create(ctx, "a", "u")
	sessionstest.SetStatus(t, client, s.ID, sessionstest.Ready("10.0.0.1"))
	at := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	if _, err := store.Mark(ctx, s.ID, sessions.Activity{Active: at}); err != nil {
		t.Fatal(err)
	}
	last := func() time.Time {
		t.Helper()
		got, err := store.Get(ctx, s.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got.LastActive
	}

	if err := store.MarkActiveSince(ctx, s.ID, at.Add(-10*time.Second)); err != nil || !last().Equal(at) {
		t.Fatalf("an earlier moment: %v, last active %s; want it left at %s", err, last(), at)
	}
	if err := store.MarkActiveSince(ctx, s.ID, at.Add(10*time.Second)); err != nil || !last().Equal(at.Add(10*time.Second)) {
		t.Fatalf("a later moment: %v, last active %s", err, last())
	}
	// Another replica writes between the read and the write: the decision
	// is made again on what it wrote.
	sessionstest.RaceNextGet(t, client, s.ID, func(obj *unstructured.Unstructured) {
		ann := obj.GetAnnotations()
		ann[sessions.AnnLastActive] = at.Add(time.Minute).Format(time.RFC3339Nano)
		obj.SetAnnotations(ann)
	})
	if err := store.MarkActiveSince(ctx, s.ID, at.Add(30*time.Second)); err != nil || !last().Equal(at.Add(time.Minute)) {
		t.Fatalf("raced by a later write: %v, last active %s; want the later one kept", err, last())
	}
	// Nothing is recorded of a session that is suspended.
	if err := store.Suspend(ctx, s.ID, sessions.StoppedByIdle); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkActiveSince(ctx, s.ID, at.Add(time.Hour)); err != nil || !last().IsZero() {
		t.Fatalf("on a suspended session: %v, last active %s; want none", err, last())
	}
}

// The store starts a session's idle period whenever it starts the session,
// and ends it when the session is suspended: the sweep needs no memory of
// when a session was first seen.
func TestTheStoreStartsAndStopsTheIdleClock(t *testing.T) {
	ctx := t.Context()
	store, client := sessionstest.New(t)
	s, _ := store.Create(ctx, "a", "u")
	sessionstest.SetStatus(t, client, s.ID, sessionstest.Ready("10.0.0.1"))
	last := func() time.Time {
		t.Helper()
		got, err := store.Get(ctx, s.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got.LastActive
	}
	stale := time.Now().Add(-time.Hour)
	held := sessions.Activity{Active: stale, Replica: "ab12", InFlightUntil: stale}

	if !recent(last()) {
		t.Fatalf("a new session's last-active = %s, want now", last())
	}
	for name, suspend := range map[string]func() error{
		"an idle sleep": func() error { return store.Sleep(ctx, s.ID, sessions.StoppedByIdle, nil) },
		"a user's stop": func() error { return store.Suspend(ctx, s.ID, sessions.StoppedByUser) },
	} {
		if _, err := store.Mark(ctx, s.ID, held); err != nil {
			t.Fatal(err)
		}
		if err := suspend(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if at := last(); !at.IsZero() {
			t.Errorf("%s left last-active at %s", name, at)
		}
		sessionstest.SetStatus(t, client, s.ID, sessionstest.Suspended())
		// A replica that still had a call open says so once more.
		if _, err := store.Mark(ctx, s.ID, held); err != nil {
			t.Fatal(err)
		}
		if err := store.Resume(ctx, s.ID); err != nil {
			t.Fatal(err)
		}
		if !recent(last()) {
			t.Errorf("after %s and a resume, last-active = %s, want now", name, last())
		}
		sessionstest.SetStatus(t, client, s.ID, sessionstest.Ready("10.0.0.1"))
	}

	// Wake, the proxy's way of starting a sleeping session.
	if err := store.Sleep(ctx, s.ID, sessions.StoppedByIdle, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Mark(ctx, s.ID, held); err != nil {
		t.Fatal(err)
	}
	if err := store.Wake(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := store.Get(ctx, s.ID)
	if !recent(got.LastActive) || len(got.InFlight) != 0 {
		t.Errorf("after a wake: last-active %s, in flight %v; want now, none", got.LastActive, got.InFlight)
	}
}

// Sleep asks stillWanted of the session as it is read for the write, with
// or without snapshots, and again if somebody wrote in between.
func TestSleepAsksOfTheSessionItIsAboutToSuspend(t *testing.T) {
	ctx := t.Context()
	store, client := sessionstest.New(t)
	s, _ := store.Create(ctx, "a", "u")
	sessionstest.SetStatus(t, client, s.ID, sessionstest.Ready("10.0.0.1"))

	used := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	sessionstest.RaceNextGet(t, client, s.ID, func(obj *unstructured.Unstructured) {
		ann := obj.GetAnnotations()
		ann[sessions.AnnLastActive] = used.Format(time.RFC3339Nano)
		obj.SetAnnotations(ann)
	})
	var asked []time.Time
	err := store.Sleep(ctx, s.ID, sessions.StoppedByIdle, func(fresh sessions.Session) bool {
		asked = append(asked, fresh.LastActive)
		return !fresh.LastActive.Equal(used)
	})
	if err != sessions.ErrStateChanged {
		t.Fatalf("Sleep = %v, want ErrStateChanged", err)
	}
	if len(asked) != 2 || asked[0].Equal(used) || !asked[1].Equal(used) {
		t.Fatalf("stillWanted was asked of %v; want the session as first read, then as written since", asked)
	}
	if got, _ := store.Get(ctx, s.ID); got.State != sessions.Running {
		t.Fatalf("state = %s, want running", got.State)
	}
}
