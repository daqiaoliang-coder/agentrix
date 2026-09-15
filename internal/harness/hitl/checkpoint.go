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

// DefaultCheckPointStore 返回进程级共享的检查点存储。
//
// 为什么必须共享：Agent 是无状态执行器，上层（HTTP handler）每次请求都重新
// NewAgent → BuildAgentGraph。若每次建图都新建存储，审批中断时写入的
// Checkpoint 会在恢复请求到来前随旧图一起被丢弃，Resume 必然失败。
//
// 生产环境应替换为持久化实现（DB/Redis）以支持多副本部署；
// 内存版仅适用于单进程。
var (
	defaultStoreOnce sync.Once
	defaultStore     *MemoryCheckPointStore
)

func DefaultCheckPointStore() *MemoryCheckPointStore {
	defaultStoreOnce.Do(func() {
		defaultStore = NewMemoryCheckPointStore()
	})
	return defaultStore
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
