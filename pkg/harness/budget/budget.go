package budget

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// TokenUsage 记录一次模型调用的 token 消耗。
//
// CachedTokens 是 PromptTokens 中命中 provider 侧 KV 缓存的部分，来自
// schema.TokenUsage.PromptTokenDetails.CachedTokens。计费上这部分通常按
// 折扣价结算，性能上也直接对应首 token 延迟。
//
// 为什么这个字段对压缩算法的评估是必需的：need-driven + 前缀稳定 + 一次清到
// 低水位这套设计，收益的主要形态不是「少发 token」而是「少发全价 token」。
// 没有缓存命中数，就无法区分两种完全相反的压缩策略——
// 一种每轮改写历史、前缀全废、省下的 token 被缓存重建成本吃掉；
// 另一种只在越阈时压一次、随后多轮命中缓存。两者在「省了多少 token」上
// 可能看不出差别，差别全在 CachedTokens 上。
type TokenUsage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	// CachedTokens 是 PromptTokens 中命中缓存的部分，是 PromptTokens 的子集，
	// 不参与 TotalTokens 的构成，因此不影响预算扣减口径。
	// provider 未返回该明细时为 0，此时缓存命中率指标不可用，应按缺失处理
	// 而非当作「命中率为零」。
	CachedTokens int
}

// CachedRatio 返回缓存命中率（CachedTokens / PromptTokens），范围 [0,1]。
// PromptTokens 为 0 时返回 0，避免除零。
func (u TokenUsage) CachedRatio() float64 {
	if u.PromptTokens <= 0 {
		return 0
	}
	r := float64(u.CachedTokens) / float64(u.PromptTokens)
	if r > 1 {
		return 1
	}
	return r
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

	// ---- 观测累计量（不参与预算判定，只用于效果评估）----

	// UsedPromptTokens 累计输入 token，UsedCachedTokens 累计其中命中缓存的部分。
	// 与 UsedTokens 分开累计，是因为 UsedTokens 混入了输出 token，
	// 用它算命中率会把分母放大、稀释真实的前缀复用程度。
	UsedPromptTokens int
	UsedCachedTokens int

	// ModelCalls 是产生过真实用量明细的模型调用次数。
	// 它决定 CachedRatio 是否可信：为 0 说明 provider 从未返回用量明细，
	// 此时命中率应当作「未知」而非「零」。把这两种情况混为一谈，
	// 会让人误以为压缩策略破坏了缓存前缀，实际只是没采到数据。
	ModelCalls int

	// CompressEvents / CompressTokensSaved / CompressNetSaved /
	// CompressSummaryTokens / CompressUnrecoverableLost 是压缩事件的累计账本，
	// 由 RecordCompaction 写入。NetSaved 已扣除摘要 LLM 自身的开销。
	CompressEvents            int
	CompressTokensSaved       int
	CompressNetSaved          int
	CompressSummaryTokens     int
	CompressUnrecoverableLost int
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

// ConsumeTokens 消耗 token，返回是否超出预算。
//
// 同时累计 prompt / cached 明细用于缓存命中率评估。累计与预算判定分开处理：
// CachedTokens 是 PromptTokens 的子集，绝不能计入 UsedTokens，否则等于把同一批
// token 扣两次预算，会导致明明没超窗就提前终止。
func (b *Budget) ConsumeTokens(usage TokenUsage) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.UsedTokens += usage.TotalTokens

	// 只在 provider 真的返回了用量明细时才计入调用次数，
	// 使 ModelCalls 语义严格等于「有多少次采到了缓存数据」。
	if usage.PromptTokens > 0 || usage.TotalTokens > 0 {
		b.ModelCalls++
		b.UsedPromptTokens += usage.PromptTokens
		b.UsedCachedTokens += usage.CachedTokens
	}

	if b.MaxTokens > 0 && b.UsedTokens > b.MaxTokens {
		return fmt.Errorf(
			"token budget exceeded: used %d, limit %d",
			b.UsedTokens, b.MaxTokens,
		)
	}
	return nil
}

// RecordCompaction 累计一次压缩事件的账本。
//
// 用基础类型而非 CompressStats 作参数，是为了不让 budget 反向依赖 context 包：
// 预算属于「资源约束」层，压缩属于「上下文装配」层，两层各自独立才能被单独复用。
// 由 harness/core 在拿到 context.CompressStats 后做一次转换。
//
// saved 是毛节省，netSaved 是扣除摘要 LLM 开销后的净节省，summaryTokens 是产出的
// 摘要占用量，unrecoverable 是不可恢复丢失条数。只在真正改写了序列时调用——
// 未越阈值的轮次不应计入 CompressEvents，否则事件数会等于模型调用数，
// 失去「压缩触发频率」这个指标的意义。
func (b *Budget) RecordCompaction(saved, netSaved, summaryTokens, unrecoverable int) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.CompressEvents++
	b.CompressTokensSaved += saved
	b.CompressNetSaved += netSaved
	b.CompressSummaryTokens += summaryTokens
	b.CompressUnrecoverableLost += unrecoverable
}

