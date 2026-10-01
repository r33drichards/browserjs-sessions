package proxy

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

const ticketTTL = 30 * time.Second

type tickets struct {
	now func() time.Time
	mu  sync.Mutex
	m   map[string]ticket
}

type ticket struct {
	session string
	expires time.Time
}

func newTickets(now func() time.Time) *tickets { return &tickets{now: now, m: map[string]ticket{}} }

func (t *tickets) Issue(session string) string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	id := hex.EncodeToString(b)
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	for k, v := range t.m { // tickets are short-lived; sweep on issue
		if !v.expires.After(now) {
			delete(t.m, k)
		}
	}
	t.m[id] = ticket{session: session, expires: now.Add(ticketTTL)}
	return id
}

// Redeem consumes a ticket; it is valid once, for the session it was issued for.
func (t *tickets) Redeem(id, session string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	tk, ok := t.m[id]
	delete(t.m, id)
	return ok && tk.session == session && tk.expires.After(t.now())
}
