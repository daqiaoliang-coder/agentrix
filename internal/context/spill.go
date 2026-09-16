package context

import (
	"context"
	"fmt"
	"sync"
)

// SpillStore 承接被 offload 的工具结果原文，使压缩从「delete」变成「可恢复」。
//
// 设计要点（对齐 meego-ai 的 Minor GC 纪律「offload 可恢复，不是 delete」）：
// 压缩淘汰老工具结果时，原文并不丢弃，而是落到 SpillStore；上下文中只留一个
// 可操作的 stub（工具身份 + coverage 摘要 + 恢复路径）。模型看到 stub 后知道
// 「这里曾有个结果，我能取回来」，从而调用 read_result 按需回读，而不是幻觉续编。
//
// ref 由调用方（Engine）生成并写入 stub，回读工具凭 ref 取回原文。
// 生产环境可替换为对象存储/DB 实现；进程内实现仅用于单机与测试。
type SpillStore interface {
	// Spill 存入原文。同 ref 重复写入时后写覆盖先写（幂等，便于重试）。
	Spill(ctx context.Context, ref string, content string) error
	// Read 按 ref 取回原文；不存在时返回错误。
	Read(ctx context.Context, ref string) (string, error)
}

// SpillRef 由工具调用 ID 生成稳定 ref：同一条工具结果的 ref 恒定，
// 因此重复压缩不会产生多份副本，回读路径也始终有效。
func SpillRef(toolCallID string) string {
	return "toolresult:" + toolCallID
}

// MemorySpillStore 是 SpillStore 的进程内实现。
type MemorySpillStore struct {
	mu    sync.RWMutex
	blobs map[string]string
}

func NewMemorySpillStore() *MemorySpillStore {
	return &MemorySpillStore{blobs: make(map[string]string)}
}

func (s *MemorySpillStore) Spill(_ context.Context, ref string, content string) error {
	if ref == "" {
		return fmt.Errorf("spill: empty ref")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.blobs[ref] = content
	return nil
}

func (s *MemorySpillStore) Read(_ context.Context, ref string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	content, ok := s.blobs[ref]
	if !ok {
		return "", fmt.Errorf("spill: ref %s not found", ref)
	}
	return content, nil
}

// Len 返回已存放的条目数，用于诊断与测试。
func (s *MemorySpillStore) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.blobs)
}