// CachedRatio 返回本 Turn 累计的缓存命中率（累计 cached / 累计 prompt）。
//
// 用累计值而非单次平均，是因为各轮 prompt 长度差异极大，等权平均会让一轮
// 短请求的命中率与一轮长请求同权重，掩盖真实的前缀复用情况。
//
// 钳制到 [0,1]：provider 偶发返回 cached > prompt 的异常数据时，
// 不钳制会让看板出现 >100% 的命中率。TokenUsage.CachedRatio 也做同样的钳制，
// 两处口径必须一致——否则同一指标在单次与累计层级给出不同的值，无法对照排查。
func (b *Budget) CachedRatio() float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cachedRatioLocked()
}

// cachedRatioLocked 是 CachedRatio 的无锁版本，调用方必须已持有 b.mu。
//
// 单独拆出来是因为 Snapshot() 本身已持锁：sync.Mutex 不可重入，
// 在持锁状态下调用 CachedRatio() 会自我死锁、整个 Turn 挂死。
// 这类故障不报错也不 panic，只在压测或线上高并发时才显现，排查成本极高。
func (b *Budget) cachedRatioLocked() float64 {
	if b.UsedPromptTokens <= 0 {
		return 0
	}
	r := float64(b.UsedCachedTokens) / float64(b.UsedPromptTokens)
	if r > 1 {
		return 1
	}
	return r
}

// HasUsageData 报告是否采到过 provider 的真实用量明细。
// 为 false 时 CachedRatio 返回的 0 表示「无数据」，不能解读为「命中率为零」——
// 这两种情况的处置完全不同：前者是埋点没接通，后者才是压缩策略真的破坏了前缀。
func (b *Budget) HasUsageData() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.ModelCalls > 0 && b.UsedPromptTokens > 0
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

	cachedRatio := b.cachedRatioLocked()

	return BudgetSnapshot{
		MaxTokens:      b.MaxTokens,
		MaxIterations:  b.MaxIterations,
		UsedTokens:     b.UsedTokens,
		UsedIterations: b.UsedIterations,
		Elapsed:        time.Since(b.StartTime),

		UsedPromptTokens: b.UsedPromptTokens,
		UsedCachedTokens: b.UsedCachedTokens,
		ModelCalls:       b.ModelCalls,
		CachedRatio:      cachedRatio,
		HasUsageData:     b.ModelCalls > 0 && b.UsedPromptTokens > 0,

		CompressEvents:            b.CompressEvents,
		CompressTokensSaved:       b.CompressTokensSaved,
		CompressNetSaved:          b.CompressNetSaved,
		CompressSummaryTokens:     b.CompressSummaryTokens,
		CompressUnrecoverableLost: b.CompressUnrecoverableLost,
	}
}

// BudgetSnapshot 是预算的只读快照，用于 Signal 投影
type BudgetSnapshot struct {
	MaxTokens      int           `json:"max_tokens"`
	MaxIterations  int           `json:"max_iterations"`
	UsedTokens     int           `json:"used_tokens"`
	UsedIterations int           `json:"used_iterations"`
	Elapsed        time.Duration `json:"elapsed"`

	// ---- 缓存命中观测 ----

	UsedPromptTokens int     `json:"used_prompt_tokens"`
	UsedCachedTokens int     `json:"used_cached_tokens"`
	ModelCalls       int     `json:"model_calls"`
	CachedRatio      float64 `json:"cached_ratio"`
	// HasUsageData 区分「命中率为零」与「根本没采到 provider 用量」。
	// 为 false 时 CachedRatio 的 0 不可解读为压缩策略破坏了前缀缓存。
	HasUsageData bool `json:"has_usage_data"`

	// ---- 压缩事件观测 ----

	CompressEvents            int `json:"compress_events"`
	CompressTokensSaved       int `json:"compress_tokens_saved"`       // 毛节省
	CompressNetSaved          int `json:"compress_net_saved"`          // 扣除摘要开销后的净节省
	CompressSummaryTokens     int `json:"compress_summary_tokens"`     // 产出摘要占用量
	CompressUnrecoverableLost int `json:"compress_unrecoverable_lost"` // 不可恢复丢失条数
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
