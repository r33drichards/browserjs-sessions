// Package proxy forwards per-session traffic (MCP, uploads, VNC) to the
// session's pod, waking it first if it is asleep.
package proxy

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/r33drichards/computer-use/backend/internal/sessions"
)

var (
	ErrStopped  = errors.New("session is stopped; resume it first")
	ErrFailed   = errors.New("session failed to start")
	ErrNotReady = errors.New("session did not become ready in time")
	// ErrNotRunning is Running's answer for a session that exists but has no
	// pod to send a request to (asleep, stopped, starting or failed).
	ErrNotRunning = errors.New("session is not running")
)

// How long a single read of a session, shared by its callers, may take.
const lookupTimeout = 15 * time.Second

// Store is what the Waker needs of the session store (a *sessions.Store).
type Store interface {
	Get(ctx context.Context, id string) (sessions.Session, error)
	Wake(ctx context.Context, id string) error
	ColdStart(ctx context.Context, id string) (bool, error)
}

// Waker finds the pod behind a session. Every proxied request asks it, so it
// keeps the cluster reads down: concurrent callers for one session share one
// read or one wait, and a session seen running is remembered for RunningTTL.
type Waker struct {
	Store   Store
	Timeout time.Duration // how long to wait for a session; 3m if unset
	Poll    time.Duration // how often to look; 1s if unset
	// RestoreTimeout is how long a session woken from a snapshot may take to
	// run, counted from when its pod got a node (or from the wake, while it
	// has none). After that the snapshot is given up and the session started
	// cold. 0 never gives up.
	RestoreTimeout time.Duration
	// RunningTTL is how long a session seen running is taken to still be
	// running on the same pod, without asking the cluster again. 0 asks
	// every time.
	RunningTTL time.Duration
	// Allow, if set, is asked before a sleeping session is woken: an error
	// leaves it asleep and is the wait's answer (billing; see billing.go).
	Allow func(ctx context.Context, s sessions.Session) error

	now func() time.Time // time.Now if unset

	wakes, lookups singleflight.Group // by session ID

	mu      sync.Mutex
	running map[string]seen
}

// seen is a session as it was last seen running.
type seen struct {
	session sessions.Session
	until   time.Time
}

func (w *Waker) clock() time.Time {
	if w.now != nil {
		return w.now()
	}
	return time.Now()
}

