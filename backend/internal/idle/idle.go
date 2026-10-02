// Package idle decides which sessions have gone unused long enough to sleep.
//
// Nothing it decides from is in a process. When a session was last used is
// written on the session itself (sessions.AnnLastActive) by whichever replica
// of the backend proxied the use, and the sweep reads that and nothing else,
// so any number of replicas, of any mix of versions that write the
// annotation, can serve one session.
//
// A Tracker is one replica's part in that: it writes the annotation for the
// sessions this replica proxies to. It writes coarsely, at most once per
// session every Every, and keeps writing at that pace for as long as the
// replica holds a connection or a call to the session open (a heartbeat). So
// a session somebody is looking at, on whatever replica, always has a recent
// annotation, with no count of viewers shared between replicas; and one whose
// replica died with its connections stops being vouched for, and sleeps an
// idle period later.
//
// What a Tracker remembers (its own open connections, and when it last
// wrote) is nobody else's to know and is lost with the process at no cost:
// the connections are lost with it.
package idle

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/r33drichards/computer-use/backend/internal/metrics"
	"github.com/r33drichards/computer-use/backend/internal/sessions"
)

// DefaultEvery is how often, at most, a replica writes a session's activity.
const DefaultEvery = 30 * time.Second

// How long one write of activity may take. A call waits for it (see Call).
const markTimeout = 3 * time.Second

// Activity is how the backend says a session is in use, and how it is asked
// when one was last used. The proxy records through it and the sweep reads
// through it, so where the answer is kept is one implementation's business.
//
// Tracker is the implementation: the answer is an annotation on the session.
// Another could keep it elsewhere (a telemetry system that is queried, say);
// docs/stateless-backend.md weighs that. Whatever it is, the sweep suspends
// only on what LastActive says of the session as it was just read.
type Activity interface {
	// Call marks an authenticated call: the session is in use and has a
	// call in flight until done is called.
	Call(ctx context.Context, id string) (done func())
	// Open marks a long-lived connection that keeps the session awake.
	Open(ctx context.Context, id string) (done func())
	// Flight marks work in flight that is not use of the session.
	Flight(id string) (done func())
	// Touch records one use of the session, now.
	Touch(id string)
	// Calls is the calls this replica has in flight to the session, and
	// Replica the name it says so under.
	Calls(id string) int
	Replica() string
	// Observe has seen told of sessions as the cluster has them, and
	// watched asked for sessions to look at although nothing is recorded
	// for them.
	Observe(seen func(sessions.Session), watched func() []string)
	// LastActive is when s was last used, zero if nothing says.
	LastActive(s sessions.Session) time.Time
}

var _ Activity = (*Tracker)(nil)

// Marker is what a Tracker needs of the session store (a *sessions.Store).
type Marker interface {
	Mark(ctx context.Context, id string, a sessions.Activity) (sessions.Session, error)
	MarkActiveSince(ctx context.Context, id string, at time.Time) error
	Get(ctx context.Context, id string) (sessions.Session, error)
}

// Tracker writes the activity of the sessions this replica proxies to.
type Tracker struct {
	store   Marker
	replica string
	every   time.Duration
	now     func() time.Time

	// Seen, if set, is told of a session each time a write or a look
	// returns it: it may have been put to sleep, or marked draining, by
	// another replica. Set it before the Tracker is used.
	Seen func(sessions.Session)
	// Watched, if set, names sessions to look at once per Every although
	// nothing is written for them (the proxy's open streams). Set it before
	// the Tracker is used.
	Watched func() []string

	mu sync.Mutex
	s  map[string]*entry
}

// entry is what this replica has open on one session, and when it last
// said so.
type entry struct {
	active  int // connections and calls that keep the session awake
	flights int // calls in flight (a drain waits for them)
	// touched is the latest use that has not been written, zero if all was.
	touched time.Time
	// wroteActive is the moment last written as the session's last use,
	// and wroteFlight when the in-flight mark was last written (or either
	// tried); looked is when the session was last looked at.
	wroteActive, wroteFlight, looked time.Time
	// writing is closed when the write in progress is over; nil if none is.
	writing chan struct{}
	gone    bool // the session no longer exists
}

// New makes the Tracker of the replica named replica (sessions.ValidReplica)
// over store. after is the idle period: activity is written every
// DefaultEvery, or more often if the idle period is short.
func New(store Marker, replica string, after time.Duration, now func() time.Time) *Tracker {
	return &Tracker{store: store, replica: replica, every: Every(after), now: now, s: map[string]*entry{}}
}

// Every is how often activity is written for an idle period of after: four
// times in it at least, so that a session held open is never taken for idle.
func Every(after time.Duration) time.Duration {
	if every := after / 4; every > 0 && every < DefaultEvery {
		return every
	}
	return DefaultEvery
}

// Observe sets Seen and Watched, where they are not set.
func (t *Tracker) Observe(seen func(sessions.Session), watched func() []string) {
	if t.Seen == nil {
		t.Seen = seen
	}
	if t.Watched == nil {
		t.Watched = watched
	}
}

