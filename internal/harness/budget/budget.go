package budget

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// TokenUsage 记录一次模型调用的 token 消耗
type TokenUsage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
}

// ErrBudgetExhausted 是预算耗尽的哨兵错误，调用方可据此区分终止原因。
var ErrBudgetExhausted = errors.New("budget exhausted")

// Budget 跟踪单个 Turn 的 token 和迭代预算
type Budget struct {
	mu sync.Mutex

	MaxTokens     int // 单 Turn token 上限
	MaxIterations int // 单 Turn 迭代上限

	UsedTokens     int
	UsedIterations int

	StartTime time.Time
	Deadline  time.Time // 超时预算，零值表示不限制

	// progressSig 记录最近若干轮工具调用签名，供 IsStagnant 无进展检测使用
	progressSig []string
}

// NewBudget 创建预算控制器
func NewBudget(maxTokens, maxIterations int) *Budget {
	return &Budget{
		MaxTokens:     maxTokens,
		MaxIterations: maxIterations,
		StartTime:     time.Now(),
	}
}

// WithDeadline 设置超时预算
func (b *Budget) WithDeadline(d time.Duration) *Budget {
	b.Deadline = b.StartTime.Add(d)
	return b
}

// ConsumeTokens 消耗 token，返回是否超出预算
func (b *Budget) ConsumeTokens(usage TokenUsage) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.UsedTokens += usage.TotalTokens
	if b.MaxTokens > 0 && b.UsedTokens > b.MaxTokens {
		return fmt.Errorf(
			"token budget exceeded: used %d, limit %d",
			b.UsedTokens, b.MaxTokens,
		)
	}
	return nil
}

// NextIteration 推进一次迭代，返回是否超出预算
func (b *Budget) NextIteration() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.UsedIterations++
	if b.MaxIterations > 0 && b.UsedIterations > b.MaxIterations {
		return fmt.Errorf(
			"iteration budget exceeded: used %d, limit %d",
			b.UsedIterations, b.MaxIterations,
		)
	}
	return nil
}

// CheckDeadline 检查是否超时
func (b *Budget) CheckDeadline() error {
	if b.Deadline.IsZero() {
		return nil
	}
	if time.Now().After(b.Deadline) {
		return fmt.Errorf("budget deadline exceeded at %s", b.Deadline)
	}
	return nil
}

// IsExhausted 判断 token 或迭代预算是否已耗尽（任一超限即 true）。
// 这是 Generate 前的快速短路检查，避免在预算已尽时仍发起模型调用。
func (b *Budget) IsExhausted() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.MaxTokens > 0 && b.UsedTokens > b.MaxTokens {
		return true
	}
	if b.MaxIterations > 0 && b.UsedIterations > b.MaxIterations {
		return true
	}
	return false
}

// progressSignatures 维护最近若干轮工具调用签名，用于无进展检测。
// 长度上限通过 progressWindow 控制。
const progressWindow = 8

// RecordProgress 记录本轮的工具调用签名（例如 ToolCalls 名称与参数的哈希）。
// 签名相同表示模型在重复同样的工具调用，触发 IsStagnant 后可注入收尾提示。
func (b *Budget) RecordProgress(signature string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.progressSig = append(b.progressSig, signature)
	if len(b.progressSig) > progressWindow {
		b.progressSig = b.progressSig[len(b.progressSig)-progressWindow:]
	}
}

// IsStagnant 判断是否连续 limit 轮无进展（签名完全相同）。
// limit <= 0 表示禁用无进展检测。
func (b *Budget) IsStagnant(limit int) bool {
	if limit <= 0 {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.progressSig) < limit {
		return false
	}
	tail := b.progressSig[len(b.progressSig)-limit:]
	first := tail[0]
	for _, s := range tail[1:] {
		if s != first {
			return false
		}
	}
	return true
}

// Remaining 返回剩余 token 预算
func (b *Budget) Remaining() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.MaxTokens <= 0 {
		return -1 // 无限制
	}
	rem := b.MaxTokens - b.UsedTokens
	if rem < 0 {
		return 0
	}
	return rem
}

// Snapshot 返回当前预算状态快照
func (b *Budget) Snapshot() BudgetSnapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	return BudgetSnapshot{
		MaxTokens:      b.MaxTokens,
		MaxIterations:  b.MaxIterations,
		UsedTokens:     b.UsedTokens,
		UsedIterations: b.UsedIterations,
		Elapsed:        time.Since(b.StartTime),
	}
}

// BudgetSnapshot 是预算的只读快照，用于 Signal 投影
type BudgetSnapshot struct {
	MaxTokens      int           `json:"max_tokens"`
	MaxIterations  int           `json:"max_iterations"`
	UsedTokens     int           `json:"used_tokens"`
	UsedIterations int           `json:"used_iterations"`
	Elapsed        time.Duration `json:"elapsed"`
}

// ---------------------------------------------------------------------------
// 上下文注入：把 Budget 挂到 context 上，节点内可通过 FromContext 获取
// ---------------------------------------------------------------------------

type budgetKey struct{}

// WithBudget 把 Budget 注入 context
func WithBudget(ctx context.Context, b *Budget) context.Context {
	return context.WithValue(ctx, budgetKey{}, b)
}

// FromContext 从 context 获取 Budget
func FromContext(ctx context.Context) (*Budget, bool) {
	b, ok := ctx.Value(budgetKey{}).(*Budget)
	return b, ok
}

// MustFromContext 从 context 获取 Budget，不存在时 panic
func MustFromContext(ctx context.Context) *Budget {
	b, ok := FromContext(ctx)
	if !ok {
		panic("budget not found in context; use budget.WithBudget() first")
	}
	return b
}
