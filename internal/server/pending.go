package server

import (
	"sync"
	"time"
)

// PendingApproval represents a tool call waiting for human override during the grace period.
type PendingApproval struct {
	ID         string    `json:"id"`
	ToolName   string    `json:"tool_name"`
	ToolInput  string    `json:"tool_input"`
	Reason     string    `json:"reason"`
	Confidence float64   `json:"confidence"`
	CreatedAt  time.Time `json:"created_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	ResultCh   chan string `json:"-"`
}

// PendingStore is a thread-safe store for grace-period pending approvals.
type PendingStore struct {
	mu    sync.Mutex
	items map[string]*PendingApproval
}

func NewPendingStore() *PendingStore {
	return &PendingStore{
		items: make(map[string]*PendingApproval),
	}
}

func (ps *PendingStore) Add(p *PendingApproval) {
	ps.mu.Lock()
	ps.items[p.ID] = p
	ps.mu.Unlock()
}

func (ps *PendingStore) Remove(id string) {
	ps.mu.Lock()
	delete(ps.items, id)
	ps.mu.Unlock()
}

// Resolve sends a decision to the pending approval's result channel.
// Returns false if the ID doesn't exist (already resolved or expired).
func (ps *PendingStore) Resolve(id, decision string) bool {
	ps.mu.Lock()
	p, ok := ps.items[id]
	if ok {
		delete(ps.items, id)
	}
	ps.mu.Unlock()

	if !ok {
		return false
	}

	select {
	case p.ResultCh <- decision:
	default:
	}
	return true
}

func (ps *PendingStore) Get(id string) *PendingApproval {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return ps.items[id]
}
