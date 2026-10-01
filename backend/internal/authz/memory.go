package authz

import (
	"context"
	"sync"
)

type Memory struct {
	mu     sync.RWMutex
	owner  map[string]string
	admins map[string]bool
}

func NewMemory() *Memory {
	return &Memory{owner: map[string]string{}, admins: map[string]bool{}}
}

func (m *Memory) Check(_ context.Context, userID, sessionID string, _ Permission) (bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	owner, ok := m.owner[sessionID]
	if !ok {
		return false, nil
	}
	return owner == userID || m.admins[userID], nil
}

func (m *Memory) AddSession(_ context.Context, sessionID, ownerID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.owner[sessionID] = ownerID
	return nil
}

func (m *Memory) RemoveSession(_ context.Context, sessionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.owner, sessionID)
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
