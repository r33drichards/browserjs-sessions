package idle

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/r33drichards/browserjs-sessions/backend/internal/sessions"
)

// Sweep puts every running session that has been idle too long to sleep, and
// stops tracking sessions that no longer exist.
func Sweep(ctx context.Context, store *sessions.Store, t *Tracker) error {
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
	for _, id := range t.Idle(running) {
		switch err := store.Suspend(ctx, id, sessions.StoppedByIdle); {
		case err == nil:
			t.Reset(id)
			slog.Info("session put to sleep", "session", id)
		case errors.Is(err, sessions.ErrStateChanged), errors.Is(err, sessions.ErrNotFound):
			// Its user stopped or deleted it first; nothing left to do.
		default:
			slog.Error("idle suspend failed", "session", id, "err", err)
		}
	}
	return nil
}

// Run sweeps every interval until ctx is done.
func Run(ctx context.Context, store *sessions.Store, t *Tracker, interval time.Duration) {
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
