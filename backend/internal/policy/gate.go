package policy

import (
	"context"
	"sync"
	"time"

	"github.com/r33drichards/browserjs-sessions/backend/internal/sessions"
)

// SessionStore is the part of the session store that finds the pod behind a
// session (what the proxy's Waker needs).
type SessionStore interface {
	Get(ctx context.Context, id string) (sessions.Session, error)
	Wake(ctx context.Context, id string) error
	ColdStart(ctx context.Context, id string) (bool, error)
}

// Gated is a session store in which a session is not running until its
// first policy is in force: until then OPA has no decision for it and denies
// it everything, so a call forwarded to its pod would be refused for no
// reason of the caller's. The proxy reads sessions through this, and so
// holds a request for a new session (created, adopted from the warm pool,
// or woken) as it holds one for a session whose pod is still starting.
//
// An edit of a running session's policy does not gate it: the policy before
// stays in force until the new one is loaded.
type Gated struct {
	SessionStore
	policies *Handlers

	mu sync.Mutex
	// Sessions whose policy was seen in force, by ID, with when the session
	// was created: a policy in force stays in force for as long as its
	// session exists, and an ID can come back as another session.
	inForce map[string]time.Time
}

// Gate returns store gated by the policies of h. With policies off (h is
// nil) it is store itself.
func Gate(store SessionStore, h *Handlers) SessionStore {
	if h == nil {
		return store
	}
	return &Gated{SessionStore: store, policies: h, inForce: map[string]time.Time{}}
}

// Get is the store's Get, with a running session whose first policy is not
// in force shown as starting. A policy that cannot be read is an error: the
// session is not handed out on a guess.
func (g *Gated) Get(ctx context.Context, id string) (sessions.Session, error) {
	s, err := g.SessionStore.Get(ctx, id)
	if err != nil || s.State != sessions.Running || !s.PolicyCapable {
		return s, err
	}
	g.mu.Lock()
	created, known := g.inForce[id]
	g.mu.Unlock()
	if known && created.Equal(s.Created) {
		return s, nil
	}
	p, err := g.policies.Summary(ctx, s)
	if err != nil {
		return sessions.Session{}, err
	}
	if gated := p.Gate(s); gated.State != sessions.Running {
		return gated, nil
	}
	g.mu.Lock()
	g.inForce[id] = s.Created
	g.mu.Unlock()
	return s, nil
}
