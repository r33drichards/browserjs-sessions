package billingtest

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/r33drichards/computer-use/backend/internal/billing"
	"github.com/r33drichards/computer-use/backend/internal/sessions"
)

// Sessions is the session store as a map with a state machine: a session
// is running, asleep or stopped. It counts what was asked of it, so a test
// can say "Create was never called". It is every interface the store sits
// behind: billing.Sessions, and those of api, proxy, idle and authz.
type Sessions struct {
	clock billing.Clock

	mu      sync.Mutex
	byID    map[string]*session
	next    int
	creates int
	wakes   int
	sleeps  []string // the reason of each Sleep that went through
}

type session struct {
	sessions.Session
	readySince time.Time
	snapshots  int
}

func NewSessions(clock billing.Clock) *Sessions {
	return &Sessions{clock: clock, byID: map[string]*session{}}
}

// id is a session ID as the store makes them: "s-" and ten of a-z2-7.
func id(n int) string {
	const letters = "abcdefghijklmnopqrstuvwxyz"
	b := []byte("aaaaaaaaaa")
	for i := len(b) - 1; i >= 0 && n > 0; i-- {
		b[i] = letters[n%len(letters)]
		n /= len(letters)
	}
	return "s-" + string(b)
}

func (s *Sessions) run(e *session) {
	e.State, e.StoppedBy, e.StateSaved = sessions.Running, "", false
	e.PodIP = "10.0.0.1"
	e.readySince = s.clock.Now()
	e.LastActive, e.InFlight = s.clock.Now(), nil
}

func (s *Sessions) suspend(e *session, state sessions.State, by string) {
	e.State, e.StoppedBy, e.PodIP, e.StateSaved = state, by, "", false
	e.Draining, e.DrainingSince = "", time.Time{}
	e.LastActive, e.InFlight = time.Time{}, nil
}

// Mark writes what a replica says of a session's use, as the store does.
func (s *Sessions) Mark(_ context.Context, id string, a sessions.Activity) (sessions.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.byID[id]
	if !ok {
		return sessions.Session{}, sessions.ErrNotFound
	}
	if !a.Active.IsZero() {
		e.LastActive = a.Active
	}
	if !a.InFlightUntil.IsZero() {
		// A new map: the sessions handed out share the old one.
		marks := map[string]time.Time{a.Replica: a.InFlightUntil}
		for replica, until := range e.InFlight {
			if replica != a.Replica {
				marks[replica] = until
			}
		}
		e.InFlight = marks
	}
	return e.Session, nil
}

// MarkActiveSince moves a running session's last use forward to at.
func (s *Sessions) MarkActiveSince(_ context.Context, id string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.byID[id]
	if !ok {
		return sessions.ErrNotFound
	}
	if (e.State == sessions.Running || e.State == sessions.Starting) && e.LastActive.Before(at) {
		e.LastActive = at
	}
	return nil
}

// ForgetInFlight removes the replicas' in-flight marks.
func (s *Sessions) ForgetInFlight(_ context.Context, id string, replicas []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.byID[id]
	if !ok {
		return sessions.ErrNotFound
	}
	marks := map[string]time.Time{}
	for replica, until := range e.InFlight {
		if !slices.Contains(replicas, replica) {
			marks[replica] = until
		}
	}
	e.InFlight = marks
	return nil
}

// CreateWithPolicy makes a session that is running at once.
func (s *Sessions) CreateWithPolicy(_ context.Context, name, owner string, _ *sessions.PolicySpec) (sessions.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.creates++
	if strings.TrimSpace(name) == "" {
		return sessions.Session{}, sessions.ErrInvalidName
	}
	if owner == "" {
		return sessions.Session{}, sessions.ErrOwnerRequired
	}
	s.next++
	e := &session{Session: sessions.Session{ID: id(s.next), Name: name, Owner: owner, Created: s.clock.Now()}}
	s.run(e)
	s.byID[e.ID] = e
	return e.Session, nil
}

func (s *Sessions) Create(ctx context.Context, name, owner string, policy *sessions.PolicySpec) (sessions.Session, error) {
	return s.CreateWithPolicy(ctx, name, owner, policy)
}

func (s *Sessions) Get(_ context.Context, id string) (sessions.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.byID[id]
	if !ok {
		return sessions.Session{}, sessions.ErrNotFound
	}
	return e.Session, nil
}

