package idle

import (
	"context"
	"log/slog"
	"time"

	"github.com/r33drichards/browserjs-sessions/backend/internal/sessions"
)

// Sweep puts every running session that has been idle too long to sleep, and
// stops tracking sessions that no longer exist.
func Sweep(ctx context.Context, store *sessions.Store, t *Tracker) error {
	all, err := store.List(ctx, "")
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
		if err := store.Suspend(ctx, id, sessions.StoppedByIdle); err != nil {
			slog.Error("idle suspend failed", "session", id, "err", err)
			continue
		}
		slog.Info("session put to sleep", "session", id)
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
