package idle

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/r33drichards/computer-use/backend/internal/sessions"
)

// Store is what the sweep needs of the session store (a *sessions.Store).
type Store interface {
	ListAll(ctx context.Context) ([]sessions.Session, error)
	Sleep(ctx context.Context, id, stoppedBy string, stillWanted func() bool) error
}

// Sweep puts every running session that has been idle too long to sleep, and
// stops tracking sessions that no longer exist. Sessions go to sleep side by
// side (each may first take a snapshot, which takes a while), and Sweep
// returns when all have: a session used in the meantime is left running.
func Sweep(ctx context.Context, store Store, t *Tracker) error {
	all, err := store.ListAll(ctx)
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(all))
	var running []string
	for _, s := range all {
		ids = append(ids, s.ID)
		if s.State == sessions.Running {
			running = append(running, s.ID)
		}
	}
	t.Retain(ids)
	var wg sync.WaitGroup
	for _, id := range t.Idle(running) {
		wg.Go(func() {
			switch err := store.Sleep(ctx, id, sessions.StoppedByIdle, func() bool { return t.StillIdle(id) }); {
			case err == nil:
				t.Reset(id)
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

// Run sweeps every interval until ctx is done.
func Run(ctx context.Context, store Store, t *Tracker, interval time.Duration) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if err := Sweep(ctx, store, t); err != nil {
				slog.Error("idle sweep failed", "err", err)
			}
		}
	}
}
