package session

import (
	"context"
	"sync"

	"github.com/cloudwego/eino/schema"
)

type HistoryStore interface {
	Load(ctx context.Context, sessionID string) ([]*schema.Message, error)
	Save(ctx context.Context, sessionID string, history []*schema.Message) error
	Append(ctx context.Context, sessionID string, msgs ...*schema.Message) error
	Truncate(ctx context.Context, sessionID string, keepLast int) error
}

type MemoryHistoryStore struct {
	mu      sync.RWMutex
	history map[string][]*schema.Message
}

func NewMemoryHistoryStore() *MemoryHistoryStore {
	return &MemoryHistoryStore{history: make(map[string][]*schema.Message)}
}

func (s *MemoryHistoryStore) Load(_ context.Context, sessionID string) ([]*schema.Message, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	msgs := s.history[sessionID]
	out := make([]*schema.Message, len(msgs))
	copy(out, msgs)
	return out, nil
}

func (s *MemoryHistoryStore) Save(_ context.Context, sessionID string, history []*schema.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := make([]*schema.Message, len(history))
	copy(cp, history)
	s.history[sessionID] = cp
	return nil
}

func (s *MemoryHistoryStore) Append(_ context.Context, sessionID string, msgs ...*schema.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.history[sessionID] = append(s.history[sessionID], msgs...)
	return nil
}

func (s *MemoryHistoryStore) Truncate(_ context.Context, sessionID string, keepLast int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	msgs := s.history[sessionID]
	if len(msgs) <= keepLast {
		return nil
	}
	s.history[sessionID] = msgs[len(msgs)-keepLast:]
	return nil
}