func (s *Sessions) list(match func(sessions.Session) bool) []sessions.Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []sessions.Session{}
	for _, e := range s.byID {
		if match(e.Session) {
			out = append(out, e.Session)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (s *Sessions) List(_ context.Context, owner string) ([]sessions.Session, error) {
	if owner == "" {
		return nil, sessions.ErrOwnerRequired
	}
	return s.list(func(e sessions.Session) bool { return e.Owner == owner }), nil
}

func (s *Sessions) ListAll(context.Context) ([]sessions.Session, error) {
	return s.list(func(sessions.Session) bool { return true }), nil
}

// Sleep takes a snapshot of a running session, then suspends it with the
// reason. stillWanted is asked after the snapshot, as the store asks it;
// a no discards the snapshot.
func (s *Sessions) Sleep(_ context.Context, id, stoppedBy string, stillWanted func(sessions.Session) bool) error {
	switch stoppedBy {
	case sessions.StoppedByIdle, sessions.StoppedBySleep, sessions.StoppedByCredit, sessions.StoppedByPaymentMethod, sessions.StoppedByBlocked:
	default:
		return fmt.Errorf("sleep: unknown reason %q", stoppedBy)
	}
	s.mu.Lock()
	e, ok := s.byID[id]
	if !ok {
		s.mu.Unlock()
		return sessions.ErrNotFound
	}
	if e.State != sessions.Running && e.State != sessions.Starting {
		s.mu.Unlock()
		return sessions.ErrStateChanged
	}
	snapshot := e.State == sessions.Running
	fresh := e.Session
	s.mu.Unlock()

	// Asked without the lock: it reads the store.
	if stillWanted != nil && !stillWanted(fresh) {
		return sessions.ErrStateChanged
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok = s.byID[id]; !ok {
		return sessions.ErrNotFound
	}
	if e.State != sessions.Running && e.State != sessions.Starting {
		return sessions.ErrStateChanged
	}
	if snapshot {
		e.snapshots++
	}
	state := sessions.Asleep
	if stoppedBy == sessions.StoppedByBlocked {
		state = sessions.Stopped
	}
	s.suspend(e, state, stoppedBy)
	e.StateSaved = snapshot
	s.sleeps = append(s.sleeps, stoppedBy)
	return nil
}

// Wake resumes a session that is asleep; one its user stopped stays.
func (s *Sessions) Wake(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.wakes++
	e, ok := s.byID[id]
	switch {
	case !ok:
		return sessions.ErrNotFound
	case e.State == sessions.Stopped:
		return sessions.ErrStateChanged
	case e.State == sessions.Asleep:
		s.run(e)
	}
	return nil
}

// ColdStart has no snapshot to give up on.
func (s *Sessions) ColdStart(context.Context, string) (bool, error) { return false, nil }

func (s *Sessions) Update(_ context.Context, id string, name *string, action string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.byID[id]
	if !ok {
		return sessions.ErrNotFound
	}
	switch action {
	case "", sessions.ActionStop, sessions.ActionResume:
	default:
		return sessions.ErrInvalidAction
	}
	if name != nil {
		if strings.TrimSpace(*name) == "" {
			return sessions.ErrInvalidName
		}
		e.Name = strings.TrimSpace(*name)
	}
	switch action {
	case sessions.ActionStop:
		s.suspend(e, sessions.Stopped, sessions.StoppedByUser)
	case sessions.ActionResume:
		if e.State != sessions.Running {
			s.run(e)
		}
	}
	return nil
}

func (s *Sessions) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.byID[id]; !ok {
		return sessions.ErrNotFound
	}
	delete(s.byID, id)
	return nil
}

func (s *Sessions) SetDraining(_ context.Context, id, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.byID[id]
	switch {
	case !ok:
		return sessions.ErrNotFound
	case reason == "":
		e.Draining, e.DrainingSince = "", time.Time{}
	case e.State != sessions.Running && e.State != sessions.Starting:
		return sessions.ErrStateChanged
	case e.Draining != reason:
		e.Draining, e.DrainingSince = reason, s.clock.Now()
	}
	return nil
}

// SetState puts a session in a state the store's own methods do not lead
// to (starting, failed).
func (s *Sessions) SetState(id string, state sessions.State) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.byID[id]; ok {
		e.State = state
		if state != sessions.Running {
			e.PodIP = ""
		}
	}
}

// Snapshots is how many snapshots were taken of a session.
func (s *Sessions) Snapshots(id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.byID[id]; ok {
		return e.snapshots
	}
	return 0
}

// Creates is how many times a create was asked for, whatever came of it.
func (s *Sessions) Creates() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.creates
}

// Wakes is how many times Wake was called.
func (s *Sessions) Wakes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.wakes
}

// Len is how many sessions there are.
func (s *Sessions) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.byID)
}

// observedByAccount is every session as the observer's tick sees it, by
// the name of its owner's Account.
func (s *Sessions) observedByAccount(diskGB int64) map[string]map[string]Observed {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]map[string]Observed{}
	for id, e := range s.byID {
		account := billing.AccountName(e.Owner)
		if out[account] == nil {
			out[account] = map[string]Observed{}
		}
		o := Observed{Awake: e.State == sessions.Running, DiskGB: diskGB}
		if o.Awake {
			since := e.readySince
			o.ReadySince = &since
		}
		out[account][id] = o
	}
	return out
}

// InFlight is the proxy's calls in flight, as numbers the test sets.
type InFlight struct {
	mu     sync.Mutex
	calls  map[string]int
	closed map[string]int
}

func NewInFlight() *InFlight { return &InFlight{calls: map[string]int{}, closed: map[string]int{}} }

func (f *InFlight) Calls(id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[id]
}

// Replica is the name of the replica these are the calls of.
func (f *InFlight) Replica() string { return "test" }

func (f *InFlight) CloseStreams(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed[id]++
}

// SetCalls says how many calls a session has in flight.
func (f *InFlight) SetCalls(id string, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[id] = n
}

// Closed is how many times a session's streams were closed.
func (f *InFlight) Closed(id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed[id]
}