// LastActive is when s was last used: what is written on it.
func (t *Tracker) LastActive(s sessions.Session) time.Time { return s.LastActive }

// States is what this replica has to say of each session it proxies to,
// for the metrics: a session it has nothing more to say of is not in it.
func (t *Tracker) States() []metrics.SessionState {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]metrics.SessionState, 0, len(t.s))
	for id, e := range t.s {
		if e.gone {
			continue
		}
		last := e.wroteActive
		if e.touched.After(last) {
			last = e.touched
		}
		out = append(out, metrics.SessionState{ID: id, LastActive: last, Open: e.active, InFlight: e.flights})
	}
	return out
}

// Replica is the name this replica writes its in-flight marks under.
func (t *Tracker) Replica() string { return t.replica }

// hold is how far ahead an in-flight mark reaches: to the next write, and
// then half as long again for the write to be late and the clocks of two
// replicas to disagree.
func (t *Tracker) hold() time.Duration { return t.every + t.every/2 }

func (t *Tracker) entry(id string) *entry {
	e := t.s[id]
	if e == nil {
		e = &entry{}
		t.s[id] = e
	}
	return e
}

// due takes, under the lock, what is to be written for e now, and notes it
// as written.
func (t *Tracker) due(e *entry, now time.Time) (a sessions.Activity, any bool) {
	if e.gone {
		return a, false
	}
	if (e.active > 0 || !e.touched.IsZero()) && now.Sub(e.wroteActive) >= t.every {
		a.Active, e.wroteActive, e.touched = now, now, time.Time{}
	}
	if e.flights > 0 && now.Sub(e.wroteFlight) >= t.every {
		a.Replica, a.InFlightUntil, e.wroteFlight = t.replica, now.Add(t.hold()), now
	}
	return a, !a.Active.IsZero() || !a.InFlightUntil.IsZero()
}

// write writes a, which due took for the entry of id, and reports the
// session as it then is. A write that failed is tried again soon, not at
// once: a cluster that does not answer must not hold up every call.
func (t *Tracker) write(ctx context.Context, id string, a sessions.Activity) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), markTimeout)
	defer cancel()
	s, err := t.store.Mark(ctx, id, a)
	metrics.ActivityWrites.WithLabelValues("mark", result(err)).Inc()
	if err != nil {
		t.failed(id, a, err)
		return
	}
	if t.Seen != nil {
		t.Seen(s)
	}
}

func result(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, sessions.ErrNotFound):
		return "gone"
	}
	return "error"
}

func (t *Tracker) failed(id string, a sessions.Activity, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e := t.s[id]
	if e == nil {
		return
	}
	if errors.Is(err, sessions.ErrNotFound) {
		e.gone = true
		return
	}
	slog.Warn("session activity not written; it is tried again", "session", id, "err", err)
	retry := t.now().Add(t.every/6 - t.every)
	if !a.Active.IsZero() {
		e.wroteActive = retry
		if e.touched.IsZero() {
			e.touched = a.Active
		}
	}
	if !a.InFlightUntil.IsZero() {
		e.wroteFlight = retry
	}
}

// begin opens a hold on a session and returns when what it says is written:
// by this caller if a write is due, by another whose write is in progress
// otherwise. With wait false it returns at once and the write follows.
func (t *Tracker) begin(ctx context.Context, id string, active, flight, wait bool) (done func()) {
	t.mu.Lock()
	e := t.entry(id)
	if active {
		e.active++
	}
	if flight {
		e.flights++
	}
	a, write := t.due(e, t.now())
	var mine, theirs chan struct{}
	if write {
		// Whoever comes while this is being written waits for it too.
		mine = make(chan struct{})
		e.writing = mine
	} else {
		theirs = e.writing
	}
	t.mu.Unlock()

	finish := func() {
		t.write(ctx, id, a)
		t.mu.Lock()
		if e.writing == mine {
			e.writing = nil
		}
		t.mu.Unlock()
		close(mine)
	}
	switch {
	case write && wait:
		finish()
	case write:
		go finish()
	case wait && theirs != nil:
		select {
		case <-theirs:
		case <-ctx.Done():
		}
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			t.mu.Lock()
			defer t.mu.Unlock()
			if active {
				e.active--
				// Closing counts as activity.
				e.touched = t.now()
			}
			if flight {
				e.flights--
			}
		})
	}
}

// Call marks an MCP call to a session: the session is in use, and has a call
// in flight, until the returned func is called. Call returns once that is
// written on the session (if it was not written lately), so that the sweep
// of another replica, which suspends a session only on what it has just
// read, cannot miss a call that is being forwarded. If the cluster does not
// answer in time the call goes ahead unrecorded.
func (t *Tracker) Call(ctx context.Context, id string) (done func()) {
	return t.begin(ctx, id, true, true, true)
}

