package proxy

import (
	"context"
	"sync"
)

// viewers tracks the open VNC connections so they can be ended on shutdown.
type viewers struct {
	mu     sync.Mutex
	next   int
	open   map[int]context.CancelFunc
	closed bool
	gone   chan struct{} // closed when the last one leaves; nil if none are open
}

// join returns a context for a viewer's connection that shutdown cancels.
// The viewer calls leave when its connection is over.
func (v *viewers) join(ctx context.Context) (_ context.Context, leave func()) {
	ctx, cancel := context.WithCancel(ctx)
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed { // too late: it ends at once
		cancel()
		return ctx, func() {}
	}
	if v.open == nil {
		v.open = map[int]context.CancelFunc{}
		v.gone = make(chan struct{})
	}
	key := v.next
	v.next++
	v.open[key] = cancel
	return ctx, func() {
		cancel()
		v.mu.Lock()
		defer v.mu.Unlock()
		delete(v.open, key)
		if len(v.open) == 0 {
			close(v.gone)
			v.open, v.gone = nil, nil
		}
	}
}

func (v *viewers) shutdown(ctx context.Context) error {
	v.mu.Lock()
	v.closed = true
	for _, cancel := range v.open {
		cancel()
	}
	gone := v.gone
	v.mu.Unlock()
	if gone == nil {
		return nil
	}
	select {
	case <-gone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
