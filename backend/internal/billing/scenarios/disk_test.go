package scenarios

import (
	"testing"
	"time"

	"github.com/r33drichards/browserjs-sessions/backend/internal/billing"
	"github.com/r33drichards/browserjs-sessions/backend/internal/billing/billingtest"
	"github.com/r33drichards/browserjs-sessions/backend/internal/sessions"
)

// A tick's gap that is still billed in full (MAX_GAP).
const longTick = 150 * time.Second

// atZero is the start of scenario 3: one session, asleep, a balance of $1,
// ticked until the disk has used it all up. It checks the first two steps
// on the way.
func atZero(t *testing.T, zeroBalanceDelete bool) (w *world, id string, exhaustedAt time.Time) {
	t.Helper()
	w = newWorld(t, func(c *billing.Config) {
		c.SignupCredit = false
		c.ZeroBalanceDelete = zeroBalanceDelete
	})
	ctx := t.Context()
	w.saveCard(user, billingtest.Card{Fingerprint: "fpA"})
	w.grant(user, 1_000_000)
	w.tick()
	id = w.create(user)
	if err := w.sessions.Sleep(ctx, id, sessions.StoppedByIdle, nil); err != nil {
		t.Fatal(err)
	}
	w.tick() // the operator's first sight of the session

	// 24 hours of ticks: 5 GB at 384 an hour, and no awake charge.
	for range 24 * time.Hour / longTick {
		w.clock.Advance(longTick)
		w.tick()
	}
	b := w.billingOf(user)
	if want := int64(1_000_000 - 5*24*384); b.BalanceMicros != want || b.Period.AwakeMicros != 0 || b.Period.DiskMicros != 5*24*384 {
		t.Fatalf("after 24 h: balance %d, awake %d, disk %d; want %d, 0, %d", b.BalanceMicros, b.Period.AwakeMicros, b.Period.DiskMicros, want, 5*24*384)
	}

	// Ticks until the balance is 0. The alert of the tick that took it
	// there is posted to the webhook, which sets exhausted.
	account := billing.AccountName(user)
	// $1 of disk at 1920 an hour is some 520 hours of ticks.
	for ticks := 0; !w.account(user).Spec.Credit.Exhausted; ticks++ {
		if ticks > 600*int(time.Hour/longTick) {
			t.Fatalf("the balance is used up and the account was never marked exhausted")
		}
		w.clock.Advance(longTick)
		w.tick()
	}
	v := w.billingOf(user)
	if v.Level != billing.LevelExhausted || v.BalanceMicros != 0 || v.ExhaustedAt == nil || !v.ExhaustedAt.Equal(w.clock.Now()) {
		t.Fatalf("at zero: level %s, balance %d, exhaustedAt %v, now %v", v.Level, v.BalanceMicros, v.ExhaustedAt, w.clock.Now())
	}
	exhaustedAt = *v.ExhaustedAt
	_, free := w.metronome.Charged(account)
	// Further ticks are used and owed by nobody; the balance stays 0 and
	// exhaustedAt keeps its start.
	for range 100 {
		w.clock.Advance(longTick)
		w.tick()
	}
	w.balancePass()
	later := w.billingOf(user)
	_, freeLater := w.metronome.Charged(account)
	if later.BalanceMicros != 0 || freeLater <= free || later.ExhaustedAt == nil || !later.ExhaustedAt.Equal(exhaustedAt) {
		t.Fatalf("past zero: balance %d, unpaid use %d (was %d), exhaustedAt %v (was %v)",
			later.BalanceMicros, freeLater, free, later.ExhaustedAt, exhaustedAt)
	}
	_ = ctx
	return w, id, exhaustedAt
}

// tickTo moves the clock to at, where the observer looks.
func (w *world) tickTo(at time.Time) {
	w.clock.Set(at)
	w.tick()
}

func (w *world) deletePass() {
	w.t.Helper()
	if err := w.enforcer.DeletePass(w.t.Context()); err != nil {
		w.t.Fatalf("deletion pass: %v", err)
	}
}

