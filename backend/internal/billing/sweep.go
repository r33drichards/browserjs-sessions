package billing

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/r33drichards/browserjs-sessions/backend/internal/sessions"
)

// stop is whether an account's running sessions are to be put to sleep
// now, and for which reason ("" for none).
func (e *Enforcer) stop(st standing) string {
	d := Decide(OpRunning, e.inputs(st))
	switch d.Action {
	case SleepNow:
		if d.Row == 2 {
			return sessions.StoppedByBlocked
		}
		return sessions.StoppedByPaymentMethod
	case StopSequence:
		// The grace: a top-up in progress must not kill work.
		if at := st.exhaustedAt(); at != nil && !e.clock.Now().Before(at.Add(e.cfg.Grace)) {
			return sessions.StoppedByCredit
		}
	}
	return ""
}

// byOwner groups sessions by owner, owners in a stable order.
func byOwner(all []sessions.Session) (owners []string, of map[string][]sessions.Session) {
	of = map[string][]sessions.Session{}
	for _, s := range all {
		if s.Owner == "" {
			continue
		}
		if _, seen := of[s.Owner]; !seen {
			owners = append(owners, s.Owner)
		}
		of[s.Owner] = append(of[s.Owner], s)
	}
	sort.Strings(owners)
	return owners, of
}

// Sweep is the stop sequence (enforcement.md): for every account that is
// out of credit past the grace, has no payment method, or is blocked, each
// running session is marked draining, left to finish the calls it has in
// flight (or until BILLING_DRAIN_TIMEOUT), then snapshotted and put to
// sleep with the reason. A mark whose reason has gone (credit arrived, a
// card is back) is removed. It reads each account from the cluster as it
// runs and keeps nothing between runs but what is on the sessions.
//
// In meter mode it stops nothing and logs what it would have.
func (e *Enforcer) Sweep(ctx context.Context, inflight InFlight) error {
	if e.cfg.Mode == Off {
		return nil
	}
	all, err := e.sessions.ListAll(ctx)
	if err != nil {
		return err
	}
	owners, of := byOwner(all)
	var wg sync.WaitGroup
	for _, owner := range owners {
		st, err := e.standing(ctx, owner)
		if err != nil {
			slog.Error("billing sweep: account not read", "owner", AccountName(owner), "err", err)
			continue
		}
		reason := e.stop(st)
		for _, s := range of[owner] {
			if s.State != sessions.Running && s.State != sessions.Starting {
				continue
			}
			want := reason
			if want == "" {
				if s.Draining != "" && e.cfg.Mode == Enforce {
					if err := e.sessions.SetDraining(ctx, s.ID, ""); err != nil && !gone(err) {
						slog.Error("billing sweep: draining mark not removed", "session", s.ID, "err", err)
					} else {
						slog.Info("billing: drain called off", "session", s.ID, "account", st.account.Name)
					}
				}
				continue
			}
			if e.cfg.Mode != Enforce {
				slog.Info("would_stop", "session", s.ID, "account", st.account.Name, "reason", want)
				continue
			}
			if !e.drained(ctx, s, want, inflight) {
				continue
			}
			wg.Go(func() { e.sleep(ctx, s.ID, owner, want) })
		}
	}
	wg.Wait()
	return nil
}

func gone(err error) bool {
	return errors.Is(err, sessions.ErrStateChanged) || errors.Is(err, sessions.ErrNotFound)
}

// drained marks s as draining for reason and reports whether it can be put
// to sleep now: it has no call in flight, or it has been draining for
// BILLING_DRAIN_TIMEOUT. A session that is still starting has nothing to
// drain.
func (e *Enforcer) drained(ctx context.Context, s sessions.Session, reason string, inflight InFlight) bool {
	if s.State == sessions.Starting {
		return true
	}
	now := e.clock.Now()
	since := s.DrainingSince
	if s.Draining != reason {
		if err := e.sessions.SetDraining(ctx, s.ID, reason); err != nil {
			if !gone(err) {
				slog.Error("billing sweep: session not marked draining", "session", s.ID, "err", err)
			}
			return false
		}
		slog.Info("billing: draining", "session", s.ID, "reason", reason)
		since = now
	}
	if since.IsZero() {
		since = now
	}
	if inflight == nil {
		return true
	}
	// From the mark on the proxy refuses new requests; its viewers and
	// event streams are not work in progress.
	inflight.CloseStreams(s.ID)
	if calls := inflight.Calls(s.ID); calls > 0 && now.Sub(since) < e.cfg.DrainTimeout {
		slog.Info("billing: waiting for calls in flight", "session", s.ID, "calls", calls)
		return false
	}
	return true
}

// sleep snapshots a session and suspends it with the reason. Credit or a
// card arriving while the snapshot is taken leaves it running.
func (e *Enforcer) sleep(ctx context.Context, id, owner, reason string) {
	stillWanted := func() bool {
		st, err := e.standing(ctx, owner)
		if err != nil {
			return true
		}
		return e.stop(st) != ""
	}
	switch err := e.sessions.Sleep(ctx, id, reason, stillWanted); {
	case err == nil:
		slog.Info("billing: session put to sleep", "session", id, "reason", reason)
	case errors.Is(err, sessions.ErrStateChanged):
		// Its user stopped it, or its owner's credit came back: the mark,
		// if it is still there, goes at the next sweep.
	case errors.Is(err, sessions.ErrNotFound):
	default:
		slog.Error("billing sweep: sleep failed; retried at the next sweep", "session", id, "err", err)
	}
}

// DeletePass deletes the sessions (disks and snapshots with them) of every
// account that has been at zero for ZERO_BALANCE_DELETE_AFTER. The Account,
// its history and any later credit remain. It does nothing unless
// ZERO_BALANCE_DELETE is on and billing is enforced.
func (e *Enforcer) DeletePass(ctx context.Context) error {
	if !e.cfg.ZeroBalanceDelete || e.cfg.Mode != Enforce {
		return nil
	}
	all, err := e.sessions.ListAll(ctx)
	if err != nil {
		return err
	}
	owners, of := byOwner(all)
	now := e.clock.Now()
	for _, owner := range owners {
		st, err := e.standing(ctx, owner)
		if err != nil {
			slog.Error("billing deletion: account not read", "owner", AccountName(owner), "err", err)
			continue
		}
		at := e.deleteAt(st)
		if at == nil || now.Before(*at) {
			continue
		}
		for _, s := range of[owner] {
			if err := e.sessions.Delete(ctx, s.ID); err != nil && !errors.Is(err, sessions.ErrNotFound) {
				slog.Error("billing deletion: session not deleted", "session", s.ID, "err", err)
				continue
			}
			slog.Warn("billing: session deleted, its account at zero since", "session", s.ID, "account", st.account.Name, "since", at.Add(-e.cfg.ZeroBalanceDeleteAfter))
		}
	}
	return nil
}

// Run sweeps every interval, and makes the deletion pass every
// deleteInterval, until ctx is done.
func (e *Enforcer) Run(ctx context.Context, inflight InFlight, interval, deleteInterval time.Duration) {
	sweep := time.NewTicker(interval)
	defer sweep.Stop()
	deletion := time.NewTicker(deleteInterval)
	defer deletion.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-sweep.C:
			if err := e.Sweep(ctx, inflight); err != nil {
				slog.Error("billing sweep failed", "err", err)
			}
		case <-deletion.C:
			if err := e.DeletePass(ctx); err != nil {
				slog.Error("billing deletion pass failed", "err", err)
			}
		}
	}
}
