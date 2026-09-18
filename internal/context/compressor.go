package context

import (
	"context"

	"github.com/cloudwego/eino/schema"

	"github.com/daqiaoliang-coder/agentrix/internal/session"
)

// Compressor 是 Engine.compress 的一层薄封装，供「不持有 session.State、
// 但想显式传入上一轮摘要并拿回本轮摘要」的调用方使用。
//
// 它不再自行实现四阶段压缩。此前这里有一份与 Engine.compress 平行的逻辑，
// 两处一旦分叉，就会出现「测试验证的是 A 路径、线上跑的是 B 路径」——
// 而且这份平行实现没有 shouldCompress 判断，是无条件压缩，正好绕过
// need-driven 纪律：每轮都改写历史、每轮都作废缓存前缀，省下的 token
// 不够付缓存重建的成本。现在统一委托，纪律只在一处实现。
type Compressor struct {
	Engine *Engine
}

func NewCompressor(engine *Engine) *Compressor {
	return &Compressor{Engine: engine}
}

// Compress 在越过软阈值时对消息序列做压缩，返回压缩结果与本轮摘要。
//
// 与 Engine.Assemble 的差异仅在摘要的传递方式：Assemble 从 state.Memory.Summary
// 读旧摘要并写回新摘要，这里改为由调用方显式传入 prevSummary、显式接收 summary。
// 压缩逻辑、触发纪律、观测账本全部走同一条 Engine.compress 路径。
//
// 未越过 softLimit 时返回原序列且 summary 回传 prevSummary（未产生新摘要），
// 保持「不该压就不压」的 need-driven 语义。
func (c *Compressor) Compress(
	ctx context.Context,
	messages []*schema.Message,
	prevSummary string,
) (result []*schema.Message, summary string, err error) {

	result, summary, _, err = c.CompressWithStats(ctx, messages, prevSummary)
	return result, summary, err
}

// CompressWithStats 是 Compress 的统计版本，额外返回本次压缩的账本。
//
// 与 Engine 的两个入口同理：账本走返回值而非仅靠 OnCompacted 钩子，
// 因为 Engine 跨 Turn 共享、Budget 按 Turn 创建，靠钩子转发会串号。
func (c *Compressor) CompressWithStats(
	ctx context.Context,
	messages []*schema.Message,
	prevSummary string,
) (result []*schema.Message, summary string, stats CompressStats, err error) {

	if c.Engine == nil {
		return messages, prevSummary, stats, nil
	}

	// need-driven：未越软阈值直接原样返回，不改写任何一条消息。
	if !c.Engine.shouldCompress(messages) {
		before := c.Engine.effectiveTokens(messages)
		return messages, prevSummary, CompressStats{
			Path: "compressor", TokensBefore: before, TokensAfter: before,
		}, nil
	}

	// 借一个临时 State 承接摘要的读写，从而复用 Engine.compress 的完整四阶段流程。
	// 只搬运 Memory.Summary 一个字段，调用方其余状态不受影响。
	tmp := &session.State{}
	tmp.Memory.Summary = prevSummary

	result, stats, err = c.Engine.compress(ctx, messages, tmp)
	if err != nil {
		return nil, prevSummary, stats, err
	}
	return result, tmp.Memory.Summary, stats, nil
}
