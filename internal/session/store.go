package session

import (
	"context"
	"sync"
	"time"

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

// Store 是会话持久化接口。
//
// RawHistory 语义为 append-only：只能通过 AppendHistory 追加，没有覆盖写
// （SaveHistory）与物理删除（Truncate）；历史范围读取统一走 LoadHistoryFrom
// 的游标语义，由调用方持有游标决定起点。
type Store interface {
	LoadState(ctx context.Context, sessionID string) (*State, error)
	SaveState(ctx context.Context, sessionID string, state *State) error
	// LoadHistory 全量加载（按 seq 升序），用于装配完整的压缩窗口
	LoadHistory(ctx context.Context, sessionID string) ([]*schema.Message, error)
	// AppendHistory 追加消息到 RawHistory，返回最后一条记录的 seq（作为游标）
	AppendHistory(ctx context.Context, sessionID string, msgs ...*schema.Message) (int64, error)
	// LoadHistoryFrom 游标范围读取：返回 seq > afterSeq 的记录，及其中最大的 seq。
	// 无新记录时返回空列表与 0，调用方据此不推进游标。
	LoadHistoryFrom(ctx context.Context, sessionID string, afterSeq int64) ([]*schema.Message, int64, error)
}

// MemoryStore 内存实现（生产环境可替换为 MySQL/Redis）。
// RawHistory 以 RawHistoryRecord 存放，写入时统一分配 id/seq/create_time。
type MemoryStore struct {
	mu      sync.RWMutex
	states  map[string]*State
	records map[string][]RawHistoryRecord
	nextID  int64 // 全局自增主键，跨会话单调
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		states:  make(map[string]*State),
		records: make(map[string][]RawHistoryRecord),
	}
}

func (s *MemoryStore) LoadState(_ context.Context, sessionID string) (*State, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if st, ok := s.states[sessionID]; ok {
		return st, nil
	}
	return NewState(sessionID), nil
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
	return recordsToMessages(s.records[sessionID]), nil
}

func (s *MemoryStore) AppendHistory(_ context.Context, sessionID string, msgs ...*schema.Message) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	existing := s.records[sessionID]
	newRecords := messagesToRecords(sessionID, msgs, int64(len(existing)), now)
	for i := range newRecords {
		s.nextID++
		newRecords[i].ID = s.nextID
	}
	s.records[sessionID] = append(existing, newRecords...)

	all := s.records[sessionID]
	if len(all) == 0 {
		return 0, nil
	}
	return all[len(all)-1].Seq, nil
}

func (s *MemoryStore) LoadHistoryFrom(_ context.Context, sessionID string, afterSeq int64) ([]*schema.Message, int64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	records := s.records[sessionID]
	var from int
	for from < len(records) && records[from].Seq <= afterSeq {
		from++
	}
	if from == len(records) {
		return nil, 0, nil
	}
	return recordsToMessages(records[from:]), records[len(records)-1].Seq, nil
}
