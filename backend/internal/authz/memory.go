package authz

import (
	"context"
	"errors"
	"sync"
)

// Memory is the in-process Authorizer used in tests. It answers the way the
// Topaz model does: can_manage is owner or admin, can_view adds viewers.
type Memory struct {
	mu      sync.RWMutex
	owner   map[string]string
	viewers map[string]map[string]bool // session → users
	admins  map[string]bool
}

func NewMemory() *Memory {
	return &Memory{owner: map[string]string{}, viewers: map[string]map[string]bool{}, admins: map[string]bool{}}
}

func (m *Memory) Check(_ context.Context, userID, sessionID string, p Permission) (bool, error) {
	if userID == "" {
		return false, nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	owner, ok := m.owner[sessionID]
	if !ok {
		return false, nil
	}
	manage := owner == userID || m.admins[userID]
	switch p {
	case Manage:
		return manage, nil
	case View:
		return manage || m.viewers[sessionID][userID], nil
	default:
		return false, nil
	}
}

func (m *Memory) AddSession(_ context.Context, sessionID, ownerID string) error {
	if sessionID == "" || ownerID == "" {
		return errors.New("authz: session and owner are required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.owner[sessionID] = ownerID
	return nil
}

// AddViewer lets userID see, but not manage, a session. It is not part of
// Authorizer: nothing in the product grants it yet. Tests use it to prove a
// route asks for the right permission.
func (m *Memory) AddViewer(_ context.Context, sessionID, userID string) error {
	if userID == "" {
		return errors.New("authz: user is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.owner[sessionID]; !ok {
		return errors.New("authz: unknown session")
	}
	if m.viewers[sessionID] == nil {
		m.viewers[sessionID] = map[string]bool{}
	}
	m.viewers[sessionID][userID] = true
	return nil
}

func (m *Memory) RemoveSession(_ context.Context, sessionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.owner, sessionID)
	delete(m.viewers, sessionID)
	return nil
}

func (m *Memory) SetAdmin(_ context.Context, userID string, admin bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if admin {
		m.admins[userID] = true
	} else {
		delete(m.admins, userID)
	}
	return nil
}
