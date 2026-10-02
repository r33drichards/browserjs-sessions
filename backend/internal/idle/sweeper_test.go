package idle_test

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"

	"github.com/r33drichards/computer-use/backend/internal/idle"
	"github.com/r33drichards/computer-use/backend/internal/sessions"
	"github.com/r33drichards/computer-use/backend/internal/sessions/sessionstest"
)

var rule = idle.Rule{After: 15 * time.Minute, Margin: idle.DefaultMargin}

// cluster is one fake cluster and the backend's replicas over it. Each has
// its own Tracker, as each has its own process; all they share is the store.
type cluster struct {
	t      *testing.T
	store  *sessions.Store
	client dynamic.Interface
	clock  *clock
	steps  int // of pass
}

func newCluster(t *testing.T) *cluster {
	store, client := sessionstest.New(t)
	return &cluster{t: t, store: store, client: client, clock: startingNow()}
}

// startingNow is a clock that starts at the present: the store stamps a new
// session with the real time.
func startingNow() *clock { return &clock{at: time.Now()} }

// replica is a Tracker of its own over the cluster's store, on the
// cluster's clock.
func (c *cluster) replica(name string) *idle.Tracker {
	return idle.New(c.store, name, rule.After, c.clock.Now)
}

// running makes a session that is running.
func (c *cluster) running(name string) string {
	c.t.Helper()
	s, err := c.store.Create(c.t.Context(), name, "u")
	if err != nil {
		c.t.Fatal(err)
	}
	sessionstest.SetStatus(c.t, c.client, s.ID, sessionstest.Ready("10.0.0.1"))
	return s.ID
}

// sweep is the sweep of whichever replica leads: it needs nothing of it.
func (c *cluster) sweep() {
	c.t.Helper()
	if err := idle.Sweep(c.t.Context(), c.store, rule, c.clock.Now); err != nil {
		c.t.Fatal(err)
	}
}

func (c *cluster) state(id string) sessions.State {
	c.t.Helper()
	s, err := c.store.Get(c.t.Context(), id)
	if err != nil {
		c.t.Fatal(err)
	}
	return s.State
}

// pass moves the clock by d, five seconds at a time, with every replica's
// heartbeat at each step and a sweep every minute: time passing on a
// deployment.
func (c *cluster) pass(d time.Duration, replicas ...*idle.Tracker) {
	c.t.Helper()
	for range int(d / (5 * time.Second)) {
		c.clock.Advance(5 * time.Second)
		for _, r := range replicas {
			r.Beat(c.t.Context())
		}
		if c.steps++; c.steps%12 == 0 {
			c.sweep()
		}
	}
}

func TestSweepSuspendsOnlyIdleRunningSessions(t *testing.T) {
	c := newCluster(t)
	ctx := t.Context()
	a := c.replica("a")
	busy, quiet := c.running("busy"), c.running("quiet")
	starting, err := c.store.Create(ctx, "starting", "u") // never ready
	if err != nil {
		t.Fatal(err)
	}

	c.sweep()
	c.clock.Advance(16 * time.Minute)
	a.Call(ctx, busy)()
	c.sweep()

	if c.state(busy) != sessions.Running {
		t.Errorf("busy session was suspended")
	}
	if c.state(quiet) != sessions.Stopping { // fake cluster never finishes the suspend
		t.Errorf("quiet session state = %s", c.state(quiet))
	}
	if c.state(starting.ID) != sessions.Starting {
		t.Errorf("a session that is still starting was suspended")
	}
}

// The idle period is IDLE_AFTER, counted from the last use, to within the
// margin and the minute between sweeps: as it was with one replica.
func TestTheIdlePeriodIsCountedFromTheLastUse(t *testing.T) {
	c := newCluster(t)
	a := c.replica("a")
	id := c.running("a")
	c.pass(10*time.Minute, a)

	// A burst of calls, the last of them 20 s after the first: nothing is
	// written for it when it happens (the first was written 20 s ago).
	a.Call(t.Context(), id)()
	c.pass(20*time.Second, a)
	a.Call(t.Context(), id)()
	last := c.clock.Now()

	for c.state(id) == sessions.Running {
		if c.clock.Now().Sub(last) > 20*time.Minute {
			t.Fatal("the session was never put to sleep")
		}
		c.pass(5*time.Second, a)
	}
	if idleFor := c.clock.Now().Sub(last); idleFor < rule.After || idleFor > rule.After+rule.Margin+time.Minute {
		t.Errorf("put to sleep %s after its last use, want between 15m and 16m05s", idleFor)
	}
}

