// Package proxy forwards per-session traffic (MCP, uploads, VNC) to the
// session's pod, waking it first if it is asleep.
package proxy

import (
	"context"
	"errors"
	"time"

	"github.com/r33drichards/browserjs-sessions/backend/internal/sessions"
)

var (
	ErrStopped  = errors.New("session is stopped; resume it first")
	ErrFailed   = errors.New("session failed to start")
	ErrNotReady = errors.New("session did not become ready in time")
)

type Waker struct {
	Store   *sessions.Store
	Timeout time.Duration
	Poll    time.Duration
}

// EnsureAwake returns the session once it is running, resuming it if it was
// put to sleep for being idle. A session the user stopped is left stopped.
func (w *Waker) EnsureAwake(ctx context.Context, id string) (sessions.Session, error) {
	ctx, cancel := context.WithTimeout(ctx, w.Timeout)
	defer cancel()
	resumed := false
	for {
		s, err := w.Store.Get(ctx, id)
		if err != nil {
			if ctx.Err() != nil {
				return sessions.Session{}, ErrNotReady
			}
			return sessions.Session{}, err
		}
		switch s.State {
		case sessions.Running:
			if s.PodIP != "" {
				return s, nil
			}
		case sessions.Stopped:
			return sessions.Session{}, ErrStopped
		case sessions.Failed:
			return sessions.Session{}, ErrFailed
		case sessions.Asleep:
			if !resumed {
				if err := w.Store.Resume(ctx, id); err != nil {
					return sessions.Session{}, err
				}
				resumed = true
			}
		}
		// starting, stopping, or just resumed: wait.
		select {
		case <-ctx.Done():
			return sessions.Session{}, ErrNotReady
		case <-time.After(w.Poll):
		}
	}
}
