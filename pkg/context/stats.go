package context

import (
	"github.com/cloudwego/eino/schema"
)

// CompressStats 是一次压缩事件的完整账本。
//
// 它存在的理由是「压缩的收益容易测、代价难测」：省下的 token 一眼可见，而丢信息
// 导致的失败率上升、摘要 LLM 自身的开销、缓存前缀被作废的长度，都发生在压缩的
// 那一瞬间且不留痕迹。把这些量一次性记下来，才可能区分「提升来自算法」与
// 「提升来自多花了算力」。
//
// 净收益的正确算法是：
//
//	TokensBefore − TokensAfter − SummaryModelTokens
//
// 最后一项是摘要模型这次调用自己烧掉的 token，属于压缩的成本而非收益；
// 漏减会系统性地高估压缩效果。UnrecoverableLost 则直接对应下游任务失败风险，
// 是代价侧唯一无法用 token 衡量的量，必须单独看。
type CompressStats struct {
	// Triggered 标记本次是否真的改写了消息序列。
	// 未越 softLimit 时压缩直接返回原序列，此时 Triggered=false——
	// 这正是 need-driven 纪律生效的证据，也是缓存前缀保持稳定的轮次。
	Triggered bool `json:"triggered"`

	// Path 记录压缩由哪条入口触发，便于区分两条语义不同的路径：
	// "inplace" 是模型调用前的规则裁剪（不调 LLM 摘要），
	// "assemble" 是 Agent 级装配压缩（含 LLM 结构化摘要）。
	Path string `json:"path"`

	TokensBefore int `json:"tokens_before"` // 压缩前 effectiveTokens（消息 + 固定开销）
	TokensAfter  int `json:"tokens_after"`  // 压缩后 effectiveTokens

	// Sampled 是被头尾采样的巨型工具结果条数（突破 keepRecent 保护区的那条路径）。
	Sampled int `json:"sampled"`
	// Evicted 是被整条淘汰并替换为 stub 的工具结果条数。
	Evicted int `json:"evicted"`
	// Offloaded 是原文成功写入 SpillStore、因而可凭 tool_call_id 回读的条数。
	// Offloaded < Sampled+Evicted 的差额就是不可恢复的部分。
	Offloaded int `json:"offloaded"`

	// UnrecoverableLost 是彻底丢失、无任何回读路径的消息条数。
	// 来源有两处：未配置 SpillStore 时的淘汰/采样，以及 fixToolCallPairs 为
	// 悬空 tool_call 补的占位符。这是代价侧最该盯的指标——它不参与 token 计算，
	// 却直接决定模型是否会因缺少信息而失败或幻觉。
	UnrecoverableLost int `json:"unrecoverable_lost"`

	// Placeholders 是 fixToolCallPairs 为补齐协议配对而插入的占位符条数。
	// 计入 UnrecoverableLost，单列出来是为了区分「主动淘汰」与「被动补洞」。
	Placeholders int `json:"placeholders"`

	SummaryTokens      int  `json:"summary_tokens"`       // 产出的摘要占用的 token
	SummaryModelTokens int  `json:"summary_model_tokens"` // 摘要 LLM 调用自身的开销（成本，非收益）
	SummaryViaLLM      bool `json:"summary_via_llm"`      // true=LLM 结构化摘要；false=规则降级摘要

	// StablePrefixTokens 是压缩前后逐条比对得出的、保持字节级相同的头部前缀 token 数。
	// 这是拿到 provider 真实 CachedTokens 之前唯一可用的缓存代理指标：
	// 前缀越长，provider 侧 KV 缓存可复用的部分越多。
	// 每轮都改写历史会让这个值趋近 0——省下的 token 远不够付缓存重建成本。
	StablePrefixTokens int `json:"stable_prefix_tokens"`
}

// TokensSaved 返回本次压缩的毛节省量（未扣除摘要开销）。
func (s CompressStats) TokensSaved() int {
	d := s.TokensBefore - s.TokensAfter
	if d < 0 {
		return 0
	}
	return d
}

// NetTokensSaved 返回净节省量：毛节省减去摘要 LLM 自身的开销。
// 这是压缩是否真正划算的判据；只报 TokensSaved 会系统性高估收益。
func (s CompressStats) NetTokensSaved() int {
	net := s.TokensSaved() - s.SummaryModelTokens
	if net < 0 {
		return 0
	}
	return net
}

// CompressionRatio 返回压缩率（节省量占压缩前的比例），范围 [0,1]。
// TokensBefore 为 0 时返回 0，避免除零。
func (s CompressStats) CompressionRatio() float64 {
	if s.TokensBefore <= 0 {
		return 0
	}
	return float64(s.TokensSaved()) / float64(s.TokensBefore)
}

// notifyCompacted 在压缩结束后把账本交给观测钩子。钩子为 nil 时静默返回。
//
// 用 defer 在调用侧触发，保证即使压缩中途走了 early return（middle 为空、
// 采样后已达标）也一定会记账——漏记的恰恰是「压缩没起作用」的那些轮次，
// 而那部分数据对判断阈值是否合理最关键。
func (e *Engine) notifyCompacted(stats CompressStats) {
	if e.OnCompacted == nil {
		return
	}
	e.OnCompacted(stats)
}

// stablePrefixTokens 计算 before 与 after 从头开始逐条相同的消息所覆盖的 token 数。
//
// 只比 role / content / toolCallID 三个决定序列化字节的关键字段：这三者一致即
// provider 侧看到的前缀字节一致，KV 缓存可复用。其余字段（Extra、Name）不影响
// 缓存命中，纳入比对会把本可复用的前缀误判为已失配。
func stablePrefixTokens(before, after []*schema.Message) int {
	n := len(before)
	if len(after) < n {
		n = len(after)
	}
	stable := 0
	for i := 0; i < n; i++ {
		b, a := before[i], after[i]
		if b == nil || a == nil {
			if b != a {
				break
			}
			continue
		}
		if b.Role != a.Role || b.Content != a.Content || b.ToolCallID != a.ToolCallID {
			break
		}
		stable++
	}
	return EstimateMessagesTokens(before[:stable])
}