// The user's stop and the idle sweep can land in either order; either way
// the session ends up stopped by the user, so it is not woken on demand.
func TestSweepNeverTakesOverAUserStop(t *testing.T) {
	ctx := t.Context()
	setup := func(t *testing.T) (*cluster, string) {
		c := newCluster(t)
		id := c.running("a")
		c.clock.Advance(16 * time.Minute) // idle long enough to be put to sleep
		return c, id
	}
	check := func(t *testing.T, c *cluster, id string) {
		t.Helper()
		obj, err := c.client.Resource(sessions.SandboxGVR).Namespace(sessionstest.Namespace).Get(ctx, id, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if by := obj.GetAnnotations()[sessions.AnnStoppedBy]; by != sessions.StoppedByUser {
			t.Errorf("stopped-by = %q, want user", by)
		}
		sessionstest.SetStatus(t, c.client, id, sessionstest.Suspended())
		if c.state(id) != sessions.Stopped {
			t.Errorf("state = %s, want stopped", c.state(id))
		}
	}

	t.Run("the user stops while the sweep is deciding", func(t *testing.T) {
		c, id := setup(t)
		// The sweep lists the session as running; the stop lands before the
		// sweep's suspend is written.
		sessionstest.RaceNextGet(t, c.client, id, sessionstest.UserStop)
		c.sweep()
		check(t, c, id)
	})

	t.Run("the sweep suspends, then the user stops", func(t *testing.T) {
		c, id := setup(t)
		c.sweep()
		if err := c.store.Suspend(ctx, id, sessions.StoppedByUser); err != nil {
			t.Fatal(err)
		}
		c.sweep()
		check(t, c, id)
	})
}

// A session its user resumes (the API path: no replica proxies anything)
// gets a full idle period, whether or not a sweep saw it asleep in between.
// The store starts its clock.
func TestSweepGivesAResumedSessionAFullIdlePeriod(t *testing.T) {
	for _, tc := range []struct {
		name        string
		sweepAsleep bool
	}{
		{"resumed after sweeps saw it asleep", true},
		{"resumed and running again before the next sweep", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			// The store stamps a resume with the real time, so this clock
			// is the real one, moved on.
			c := newCluster(t)
			id := c.running("a")
			c.sweep()
			c.clock.Advance(16 * time.Minute)
			c.sweep()
			if c.state(id) != sessions.Stopping {
				t.Fatalf("state = %s, want stopping", c.state(id))
			}
			sessionstest.SetStatus(t, c.client, id, sessionstest.Suspended())
			if tc.sweepAsleep {
				c.sweep()
			}

			if err := c.store.Resume(ctx, id); err != nil {
				t.Fatal(err)
			}
			resumed := time.Now()
			sessionstest.SetStatus(t, c.client, id, sessionstest.Ready("10.0.0.2"))
			at := func(d time.Duration) {
				c.clock.mu.Lock()
				c.clock.at = resumed.Add(d)
				c.clock.mu.Unlock()
				c.sweep()
			}
			at(30 * time.Second)
			if c.state(id) != sessions.Running {
				t.Fatalf("state after the sweep following a resume = %s, want running", c.state(id))
			}
			at(14*time.Minute + 30*time.Second)
			if c.state(id) != sessions.Running {
				t.Errorf("state %s before a full idle period had passed, want running", c.state(id))
			}
			at(16 * time.Minute)
			if c.state(id) != sessions.Stopping {
				t.Errorf("state = %s after a full idle period, want stopping", c.state(id))
			}
		})
	}
}

