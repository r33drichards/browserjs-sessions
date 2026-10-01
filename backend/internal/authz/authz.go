// Package authz answers "may this user use this session?".
//
// A session belongs to the user who created it: the owner is recorded on its
// Sandbox. The owner may see and manage the session, and so may an admin.
// Nobody else may do anything with it.
package authz

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/r33drichards/browserjs-sessions/backend/internal/auth"
	"github.com/r33drichards/browserjs-sessions/backend/internal/sessions"
)

// Checker decides whether a user may use a session. False with a nil error
// is a denial, and is also the answer for a session that does not exist. An
// error means the answer could not be had; it is never a grant.
type Checker interface {
	Allowed(ctx context.Context, u auth.User, sessionID string) (bool, error)
}

// Store is where a session's owner is looked up (a *sessions.Store).
type Store interface {
	Get(ctx context.Context, id string) (sessions.Session, error)
}

// Owners is the Checker: it allows a session's owner, and admins.
type Owners struct {
	store Store
	ttl   time.Duration
	now   func() time.Time

	mu    sync.Mutex
	owner map[string]known
}

type known struct {
	owner string
	until time.Time
}

// NewOwners checks against the owners recorded in store. A session's owner
// never changes, so it is remembered for ttl rather than read from the
// cluster on every request; 0 reads it every time.
func NewOwners(store Store, ttl time.Duration) *Owners {
	return &Owners{store: store, ttl: ttl, now: time.Now, owner: map[string]known{}}
}

func (o *Owners) Allowed(ctx context.Context, u auth.User, sessionID string) (bool, error) {
	if u.Subject == "" || !sessions.ValidID(sessionID) {
		return false, nil
	}
	owner, err := o.ownerOf(ctx, sessionID)
	if errors.Is(err, sessions.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	// A session with no owner on record is nobody's but the admins'.
	return u.Admin || (owner != "" && owner == u.Subject), nil
}

func (o *Owners) ownerOf(ctx context.Context, id string) (string, error) {
	now := o.now()
	o.mu.Lock()
	k, ok := o.owner[id]
	o.mu.Unlock()
	if ok && now.Before(k.until) {
		return k.owner, nil
	}
	s, err := o.store.Get(ctx, id)
	if err != nil {
		return "", err
	}
	if o.ttl > 0 {
		o.mu.Lock()
		for id, k := range o.owner { // entries are short-lived; sweep on insert
			if !now.Before(k.until) {
				delete(o.owner, id)
			}
		}
		o.owner[id] = known{owner: s.Owner, until: now.Add(o.ttl)}
		o.mu.Unlock()
	}
	return s.Owner, nil
}