// TestDiskAccruesAsleep is scenario 3 of docs/contracts/billing/testing.md:
// a session that is asleep is charged for its disk, at zero the charge is
// overdraft, and the deletion clock runs from exhaustedAt when
// ZERO_BALANCE_DELETE is on.
func TestDiskAccruesAsleep(t *testing.T) {
	const fortnight = 14 * 24 * time.Hour

	t.Run("ZERO_BALANCE_DELETE on: deleted at exhaustedAt + 14 days", func(t *testing.T) {
		w, id, exhaustedAt := atZero(t, true)
		deleteAt := exhaustedAt.Add(fortnight)
		if b := w.billingOf(user); b.DeleteAt == nil || !b.DeleteAt.Equal(deleteAt) {
			t.Fatalf("deleteAt = %v, want %v", b.DeleteAt, deleteAt)
		}
		if v := w.view(user, id); v.DeleteAfter == nil || !v.DeleteAfter.Equal(deleteAt) {
			t.Fatalf("the session's deleteAfter = %v, want %v", v.DeleteAfter, deleteAt)
		}
		// A second before, nothing is deleted.
		w.tickTo(deleteAt.Add(-time.Second))
		w.deletePass()
		if w.sessions.Len() != 1 {
			t.Fatalf("deleted before deleteAt")
		}
		w.tickTo(deleteAt.Add(time.Hour))
		w.deletePass()
		if w.sessions.Len() != 0 {
			t.Fatalf("the session was not deleted at deleteAt")
		}
		// The Account remains.
		if acc := w.account(user); acc.Spec.Owner != user || acc.Spec.DeletedAt != nil {
			t.Fatalf("the account did not remain as it was: %+v", acc.Spec)
		}
	})

	t.Run("credit bought at day 10: the clock is gone", func(t *testing.T) {
		w, id, exhaustedAt := atZero(t, true)
		w.tickTo(exhaustedAt.Add(10 * 24 * time.Hour))
		w.buy(user, "cu_credit_5_v1")
		w.tick()
		if b := w.billingOf(user); b.ExhaustedAt != nil || b.DeleteAt != nil || b.BalanceMicros != 5_000_000 {
			t.Fatalf("after the purchase: exhaustedAt %v, deleteAt %v, balance %d", b.ExhaustedAt, b.DeleteAt, b.BalanceMicros)
		}
		if v := w.view(user, id); v.DeleteAfter != nil {
			t.Fatalf("the session still has deleteAfter %v", v.DeleteAfter)
		}
		w.tickTo(exhaustedAt.Add(fortnight + time.Hour))
		w.deletePass()
		if w.sessions.Len() != 1 {
			t.Fatalf("the session was deleted although credit arrived")
		}
	})

	t.Run("ZERO_BALANCE_DELETE off: nothing is ever deleted", func(t *testing.T) {
		w, id, exhaustedAt := atZero(t, false)
		if b := w.billingOf(user); b.DeleteAt != nil {
			t.Fatalf("deleteAt = %v with the deletion off", b.DeleteAt)
		}
		if v := w.view(user, id); v.DeleteAfter != nil {
			t.Fatalf("deleteAfter = %v with the deletion off", v.DeleteAfter)
		}
		w.tickTo(exhaustedAt.Add(60 * 24 * time.Hour))
		w.deletePass()
		if w.sessions.Len() != 1 {
			t.Fatalf("a session was deleted with the deletion off")
		}
	})

	t.Run("no card and at zero: the clock runs from when the card went", func(t *testing.T) {
		w, _, exhaustedAt := atZero(t, true)
		removed := exhaustedAt.Add(3 * 24 * time.Hour)
		w.tickTo(removed)
		pm := w.account(user).Spec.PaymentMethod.IDs[0]
		if res := w.webhook(w.stripe.DetachCard(pm)); res.Code != 200 {
			t.Fatalf("webhook %d", res.Code)
		}
		if b := w.billingOf(user); b.State != billing.StateNoCard || b.DeleteAt == nil || !b.DeleteAt.Equal(removed.Add(fortnight)) {
			t.Fatalf("state %s, deleteAt %v, want no_card and %v", b.State, b.DeleteAt, removed.Add(fortnight))
		}
	})
}