// Open marks a long-lived connection (a VNC viewer). The session cannot go
// idle until the returned func is called.
func (t *Tracker) Open(ctx context.Context, id string) (done func()) {
	return t.begin(ctx, id, true, false, true)
}

// Flight marks work in flight that is not, for now, use of the session (an
// upload, which anybody can send: only one the pod took is activity, and
// then Touch says so). It does not wait for the write.
func (t *Tracker) Flight(id string) (done func()) {
	return t.begin(context.Background(), id, false, true, false)
}

// Touch records activity on a session (an upload the pod took).
func (t *Tracker) Touch(id string) {
	t.mu.Lock()
	e := t.entry(id)
	e.touched = t.now()
	t.mu.Unlock()
	t.begin(context.Background(), id, false, false, false)()
}

// Calls is how many calls to the session this replica has in flight.
func (t *Tracker) Calls(id string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	if e := t.s[id]; e != nil {
		return e.flights
	}
	return 0
}

// Beat writes what is due: the heartbeat of every session this replica
// holds open, and the last use of one it no longer does. It looks at the
// watched sessions that had no write. It returns when all is written.
func (t *Tracker) Beat(ctx context.Context) {
	now := t.now()
	type trailing struct {
		id string
		at time.Time
	}
	var (
		marks  = map[string]sessions.Activity{}
		past   []trailing
		looks  []string
		watch  []string
		wg     sync.WaitGroup
		recent = func(at time.Time) bool { return now.Sub(at) < t.every }
	)
	if t.Watched != nil {
		watch = t.Watched()
	}
	// Writes in progress first: what they leave to say is said below.
	t.mu.Lock()
	var writing []chan struct{}
	for _, e := range t.s {
		if e.writing != nil {
			writing = append(writing, e.writing)
		}
	}
	t.mu.Unlock()
	for _, done := range writing {
		select {
		case <-done:
		case <-ctx.Done():
			return
		}
	}
	t.mu.Lock()
	for id, e := range t.s {
		switch {
		case e.writing != nil:
		case e.active == 0 && !e.touched.IsZero() && !recent(e.wroteActive) && !e.gone:
			// Its last use is a moment that has passed: written as that
			// moment, and only if nothing later is.
			past = append(past, trailing{id, e.touched})
			e.wroteActive, e.touched = e.touched, time.Time{}
			if a, ok := t.due(e, now); ok {
				marks[id] = a
			}
		default:
			if a, ok := t.due(e, now); ok {
				marks[id] = a
			}
		}
		said := e.touched.IsZero() && !recent(e.wroteActive) && !recent(e.wroteFlight) && !recent(e.looked)
		if e.active == 0 && e.flights == 0 && e.writing == nil && (e.gone || said) {
			delete(t.s, id) // nothing held, nothing to say
		}
	}
	for _, id := range watch {
		if _, written := marks[id]; written {
			continue
		}
		if e := t.entry(id); !recent(e.looked) && !e.gone {
			e.looked = now
			looks = append(looks, id)
		}
	}
	t.mu.Unlock()

	for id, a := range marks {
		wg.Go(func() { t.write(ctx, id, a) })
	}
	for _, p := range past {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(ctx, markTimeout)
			defer cancel()
			err := t.store.MarkActiveSince(ctx, p.id, p.at)
			metrics.ActivityWrites.WithLabelValues("last-use", result(err)).Inc()
			if err != nil {
				t.failed(p.id, sessions.Activity{Active: p.at}, err)
			}
		})
	}
	for _, id := range looks {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(ctx, markTimeout)
			defer cancel()
			s, err := t.store.Get(ctx, id)
			metrics.ActivityWrites.WithLabelValues("look", result(err)).Inc()
			if err == nil && t.Seen != nil {
				t.Seen(s)
			}
		})
	}
	wg.Wait()
}

// Flush writes the last use of every session this replica has yet to
// write, whatever the pace: for a replica that is shutting down, whose next
// beat will not come. Its connections are closed by then, so there is
// nothing left to hold.
func (t *Tracker) Flush(ctx context.Context) {
	t.mu.Lock()
	last := map[string]time.Time{}
	for id, e := range t.s {
		if !e.touched.IsZero() && !e.gone {
			last[id], e.wroteActive, e.touched = e.touched, e.touched, time.Time{}
		}
	}
	t.mu.Unlock()
	var wg sync.WaitGroup
	for id, at := range last {
		wg.Go(func() {
			if err := t.store.MarkActiveSince(ctx, id, at); err != nil && !errors.Is(err, sessions.ErrNotFound) {
				slog.Warn("session activity not written at shutdown", "session", id, "err", err)
			}
		})
	}
	wg.Wait()
}

// Run beats until ctx is done, several times in each Every, so that a write
// is never late by more than a fraction of it.
func (t *Tracker) Run(ctx context.Context) {
	tick := time.NewTicker(max(t.every/6, time.Millisecond))
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			t.Beat(ctx)
		}
	}
}
