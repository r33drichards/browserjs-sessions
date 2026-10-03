package idle_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/r33drichards/computer-use/backend/internal/idle"
	"github.com/r33drichards/computer-use/backend/internal/sessions"
)

// clock is a clock the test moves.
type clock struct {
	mu sync.Mutex
	at time.Time
}

func newClock() *clock { return &clock{at: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// written is one write of a session's activity, as the store was asked.
type written struct {
	id   string
	at   time.Time // when it was asked, by the clock
	mark sessions.Activity
	past time.Time // of a MarkActiveSince: the moment
}

// marks is a Marker that records what it is asked to write.
type marks struct {
	clock *clock

	mu     sync.Mutex
	writes []written
	gets   []string
	err    error
	// answer is the session a write or a look returns.
	answer sessions.Session
	// hold, if set, is waited for by each Mark.
	hold chan struct{}
}

func (m *marks) Mark(_ context.Context, id string, a sessions.Activity) (sessions.Session, error) {
	m.mu.Lock()
	hold := m.hold
	m.mu.Unlock()
	if hold != nil {
		<-hold
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.writes = append(m.writes, written{id: id, at: m.clock.Now(), mark: a})
	s := m.answer
	s.ID = id
	return s, m.err
}

func (m *marks) MarkActiveSince(_ context.Context, id string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.writes = append(m.writes, written{id: id, at: m.clock.Now(), past: at})
	return m.err
}

func (m *marks) Get(_ context.Context, id string) (sessions.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gets = append(m.gets, id)
	s := m.answer
	s.ID = id
	return s, m.err
}

func (m *marks) all() []written {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]written(nil), m.writes...)
}

func (m *marks) fail(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.err = err
}

func newTracker(t *testing.T) (*idle.Tracker, *marks, *clock) {
	t.Helper()
	c := newClock()
	m := &marks{clock: c}
	return idle.New(m, "a", 15*time.Minute, c.Now), m, c
}

// tick moves the clock by d in steps of five seconds, with a Beat at each,
// as Run would.
func tick(t *testing.T, tr *idle.Tracker, c *clock, d time.Duration) {
	t.Helper()
	for range int(d / (5 * time.Second)) {
		c.Advance(5 * time.Second)
		tr.Beat(t.Context())
	}
}

const id = "s-aaaaaaaaaa"

// A call writes that the session is in use and has a call in flight, once,
// before it returns: a patch per call would hammer the API server.
func TestCallsWriteAtMostOncePerPeriod(t *testing.T) {
	tr, m, c := newTracker(t)
	start := c.Now()
	for range 100 {
		tr.Call(t.Context(), id)()
		c.Advance(250 * time.Millisecond) // 25 s of calls
	}
	got := m.all()
	if len(got) != 1 {
		t.Fatalf("%d writes for 100 calls in 25 s, want 1: %+v", len(got), got)
	}
	w := got[0]
	if !w.mark.Active.Equal(start) || w.mark.Replica != "a" || !w.mark.InFlightUntil.Equal(start.Add(45*time.Second)) {
		t.Errorf("the write = %+v, want in use at the start, in flight for 45 s, by replica a", w.mark)
	}
	// The next period has its own write.
	c.Advance(5 * time.Second)
	tr.Call(t.Context(), id)()
	if got := m.all(); len(got) != 2 || !got[1].mark.Active.Equal(start.Add(30*time.Second)) {
		t.Fatalf("after 30 s: %+v, want a second write, of that moment", got)
	}
}

// A connection that stays open is said again every period, for as long as
// it is: that is all that keeps the session awake as other replicas see it.
func TestAnOpenConnectionIsAHeartbeat(t *testing.T) {
	tr, m, c := newTracker(t)
	done := tr.Open(t.Context(), id)
	tick(t, tr, c, 5*time.Minute)
	got := m.all()
	if len(got) != 11 { // at 0 s, then every 30 s
		t.Fatalf("%d writes in 5 minutes with a viewer open, want 11", len(got))
	}
	for i, w := range got {
		if want := time.Duration(i) * 30 * time.Second; !w.mark.Active.Equal(w.at) || w.at.Sub(got[0].at) != want {
			t.Errorf("write %d: at %s, of %s; want every 30 s, of the moment it is made", i, w.at.Sub(got[0].at), w.mark.Active)
		}
		if !w.mark.InFlightUntil.IsZero() {
			t.Errorf("write %d says a call is in flight: a viewer is not one", i)
		}
	}

	// Closing is the last use, written as the moment it was, and then
	// nothing more.
	c.Advance(7 * time.Second)
	closed := c.Now()
	done()
	done() // closing twice changes nothing
	tick(t, tr, c, 10*time.Minute)
	got = m.all()[11:]
	if len(got) != 1 || !got[0].past.Equal(closed) {
		t.Fatalf("after the viewer left: %+v, want one write, of the moment it left", got)
	}
}

// A call in flight keeps its mark ahead of the clock for as long as it runs.
func TestACallInFlightRenewsItsMark(t *testing.T) {
	tr, m, c := newTracker(t)
	done := tr.Call(t.Context(), id)
	if tr.Calls(id) != 1 {
		t.Fatalf("Calls = %d, want 1", tr.Calls(id))
	}
	for range 60 { // five minutes
		c.Advance(5 * time.Second)
		tr.Beat(t.Context())
		last := m.all()[len(m.all())-1]
		if left := last.mark.InFlightUntil.Sub(c.Now()); left < 10*time.Second {
			t.Fatalf("the mark runs out in %s with the call still in flight", left)
		}
	}
	done()
	if tr.Calls(id) != 0 {
		t.Fatalf("Calls = %d after the call, want 0", tr.Calls(id))
	}
	before := len(m.all())
	tick(t, tr, c, 5*time.Minute)
	for _, w := range m.all()[before:] {
		if !w.mark.InFlightUntil.IsZero() {
			t.Fatalf("the mark was renewed after the call ended: %+v", w)
		}
	}
}

// An upload is in flight (a drain waits for it) and is not use of the
// session: anybody can send one. Touch, for one the pod took, is.
func TestAnUploadIsInFlightAndNotUse(t *testing.T) {
	tr, m, c := newTracker(t)
	done := tr.Flight(id)
	tr.Beat(t.Context()) // waits for the write
	got := m.all()
	if len(got) != 1 || !got[0].mark.Active.IsZero() || got[0].mark.InFlightUntil.IsZero() {
		t.Fatalf("an upload wrote %+v, want an in-flight mark and no use", got)
	}
	done()
	tick(t, tr, c, time.Minute)
	if got := m.all(); len(got) != 1 {
		t.Fatalf("a refused upload was written as use: %+v", got[1:])
	}

	tr.Touch(id)
	tr.Beat(t.Context())
	if got := m.all(); len(got) != 2 || !got[1].mark.Active.Equal(c.Now()) {
		t.Fatalf("an accepted upload wrote %+v, want its moment as the last use", got[1:])
	}
}

// A call returns only when it is written on the session: the replica that
// is about to suspend the session must be able to see it.
func TestACallWaitsForItsWrite(t *testing.T) {
	tr, m, _ := newTracker(t)
	m.hold = make(chan struct{})
	returned := make(chan struct{}, 2)
	for range 2 { // the second finds the write in progress, and waits for it too
		go func() {
			tr.Call(t.Context(), id)
			returned <- struct{}{}
		}()
	}
	select {
	case <-returned:
		t.Fatal("a call went ahead before it was written on the session")
	case <-time.After(50 * time.Millisecond):
	}
	close(m.hold)
	for range 2 {
		select {
		case <-returned:
		case <-time.After(5 * time.Second):
			t.Fatal("a call never went ahead")
		}
	}
	if got := m.all(); len(got) != 1 {
		t.Fatalf("%d writes for two calls at once, want 1", len(got))
	}
}

// A cluster that does not answer holds nothing up for long, and the write
// is tried again within the period.
func TestAFailedWriteIsTriedAgainSoon(t *testing.T) {
	tr, m, c := newTracker(t)
	m.fail(errors.New("the API server is down"))
	done := tr.Open(t.Context(), id)
	defer done()
	if got := m.all(); len(got) != 1 {
		t.Fatalf("%d writes, want 1", len(got))
	}
	m.fail(nil)
	tick(t, tr, c, 5*time.Second)
	if got := m.all(); len(got) != 2 || !got[1].mark.Active.Equal(c.Now()) {
		t.Fatalf("5 s after a failed write: %+v, want it tried again", got)
	}
	tick(t, tr, c, 25*time.Second)
	if got := m.all(); len(got) != 2 {
		t.Fatalf("written again inside the period: %+v", got[2:])
	}
}

// A session that was deleted is not written to again.
func TestADeletedSessionIsLeftAlone(t *testing.T) {
	tr, m, c := newTracker(t)
	done := tr.Open(t.Context(), id)
	m.fail(sessions.ErrNotFound)
	tick(t, tr, c, 30*time.Second)
	before := len(m.all())
	tick(t, tr, c, 5*time.Minute)
	done()
	tick(t, tr, c, 5*time.Minute)
	if got := m.all(); len(got) != before {
		t.Fatalf("%d more writes to a session that is gone", len(got)-before)
	}
}

// What a write returns is the session as the cluster has it, and the
// replica is told: it may have been put to sleep, or marked draining, by
// another. A session it has only a stream open to is written nothing, and
// looked at once per period instead.
func TestTheReplicaIsToldWhatItSees(t *testing.T) {
	tr, m, c := newTracker(t)
	m.answer = sessions.Session{State: sessions.Running, Draining: sessions.StoppedByCredit}
	var mu sync.Mutex
	var seen []sessions.Session
	tr.Seen = func(s sessions.Session) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, s)
	}
	const streamed = "s-bbbbbbbbbb"
	tr.Watched = func() []string { return []string{id, streamed} }

	tr.Call(t.Context(), id)()
	mu.Lock()
	if len(seen) != 1 || seen[0].ID != id || seen[0].Draining != sessions.StoppedByCredit {
		t.Fatalf("after a call: told %+v, want the session, draining", seen)
	}
	mu.Unlock()

	tick(t, tr, c, time.Minute)
	m.mu.Lock()
	looks := map[string]int{}
	for _, got := range m.gets {
		looks[got]++
	}
	m.mu.Unlock()
	if looks[streamed] != 2 {
		t.Errorf("a session with only a stream open was looked at %d times in a minute, want 2", looks[streamed])
	}
	for _, w := range m.all() {
		if w.id == streamed {
			t.Errorf("a session with only a stream open was written to: %+v", w)
		}
	}
}

func TestEvery(t *testing.T) {
	for after, want := range map[time.Duration]time.Duration{
		15 * time.Minute: 30 * time.Second,
		2 * time.Minute:  30 * time.Second,
		time.Minute:      15 * time.Second,
		20 * time.Second: 5 * time.Second,
	} {
		if got := idle.Every(after); got != want {
			t.Errorf("Every(%s) = %s, want %s", after, got, want)
		}
	}
}
