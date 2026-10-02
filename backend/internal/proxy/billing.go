package proxy

import (
	"context"
	"net/http"
	"strings"
	"sync"

	"github.com/r33drichards/browserjs-sessions/backend/internal/billing"
	"github.com/r33drichards/browserjs-sessions/backend/internal/sessions"
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

// flights is the work in progress on each session: the calls being proxied
// now (MCP calls and uploads), which a drain waits for, and the streams
// (VNC viewers, MCP event streams), which it closes.
type flights struct {
	mu      sync.Mutex
	calls   map[string]int
	next    int
	streams map[string]map[int]context.CancelFunc
}

// call counts a call in flight until the returned func is called.
func (f *flights) call(id string) (done func()) {
	f.mu.Lock()
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	f.calls[id]++
	f.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.calls[id]--; f.calls[id] <= 0 {
				delete(f.calls, id)
			}
		})
	}
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

// Calls is how many MCP calls and uploads to the session are being proxied
// now. With CloseStreams it makes the Proxy a billing.InFlight.
func (p *Proxy) Calls(id string) int {
	p.flights.mu.Lock()
	defer p.flights.mu.Unlock()
	return p.flights.calls[id]
}

// CloseStreams ends the session's VNC viewers and MCP event streams, and
// forgets what is remembered of the session, so that the next request
// sees it as the cluster has it (draining).
func (p *Proxy) CloseStreams(id string) {
	p.flights.mu.Lock()
	for _, cancel := range p.flights.streams[id] {
		cancel()
	}
	p.flights.mu.Unlock()
	p.Waker.Invalidate(id)
}
