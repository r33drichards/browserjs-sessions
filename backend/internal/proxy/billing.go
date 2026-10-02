package proxy

import (
	"context"
	"net/http"
	"strings"
	"sync"

	"github.com/r33drichards/computer-use/backend/internal/billing"
	"github.com/r33drichards/computer-use/backend/internal/sessions"
)

// Billing is what the proxy asks of billing (a *billing.Enforcer). With
// none (Proxy.Billing nil) nothing here refuses anything.
type Billing interface {
	// Start judges waking a sleeping session: nil, or the refusal.
	Start(ctx context.Context, s sessions.Session) error
	// Draining is the refusal for a new request to a session that is
	// being drained before a sleep, nil for one that is not.
	Draining(s sessions.Session) error
}

// awake is Waker.EnsureAwake, and running Waker.Running, for a request
// that is about to be sent to the pod: a session that is draining takes no
// new ones.
func (p *Proxy) awake(ctx context.Context, id string) (sessions.Session, error) {
	return p.undrained(ctx, id, p.Waker.EnsureAwake)
}

func (p *Proxy) running(ctx context.Context, id string) (sessions.Session, error) {
	return p.undrained(ctx, id, p.Waker.Running)
}

func (p *Proxy) undrained(ctx context.Context, id string, find func(context.Context, string) (sessions.Session, error)) (sessions.Session, error) {
	s, err := find(ctx, id)
	if err != nil || p.Billing == nil || s.Draining == "" {
		return s, err
	}
	// The mark may be the Waker's memory of it, and gone since (credit
	// arrived): ask the cluster before refusing.
	p.Waker.Invalidate(id)
	if s, err = find(ctx, id); err != nil {
		return s, err
	}
	if err := p.Billing.Draining(s); err != nil {
		return sessions.Session{}, err
	}
	return s, nil
}

// refused answers a request that billing refused, in the form its caller
// reads: a JSON-RPC error on a session's MCP endpoint, the API's error
// anywhere else. It reports whether err was a refusal.
func refused(w http.ResponseWriter, r *http.Request, err error) bool {
	ref, ok := billing.AsRefusal(err)
	if !ok {
		return false
	}
	if rt := routeOf(r); rt.id != "" && (r.URL.Path == "/mcp" || strings.HasPrefix(r.URL.Path, "/mcp/")) {
		ref.WriteMCP(w)
	} else {
		ref.WriteHTTP(w)
	}
	return true
}

// flights is the streams this replica has open to each session (VNC
// viewers, MCP event streams), which a drain closes. The calls in flight,
// which a drain waits for, are the Tracker's (Proxy.Idle): it says them on
// the session, where the replica that drains reads them.
type flights struct {
	mu      sync.Mutex
	next    int
	streams map[string]map[int]context.CancelFunc
}

// stream returns a context for a stream of a session, which CloseStreams
// cancels. The stream calls leave when it is over.
func (f *flights) stream(ctx context.Context, id string) (_ context.Context, leave func()) {
	ctx, cancel := context.WithCancel(ctx)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.streams == nil {
		f.streams = map[string]map[int]context.CancelFunc{}
	}
	if f.streams[id] == nil {
		f.streams[id] = map[int]context.CancelFunc{}
	}
	key := f.next
	f.next++
	f.streams[id][key] = cancel
	return ctx, func() {
		cancel()
		f.mu.Lock()
		defer f.mu.Unlock()
		delete(f.streams[id], key)
		if len(f.streams[id]) == 0 {
			delete(f.streams, id)
		}
	}
}

// sessions is the sessions this replica has a stream open to.
func (f *flights) sessions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	ids := make([]string, 0, len(f.streams))
	for id := range f.streams {
		ids = append(ids, id)
	}
	return ids
}

// Calls is how many MCP calls and uploads to the session this replica is
// proxying now. With CloseStreams and Replica it makes the Proxy a
// billing.InFlight.
func (p *Proxy) Calls(id string) int { return p.Idle.Calls(id) }

// Replica is the name under which this replica says, on a session, that it
// has calls in flight (sessions.AnnInFlightPrefix): the drain reads the
// marks of the others.
func (p *Proxy) Replica() string { return p.Idle.Replica() }

// CloseStreams ends this replica's VNC viewers and MCP event streams of the
// session, and forgets what is remembered of the session, so that the next
// request sees it as the cluster has it (draining).
func (p *Proxy) CloseStreams(id string) {
	p.flights.mu.Lock()
	for _, cancel := range p.flights.streams[id] {
		cancel()
	}
	p.flights.mu.Unlock()
	p.Waker.Invalidate(id)
}

// seen is told of a session as the cluster has it, each time this replica
// writes its activity or looks at one it has a stream open to
// (idle.Tracker). Another replica may have put it to sleep, or marked it
// draining, since this one last read it: what is remembered of it is then
// dropped, and the streams of one that is draining are closed here as the
// draining replica closed its own.
func (p *Proxy) seen(s sessions.Session) {
	if s.State != sessions.Running || s.Draining != "" {
		p.Waker.Invalidate(s.ID)
	}
	if p.Billing != nil && p.Billing.Draining(s) != nil {
		p.CloseStreams(s.ID)
	}
}
