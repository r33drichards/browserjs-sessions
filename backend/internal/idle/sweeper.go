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

// DefaultMargin is added to the idle period before a session is put to
// sleep. The moment a session was last used is read off one replica's clock
// and compared with another's; the margin is for the two to disagree.
const DefaultMargin = 5 * time.Second

// How long after an in-flight mark ran out it is removed from a session
// that is still running: its replica is gone, or has no more calls.
const staleMark = 10 * time.Minute

// Rule is when a session counts as idle.
type Rule struct {
	After  time.Duration // the idle period (IDLE_AFTER)
	Margin time.Duration // see DefaultMargin
	// Source says when a session was last used: an Activity. Nil reads the
	// annotation on the session, which is what Tracker writes.
	Source interface {
		LastActive(s sessions.Session) time.Time
	}
}

func (r Rule) lastActive(s sessions.Session) time.Time {
	if r.Source != nil {
		return r.Source.LastActive(s)
	}
	return s.LastActive
}

// Idle reports whether s, at now, is running and has not been used for the
// idle period. It is decided from s alone. A session nobody has said
// anything about is not idle: its period has yet to start.
func (r Rule) Idle(s sessions.Session, now time.Time) bool {
	last := r.lastActive(s)
	return s.State == sessions.Running && !last.IsZero() && now.Sub(last) >= r.After+r.Margin
}

// Store is what the sweep needs of the session store (a *sessions.Store).
type Store interface {
	ListAll(ctx context.Context) ([]sessions.Session, error)
	Mark(ctx context.Context, id string, a sessions.Activity) (sessions.Session, error)
	ForgetInFlight(ctx context.Context, id string, replicas []string) error
	Sleep(ctx context.Context, id, stoppedBy string, stillWanted func(sessions.Session) bool) error
}

// Sweep puts every running session that has been idle too long to sleep.
// Sessions go to sleep side by side (each may first take a snapshot, which
// takes a while), and Sweep returns when all have.
//
// It keeps nothing between runs and needs nothing from the replica it runs
// on: a session is idle if its own annotation says so, and it is suspended
// only if that is still what the annotation says in the read the suspending
// write is conditional on (sessions.Store.Sleep). A replica that proxies a
// call writes the annotation first, so of the call and the sleep the one
// that comes second sees the other: the session stays awake, or the call
// finds it asleep and wakes it.
//
// A running session with no annotation (it was started by a backend from
// before the annotation, or by hand) starts its idle period at this sweep.
//
// One replica at a time sweeps (internal/leader). Two sweeping at once would
// only do the work twice: every step is a conditional write.
func Sweep(ctx context.Context, store Store, rule Rule, now func() time.Time) error {
	all, err := store.ListAll(ctx)
	if err != nil {
		return err
	}
	at := now()
	var wg sync.WaitGroup
	for _, s := range all {
		if s.State != sessions.Running {
			continue
		}
		id := s.ID
		if rule.lastActive(s).IsZero() {
			if _, err := store.Mark(ctx, id, sessions.Activity{Active: at}); err != nil && !errors.Is(err, sessions.ErrNotFound) {
				slog.Error("idle period not started", "session", id, "err", err)
			}
			continue
		}
		var stale []string
		for replica, until := range s.InFlight {
			if at.Sub(until) >= staleMark {
				stale = append(stale, replica)
			}
		}
		if len(stale) > 0 {
			if err := store.ForgetInFlight(ctx, id, stale); err != nil && !errors.Is(err, sessions.ErrNotFound) {
				slog.Warn("stale in-flight marks not removed", "session", id, "err", err)
			}
		}
		if !rule.Idle(s, at) {
			continue
		}
		wg.Go(func() {
			stillIdle := func(fresh sessions.Session) bool { return rule.Idle(fresh, now()) }
			switch err := store.Sleep(ctx, id, sessions.StoppedByIdle, stillIdle); {
			case err == nil:
				slog.Info("session put to sleep", "session", id)
			case errors.Is(err, sessions.ErrStateChanged), errors.Is(err, sessions.ErrNotFound):
				// Its user used, stopped or deleted it first; nothing left to do.
			default:
				slog.Error("idle suspend failed", "session", id, "err", err)
			}
		})
	}
	wg.Wait()
	return nil
}

func passResult(err error) string {
	if err != nil {
		return "error"
	}
	return "ok"
}

// Run sweeps every interval until ctx is done.
func Run(ctx context.Context, store Store, rule Rule, interval time.Duration) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			err := Sweep(ctx, store, rule, time.Now)
			metrics.Passes.WithLabelValues("idle", passResult(err)).Inc()
			if err != nil {
				slog.Error("idle sweep failed", "err", err)
			}
		}
	}
}