func (w *Waker) remembered(id string) (sessions.Session, bool) {
	if w.RunningTTL <= 0 {
		return sessions.Session{}, false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	e, ok := w.running[id]
	if !ok {
		return sessions.Session{}, false
	}
	if !w.clock().Before(e.until) {
		delete(w.running, id)
		return sessions.Session{}, false
	}
	return e.session, true
}

func (w *Waker) remember(s sessions.Session) {
	if w.RunningTTL <= 0 {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.clock()
	if w.running == nil {
		w.running = map[string]seen{}
	}
	for id, e := range w.running { // entries are short-lived; sweep on insert
		if !now.Before(e.until) {
			delete(w.running, id)
		}
	}
	w.running[s.ID] = seen{session: s, until: now.Add(w.RunningTTL)}
}

// Invalidate forgets what is remembered about a session's pod, after a
// request to it went wrong.
func (w *Waker) Invalidate(id string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.running, id)
}

// share runs find once for all concurrent callers asking about the same
// session. The work is not any one caller's: it runs on a context detached
// from theirs, bounded by timeout, so a caller that goes away gets its own
// context's error and leaves the rest waiting.
func share(ctx context.Context, group *singleflight.Group, id string, timeout time.Duration,
	find func(ctx context.Context) (sessions.Session, error)) (sessions.Session, error) {
	if err := ctx.Err(); err != nil {
		return sessions.Session{}, err
	}
	result := group.DoChan(id, func() (any, error) {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
		defer cancel()
		return find(ctx)
	})
	select {
	case <-ctx.Done():
		return sessions.Session{}, ctx.Err()
	case r := <-result:
		s, _ := r.Val.(sessions.Session)
		return s, r.Err
	}
}

// Running returns the session if it is running, without waking it:
// ErrNotRunning if it is not.
func (w *Waker) Running(ctx context.Context, id string) (sessions.Session, error) {
	if s, ok := w.remembered(id); ok {
		return s, nil
	}
	return share(ctx, &w.lookups, id, lookupTimeout, func(ctx context.Context) (sessions.Session, error) {
		s, err := w.Store.Get(ctx, id)
		if err != nil {
			return sessions.Session{}, err
		}
		if s.State != sessions.Running || s.PodIP == "" {
			return sessions.Session{}, ErrNotRunning
		}
		w.remember(s)
		return s, nil
	})
}

// EnsureAwake returns the session once it is running, resuming it if it was
// put to sleep for being idle. A session the user stopped is left stopped.
// If the caller's own context ends first, its error is returned; ErrNotReady
// means the session itself took longer than Timeout.
//
// Callers waiting for the same session share one wait, which began (and so
// times out) with the first of them.
func (w *Waker) EnsureAwake(ctx context.Context, id string) (sessions.Session, error) {
	if s, ok := w.remembered(id); ok {
		return s, nil
	}
	timeout := w.Timeout
	if timeout <= 0 {
		timeout = 3 * time.Minute
	}
	return share(ctx, &w.wakes, id, timeout, func(ctx context.Context) (sessions.Session, error) {
		s, err := w.await(ctx, id)
		if err == nil {
			w.remember(s)
		}
		return s, err
	})
}

// await polls the session until it runs, waking it once if it is asleep.
// ctx carries the timeout.
func (w *Waker) await(ctx context.Context, id string) (sessions.Session, error) {
	poll := w.Poll
	if poll <= 0 {
		poll = time.Second
	}
	// ended tells a wait that ran out from a failure to ask the cluster.
	ended := func(err error) error {
		if ctx.Err() != nil {
			return ErrNotReady
		}
		return err
	}
	woken := false
	// While a woken session is on its way up: since when, whether its pod
	// had a node then, and whether its snapshot was given up already.
	var since time.Time
	scheduled, cold := false, false
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
			if !woken || cold {
				return sessions.Session{}, ErrFailed
			}
		case sessions.Asleep:
			if !woken {
				// Wake only undoes an idle sleep: if the user stopped the
				// session after we looked, it stays stopped.
				if w.Allow != nil {
					if err := w.Allow(ctx, s); err != nil {
						return sessions.Session{}, err
					}
				}
				err := w.Store.Wake(ctx, id)
				if errors.Is(err, sessions.ErrStateChanged) {
					return sessions.Session{}, ErrStopped
				}
				if err != nil {
					return sessions.Session{}, ended(err)
				}
				woken, since = true, w.clock()
			}
		}
		if woken && !cold && s.State != sessions.Asleep {
			if !scheduled && s.Node != "" {
				scheduled, since = true, w.clock()
			}
			stuck := w.RestoreTimeout > 0 && w.clock().Sub(since) >= w.RestoreTimeout
			if s.State == sessions.Failed || stuck {
				// The snapshot may be what keeps it from starting (it only
				// restores on the CPU it was taken on): once, start cold.
				cold = true
				did, err := w.Store.ColdStart(ctx, id)
				switch {
				case errors.Is(err, sessions.ErrStateChanged):
					return sessions.Session{}, ErrStopped
				case err != nil:
					slog.Error("cold start after a failed restore", "session", id, "err", err)
				}
				if s.State == sessions.Failed && (!did || err != nil) {
					return sessions.Session{}, ErrFailed
				}
				continue
			}
		}
		// starting, stopping, or just resumed: wait.
		select {
		case <-ctx.Done():
			return sessions.Session{}, ErrNotReady
		case <-time.After(poll):
		}
	}
}
