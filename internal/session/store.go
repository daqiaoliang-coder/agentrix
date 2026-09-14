package session

import (
	"context"
	"sync"

	"github.com/cloudwego/eino/schema"
)

// 为什么分离：
//
// SessionStateV1 是轻量的、结构化的、可被模型高效消费的“工作记忆”——它是“模型需要记住什么”。
//
// RawHistory 是完整的、不可篡改的审计记录——它是“实际发生了什么”。
//
// 两者通过游标关联，既保证模型上下文精简，又保留完整可追溯性。
//
// 关键约束：SessionStateV1 不等于业务执行账本，也不等于任意失败恢复点。它是 Agent 的“工作记忆”，业务状态应由业务方通过 Tool 维护。

type Store interface {
	LoadState(ctx context.Context, sessionID string) (*State, error)
	SaveState(ctx context.Context, sessionID string, state *State) error
	LoadHistory(ctx context.Context, sessionID string) ([]*schema.Message, error)
	SaveHistory(ctx context.Context, sessionID string, history []*schema.Message) error
}

// MemoryStore 内存实现（生产环境可替换为 MySQL/Redis）
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
