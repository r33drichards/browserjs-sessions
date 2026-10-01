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
	Timeout time.Duration // how long to wait for a session; 3m if unset
	Poll    time.Duration // how often to look; 1s if unset
}

// EnsureAwake returns the session once it is running, resuming it if it was
// put to sleep for being idle. A session the user stopped is left stopped.
// If the caller's own context ends first, its error is returned; ErrNotReady
// means the session itself took longer than Timeout.
func (w *Waker) EnsureAwake(ctx context.Context, id string) (sessions.Session, error) {
	timeout, poll := w.Timeout, w.Poll
	if timeout <= 0 {
		timeout = 3 * time.Minute
	}
	if poll <= 0 {
		poll = time.Second
	}
	caller := ctx
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// ended says why the wait is over, when err may be either context's.
	ended := func(err error) error {
		switch {
		case caller.Err() != nil:
			return caller.Err()
		case ctx.Err() != nil:
			return ErrNotReady
		}
		return err
	}
	woken := false
	for {
		s, err := w.Store.Get(ctx, id)
		if err != nil {
			return sessions.Session{}, ended(err)
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
			if !woken {
				// Wake only undoes an idle sleep: if the user stopped the
				// session after we looked, it stays stopped.
				err := w.Store.Wake(ctx, id)
				if errors.Is(err, sessions.ErrStateChanged) {
					return sessions.Session{}, ErrStopped
				}
				if err != nil {
					return sessions.Session{}, ended(err)
				}
				woken = true
			}
		}
		// starting, stopping, or just resumed: wait.
		select {
		case <-ctx.Done():
			return sessions.Session{}, ended(ctx.Err())
		case <-time.After(poll):
		}
	}
}
