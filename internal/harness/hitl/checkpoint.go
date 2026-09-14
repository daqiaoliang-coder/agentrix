package hitl

import (
	"context"
	"sync"

	"github.com/cloudwego/eino/compose"
)

type CheckPointStore = compose.CheckPointStore

type CheckPointDeleter interface {
	Delete(ctx context.Context, checkPointID string) error
}

type MemoryCheckPointStore struct {
	mu    sync.RWMutex
	store map[string][]byte
}

func NewMemoryCheckPointStore() *MemoryCheckPointStore {
	return &MemoryCheckPointStore{store: make(map[string][]byte)}
}

func (s *MemoryCheckPointStore) Get(_ context.Context, checkPointID string) ([]byte, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	data, ok := s.store[checkPointID]
	return data, ok, nil
}

func (s *MemoryCheckPointStore) Set(_ context.Context, checkPointID string, checkPoint []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := make([]byte, len(checkPoint))
	copy(cp, checkPoint)
	s.store[checkPointID] = cp
	return nil
}

func (s *MemoryCheckPointStore) Delete(_ context.Context, checkPointID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.store, checkPointID)
	return nil
}

func (s *MemoryCheckPointStore) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.store)
}
