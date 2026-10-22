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
// 默认回退为内存实现（单进程）；生产环境应在启动时经
// SetDefaultCheckPointStore 注入持久化实现（如 MySQLCheckPointStore），
// 以支持进程重启与多副本部署下的审批恢复。
var (
	defaultMu     sync.RWMutex
	defaultShared CheckPointStore
)

// SetDefaultCheckPointStore 注入进程级共享的检查点存储实现。
// 必须在首次构建图（NewAgent）之前调用；运行时重复调用以最后一次为准。
func SetDefaultCheckPointStore(s CheckPointStore) {
	defaultMu.Lock()
	defer defaultMu.Unlock()
	defaultShared = s
}

func DefaultCheckPointStore() CheckPointStore {
	defaultMu.RLock()
	s := defaultShared
	defaultMu.RUnlock()
	if s != nil {
		return s
	}
	defaultMu.Lock()
	defer defaultMu.Unlock()
	if defaultShared == nil {
		defaultShared = NewMemoryCheckPointStore()
	}
	return defaultShared
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
