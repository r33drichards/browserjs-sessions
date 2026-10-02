// Package billingtest is the in-memory fakes of billing's interfaces
// (docs/contracts/billing/testing.md): a clock the test sets, accounts in a
// map, a Metronome that holds the credit and runs the metering step, a
// Stripe that holds what its events refer to, a session store with a state machine, and the
// proxy's calls in flight as numbers. No test that uses them needs a
// Kubernetes API or Stripe.
package billingtest

import (
	"sync"
	"time"
)

// Clock is a clock the test sets and advances.
type Clock struct {
	mu  sync.Mutex
	now time.Time
}

func NewClock(now time.Time) *Clock { return &Clock{now: now.UTC()} }

func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *Clock) Set(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = now.UTC()
}

func (c *Clock) Advance(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	return c.now
}
