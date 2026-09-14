package session

import (
	"context"
	"sync"

	"github.com/cloudwego/eino/schema"
)

type Store interface {
	LoadState(ctx context.Context, sessionID string) (*State, error)
	SaveState(ctx context.Context, sessionID string, state *State) error
	LoadHistory(ctx context.Context, sessionID string) ([]*schema.Message, error)
	SaveHistory(ctx context.Context, sessionID string, history []*schema.Message) error
}

type MemoryStore struct {
	mu      sync.RWMutex
	states  map[string]*State
	history map[string][]*schema.Message
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		states:  make(map[string]*State),
		history: make(map[string][]*schema.Message),
	}
}

func (s *MemoryStore) LoadState(_ context.Context, sessionID string) (*State, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if st, ok := s.states[sessionID]; ok {
		return st, nil
	}
	return &State{SkillRuntime: make(map[string]any)}, nil
}

func (s *MemoryStore) SaveState(_ context.Context, sessionID string, state *State) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.states[sessionID] = state
	return nil
}

func (s *MemoryStore) LoadHistory(_ context.Context, sessionID string) ([]*schema.Message, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.history[sessionID], nil
}

func (s *MemoryStore) SaveHistory(_ context.Context, sessionID string, history []*schema.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.history[sessionID] = history
	return nil
}