// A snapshot takes a while. A session that is used while its snapshot is
// taken stays up, and the snapshot is thrown away: by whichever replica it
// is used through.
func TestSweepLeavesASessionUsedDuringItsSnapshot(t *testing.T) {
	store, client, _ := sessionstest.NewWithSnapshots(t, sessions.SnapshotOptions{Timeout: 2 * time.Second})
	c := &cluster{t: t, store: store, client: client, clock: startingNow()}

	used, quiet := c.running("used"), c.running("quiet")
	c.sweep()
	c.clock.Advance(16 * time.Minute)
	// The sweep reads the session, to snapshot it; before the snapshot is
	// done, another replica has written that it took a call.
	sessionstest.RaceNextGet(t, client, used, usedNow(c))
	c.sweep()

	if c.state(used) != sessions.Running {
		t.Errorf("the session in use was suspended")
	}
	if c.state(quiet) != sessions.Stopping {
		t.Errorf("quiet session state = %s", c.state(quiet))
	}
	if got := sessionstest.Snapshots(t, client); len(got) != 1 || !strings.Contains(got[0], quiet) {
		t.Errorf("snapshots = %v, want one, of the quiet session", got)
	}
}

// A running session nobody has said anything about (started by a backend
// from before the annotation) gets a full idle period from the first sweep
// that sees it, not an immediate suspend.
func TestSweepStartsTheClockOfASessionWithNoAnnotation(t *testing.T) {
	c := newCluster(t)
	id := c.running("old")
	strip(t, c, id)
	c.clock.Advance(3 * time.Hour)

	c.sweep()
	if c.state(id) != sessions.Running {
		t.Fatalf("a session with no annotation was suspended at first sight: %s", c.state(id))
	}
	if s, _ := c.store.Get(t.Context(), id); !s.LastActive.Equal(c.clock.Now()) {
		t.Fatalf("last-active = %s, want the time of the sweep", s.LastActive)
	}
	c.clock.Advance(14 * time.Minute)
	c.sweep()
	if c.state(id) != sessions.Running {
		t.Fatalf("suspended before its idle period: %s", c.state(id))
	}
	c.clock.Advance(2 * time.Minute)
	c.sweep()
	if c.state(id) != sessions.Stopping {
		t.Fatalf("state = %s after its idle period, want stopping", c.state(id))
	}
}

// usedNow edits a Sandbox the way a replica that proxies a call to it does.
func usedNow(c *cluster) func(obj *unstructured.Unstructured) {
	return func(obj *unstructured.Unstructured) {
		ann := obj.GetAnnotations()
		ann[sessions.AnnLastActive] = c.clock.Now().UTC().Format(time.RFC3339Nano)
		obj.SetAnnotations(ann)
	}
}

