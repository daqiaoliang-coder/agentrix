package hitl

import (
	"context"
	"errors"
	"sync"
	"time"
)

// 审批审计记录的状态。
const (
	// ApprovalPending 表示审批已发起、尚未决策。
	ApprovalPending = "pending"
	// ApprovalApproved 表示审批通过（写操作随后执行）。
	ApprovalApproved = "approved"
	// ApprovalRejected 表示审批被拒绝（写操作未执行）。
	ApprovalRejected = "rejected"
	// ApprovalAbandoned 表示用户发起新 Turn，挂起审批被放弃。
	ApprovalAbandoned = "abandoned"
)

// ApprovalAudit 是一次人工审批的完整审计事实：谁在什么会话里、对哪次工具调用、
// 基于什么参数、做了什么决策。
//
// 检查点只承载图状态且成功后删除，无法回答「谁批准了哪次写操作」，故须独立落库。
type ApprovalAudit struct {
	InterruptID string
	Scene       string
	SessionID   string
	TurnID      string
	ToolName    string
	ToolCallID  string
	Arguments   string
	Reason      string
	Status      string
	Operator    string
	Comment     string
	RequestedAt time.Time
	DecidedAt   time.Time
}

// ApprovalAuditStore 持久化审批审计。写入语义：
//   - RecordRequest 在审批中断时落一条 pending；
//   - RecordDecision 在 Resume 认领时写入决策人与结果；
//   - MarkAbandoned 在新 Turn 放弃挂起审批时标记为 abandoned。
//
// 同一 InterruptID 只能对应一条记录，重复写入按覆盖处理（幂等）。
type ApprovalAuditStore interface {
	RecordRequest(ctx context.Context, a *ApprovalAudit) error
	RecordDecision(ctx context.Context, interruptID, operator, comment string, approved bool) error
	MarkAbandoned(ctx context.Context, interruptID string) error
}

// ErrApprovalAuditMissingInterruptID 表示审计记录缺少中断标识。
var ErrApprovalAuditMissingInterruptID = errors.New("approval audit: interrupt id is required")

// MemoryApprovalAuditStore 是进程内实现，仅用于本地开发与测试。
type MemoryApprovalAuditStore struct {
	mu      sync.RWMutex
	records map[string]ApprovalAudit
}

func NewMemoryApprovalAuditStore() *MemoryApprovalAuditStore {
	return &MemoryApprovalAuditStore{records: make(map[string]ApprovalAudit)}
}

func (s *MemoryApprovalAuditStore) RecordRequest(_ context.Context, a *ApprovalAudit) error {
	if a == nil || a.InterruptID == "" {
		return ErrApprovalAuditMissingInterruptID
	}
	rec := *a
	if rec.RequestedAt.IsZero() {
		rec.RequestedAt = time.Now()
	}
	if rec.Status == "" {
		rec.Status = ApprovalPending
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records[rec.InterruptID] = rec
	return nil
}

func (s *MemoryApprovalAuditStore) RecordDecision(
	_ context.Context,
	interruptID, operator, comment string,
	approved bool,
) error {
	if interruptID == "" {
		return ErrApprovalAuditMissingInterruptID
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.records[interruptID]
	if !ok {
		rec = ApprovalAudit{InterruptID: interruptID, RequestedAt: time.Now()}
	}
	rec.Status = ApprovalRejected
	if approved {
		rec.Status = ApprovalApproved
	}
	rec.Operator = operator
	rec.Comment = comment
	rec.DecidedAt = time.Now()
	s.records[interruptID] = rec
	return nil
}

func (s *MemoryApprovalAuditStore) MarkAbandoned(_ context.Context, interruptID string) error {
	if interruptID == "" {
		return ErrApprovalAuditMissingInterruptID
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.records[interruptID]
	if !ok || rec.Status != ApprovalPending {
		return nil
	}
	rec.Status = ApprovalAbandoned
	rec.DecidedAt = time.Now()
	s.records[interruptID] = rec
	return nil
}

// Get 返回某次审批的审计快照；供测试与运维排查使用。
func (s *MemoryApprovalAuditStore) Get(interruptID string) (ApprovalAudit, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, ok := s.records[interruptID]
	return rec, ok
}

var (
	defaultAuditMu     sync.RWMutex
	defaultAuditShared ApprovalAuditStore
)

// SetDefaultApprovalAuditStore 注入进程级审计存储。
// 生产环境须在首次执行 Turn 前注入持久化实现（如 MySQLApprovalAuditStore）。
func SetDefaultApprovalAuditStore(s ApprovalAuditStore) {
	defaultAuditMu.Lock()
	defer defaultAuditMu.Unlock()
	defaultAuditShared = s
}

// DefaultApprovalAuditStore 返回进程级审计存储；未注入时回退为内存实现。
func DefaultApprovalAuditStore() ApprovalAuditStore {
	defaultAuditMu.RLock()
	s := defaultAuditShared
	defaultAuditMu.RUnlock()
	if s != nil {
		return s
	}
	defaultAuditMu.Lock()
	defer defaultAuditMu.Unlock()
	if defaultAuditShared == nil {
		defaultAuditShared = NewMemoryApprovalAuditStore()
	}
	return defaultAuditShared
}