// strip removes the activity annotation from a session's Sandbox.
func strip(t *testing.T, c *cluster, id string) {
	t.Helper()
	sandboxes := c.client.Resource(sessions.SandboxGVR).Namespace(sessionstest.Namespace)
	obj, err := sandboxes.Get(t.Context(), id, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ann := obj.GetAnnotations()
	delete(ann, sessions.AnnLastActive)
	obj.SetAnnotations(ann)
	if _, err := sandboxes.Update(t.Context(), obj, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

// Two replicas. A viewer is open on the one that does not sweep: the other
// never puts the session to sleep, for as long as the first says it is
// there, with no count of viewers between them.
func TestAViewerOnAnotherReplicaKeepsTheSessionAwake(t *testing.T) {
	c := newCluster(t)
	a, b := c.replica("a"), c.replica("b")
	id := c.running("a")

	leave := b.Open(t.Context(), id)
	c.pass(time.Hour, a, b)
	if c.state(id) != sessions.Running {
		t.Fatalf("a session with a viewer on another replica was put to sleep: %s", c.state(id))
	}

	// The viewer leaves: the idle period runs from then.
	leave()
	left := c.clock.Now()
	c.pass(14*time.Minute, a, b)
	if c.state(id) != sessions.Running {
		t.Fatalf("put to sleep %s after the viewer left", c.clock.Now().Sub(left))
	}
	c.pass(2*time.Minute+5*time.Second, a, b)
	if c.state(id) != sessions.Stopping {
		t.Fatalf("still %s, %s after the viewer left", c.state(id), c.clock.Now().Sub(left))
	}
}

// A replica that dies with connections open stops saying so. The session is
// not put to sleep at once (the last heartbeat stands), and not kept
// forever either: it sleeps one idle period after the last heartbeat.
func TestAReplicaThatDiesStopsHoldingItsSessions(t *testing.T) {
	c := newCluster(t)
	a, b := c.replica("a"), c.replica("b")
	id := c.running("a")

	b.Open(t.Context(), id) // never closed: the process is killed
	c.pass(10*time.Minute, a, b)
	died := c.clock.Now()

	c.pass(14*time.Minute, a) // b beats no more
	if c.state(id) != sessions.Running {
		t.Fatalf("put to sleep %s after its replica died, inside the idle period", c.clock.Now().Sub(died))
	}
	c.pass(2*time.Minute+5*time.Second, a)
	if c.state(id) != sessions.Stopping {
		t.Fatalf("still %s, %s after the replica that held it died", c.state(id), c.clock.Now().Sub(died))
	}
}

// The race the design turns on: the sweep reads a session as idle, and
// before its suspend is written another replica proxies a call. The call was
// written on the session first, so the suspend, which is conditional on
// what the sweep read, does not go through; read again, the session is in
// use.
func TestACallThatLandsBeforeTheSuspendWins(t *testing.T) {
	c := newCluster(t)
	b := c.replica("b")
	id := c.running("a")
	c.clock.Advance(16 * time.Minute)

	// The sweep's read for the suspend is served the session as it was;
	// the other replica's write lands before the sweep can act on it.
	sessionstest.RaceNextGet(t, c.client, id, usedNow(c))
	c.sweep()
	if c.state(id) != sessions.Running {
		t.Fatalf("the session was suspended under a call another replica had just taken: %s", c.state(id))
	}

	// The other order: the suspend is written first. The replica's write
	// then returns the session as it now is, and the replica is told, so
	// that it wakes the session instead of proxying to a pod that is going.
	c.clock.Advance(16 * time.Minute)
	c.sweep()
	if c.state(id) != sessions.Stopping {
		t.Fatalf("state = %s, want stopping", c.state(id))
	}
	var told []sessions.State
	b.Seen = func(s sessions.Session) { told = append(told, s.State) }
	b.Call(t.Context(), id)()
	if len(told) != 1 || told[0] == sessions.Running {
		t.Fatalf("after a call to a session that was just suspended the replica was told %v, want that it is not running", told)
	}
}

// The moment a session was last used is one replica's clock's, and the
// sweep compares it with another's. A sweeping replica whose clock runs a
// few seconds ahead must not find a session idle early.
func TestClockSkewBetweenReplicasIsAllowedFor(t *testing.T) {
	c := newCluster(t)
	id := c.running("a")
	behind := c.clock.Now().Add(-3 * time.Second) // the other replica's clock
	b := idle.New(c.store, "b", rule.After, func() time.Time { return behind })
	b.Call(t.Context(), id)()

	// Fifteen minutes later by the sweeper's clock, 15 m 3 s by the stamp.
	c.clock.Advance(rule.After)
	c.sweep()
	if c.state(id) != sessions.Running {
		t.Fatalf("put to sleep before its idle period was over by the sweeper's own clock")
	}
	c.clock.Advance(rule.Margin)
	c.sweep()
	if c.state(id) != sessions.Stopping {
		t.Fatalf("state = %s once the margin had passed, want stopping", c.state(id))
	}
}

// The in-flight mark of a replica that is long gone is removed from a
// session that keeps running, so that marks do not pile up over deploys.
func TestSweepRemovesMarksThatRanOutLongAgo(t *testing.T) {
	c := newCluster(t)
	a, old := c.replica("a"), c.replica("old")
	id := c.running("a")
	old.Call(t.Context(), id) // never finished: the replica was replaced
	keep := a.Open(t.Context(), id)
	defer keep()

	marks := func() map[string]time.Time {
		s, err := c.store.Get(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		return s.InFlight
	}
	if _, ok := marks()["old"]; !ok {
		t.Fatalf("no mark of the old replica: %v", marks())
	}
	c.pass(5*time.Minute, a)
	if _, ok := marks()["old"]; !ok {
		t.Fatalf("the mark was removed while it could still be the trace of a call: %v", marks())
	}
	c.pass(10*time.Minute, a)
	if got := marks(); len(got) != 0 {
		t.Fatalf("marks left on the session: %v", got)
	}
	if c.state(id) != sessions.Running {
		t.Fatalf("state = %s, want running", c.state(id))
	}
}
