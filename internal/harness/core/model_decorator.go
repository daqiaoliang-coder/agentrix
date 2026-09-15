package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	ctxengine "github.com/daqiaoliang-coder/agentrix/internal/context"
	"github.com/daqiaoliang-coder/agentrix/internal/harness/budget"
)

// BudgetModelConfig 装饰器配置。零值表示对应能力关闭。
type BudgetModelConfig struct {
	// CallTimeout 单次模型调用超时（双层超时的内层）。零值表示不限制。
	CallTimeout time.Duration
	// MaxRetries 可重试错误的最大重试次数（不含首次调用）。零值表示不重试。
	MaxRetries int
	// Backoff 重试退避基数，每次重试等待 Backoff * 2^(attempt-1)。
	Backoff time.Duration
	// NoProgressLimit 无进展检测阈值：连续 N 轮工具调用签名相同则注入收尾提示。零值表示禁用。
	NoProgressLimit int
	// FallbackModel 降级模型，主模型重试耗尽后切到该模型再试一次。可为 nil。
	FallbackModel model.ToolCallingChatModel
	// Engine 上下文压缩引擎，用于在每次模型调用前做规则裁剪。可为 nil（禁用循环内压缩）。
	Engine *ctxengine.Engine
}

// BudgetModel 是 model.ToolCallingChatModel 的装饰器，在图架构下承担
// 伪代码中 CallWithFallback 的职责：预算检查、双层超时、错误重试与降级、
// 用量记录、无进展检测、上下文循环内压缩。
//
// 装饰器自身不持有 *budget.Budget（避免跨 Turn 状态泄漏），预算按 Turn
// 在 Agent.Run 中创建并经 budget.WithBudget 注入 context，这里通过
// budget.FromContext 读取。
type BudgetModel struct {
	raw model.ToolCallingChatModel
	cfg BudgetModelConfig
}

// NewBudgetModel 用给定配置包装原始模型。
func NewBudgetModel(raw model.ToolCallingChatModel, cfg BudgetModelConfig) *BudgetModel {
	return &BudgetModel{raw: raw, cfg: cfg}
}

// Generate 实现 model.BaseChatModel.Generate。
func (m *BudgetModel) Generate(
	ctx context.Context,
	input []*schema.Message,
	opts ...model.Option,
) (*schema.Message, error) {

	// ① 预算短路检查
	b, hasBudget := budget.FromContext(ctx)
	if hasBudget {
		if b.IsExhausted() {
			return nil, fmt.Errorf("budget exhausted before model call: %w", budget.ErrBudgetExhausted)
		}
		if err := b.CheckDeadline(); err != nil {
			return nil, err
		}
	}

	// ② 无进展检测：注入收尾提示（注入输入而非输出，让模型还能再走一轮）
	if hasBudget && m.cfg.NoProgressLimit > 0 && b.IsStagnant(m.cfg.NoProgressLimit) {
		nudge := schema.SystemMessage(
			fmt.Sprintf(
				"你已连续 %d 轮没有产生新的工具调用或新信息，请基于现有信息总结并给出最终回答。",
				m.cfg.NoProgressLimit,
			),
		)
		input = append(append([]*schema.Message(nil), input...), nudge)
	}

	// ③ 上下文循环内压缩（规则裁剪，不调 LLM）
	if m.cfg.Engine != nil {
		input = m.cfg.Engine.CompressInPlace(input)
	}

	// ④ 带重试的模型调用
	out, err := m.generateWithRetry(ctx, input, opts...)
	if err != nil {
		return nil, err
	}

	// ⑤ 用量记录
	if hasBudget && out != nil {
		m.recordUsage(b, input, out)
	}

	// ⑥ 无进展签名记录
	if hasBudget && out != nil {
		b.RecordProgress(toolCallSignature(out))
	}

	return out, nil
}

// generateWithRetry 执行带退避重试和可选降级的模型调用。
func (m *BudgetModel) generateWithRetry(
	ctx context.Context,
	input []*schema.Message,
	opts ...model.Option,
) (*schema.Message, error) {

	var lastErr error
	for attempt := 0; attempt <= m.cfg.MaxRetries; attempt++ {
		if attempt > 0 {
			if err := sleepBackoff(ctx, m.cfg.Backoff, attempt); err != nil {
				return nil, err
			}
		}

		out, err := m.callWithTimeout(ctx, m.raw, input, opts...)
		if err == nil {
			return out, nil
		}
		lastErr = err

		// 非可重试错误：不重试
		if !isRetryable(err) {
			break
		}
	}

	// 主模型重试耗尽，尝试降级模型
	if m.cfg.FallbackModel != nil {
		if out, err := m.callWithTimeout(ctx, m.cfg.FallbackModel, input, opts...); err == nil {
			return out, nil
		}
	}

	// 重试/降级均失败：返回合成 assistant 消息（无 ToolCalls，图分支走 END 收尾）
	return syntheticErrorMessage(lastErr), nil
}

// callWithTimeout 对单次模型调用施加内层超时（双层超时的内层）。
func (m *BudgetModel) callWithTimeout(
	ctx context.Context,
	delegate model.ToolCallingChatModel,
	input []*schema.Message,
	opts ...model.Option,
) (*schema.Message, error) {
	if m.cfg.CallTimeout <= 0 {
		return delegate.Generate(ctx, input, opts...)
	}
	callCtx, cancel := context.WithTimeout(ctx, m.cfg.CallTimeout)
	defer cancel()
	return delegate.Generate(callCtx, input, opts...)
}

// Stream 实现 model.BaseChatModel.Stream。
//
// 简化策略：仅做输入压缩与直通委托，不对流式调用施加单次超时，也不重试。
// 原因：Stream 返回 StreamReader 后才被消费，若在此 defer cancel 会立即切断 reader；
// 而 leak cancel 又有资源风险。流式超时改由 Agent.Run 的外层 TotalTimeout 经 ctx
// 传播兜底，重试语义（需重放已消费的 reader）留作后续增强。
func (m *BudgetModel) Stream(
	ctx context.Context,
	input []*schema.Message,
	opts ...model.Option,
) (*schema.StreamReader[*schema.Message], error) {

	if m.cfg.Engine != nil {
		input = m.cfg.Engine.CompressInPlace(input)
	}
	return m.raw.Stream(ctx, input, opts...)
}

// WithTools 实现 model.ToolCallingChatModel.WithTools。
// 必须返回新的装饰器包装派生后的模型，否则预算/超时/重试能力会静默丢失。
func (m *BudgetModel) WithTools(tools []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	derived, err := m.raw.WithTools(tools)
	if err != nil {
		return nil, fmt.Errorf("budget model WithTools: %w", err)
	}
	return &BudgetModel{raw: derived, cfg: m.cfg}, nil
}

// ----------------------------------------------------------------------------
// 辅助函数
// ----------------------------------------------------------------------------

// recordUsage 从输出提取 token 用量并消费预算；缺失时按估算兜底。
func (m *BudgetModel) recordUsage(b *budget.Budget, input []*schema.Message, out *schema.Message) {
	if out.ResponseMeta != nil && out.ResponseMeta.Usage != nil {
		_ = b.ConsumeTokens(budget.TokenUsage{
			PromptTokens:     out.ResponseMeta.Usage.PromptTokens,
			CompletionTokens: out.ResponseMeta.Usage.CompletionTokens,
			TotalTokens:      out.ResponseMeta.Usage.TotalTokens,
		})
		return
	}
	// 估算兜底：输出 + 输入粗略 token
	est := estimateTokensForMessages(input) + estimateTextLen(out.Content)
	_ = b.ConsumeTokens(budget.TokenUsage{TotalTokens: est})
}

// toolCallSignature 把本轮 ToolCalls 摘要为稳定签名，用于无进展检测。
func toolCallSignature(msg *schema.Message) string {
	if msg == nil || len(msg.ToolCalls) == 0 {
		return ""
	}
	var sb strings.Builder
	for _, tc := range msg.ToolCalls {
		sb.WriteString(tc.Function.Name)
		sb.WriteString(":")
		sb.WriteString(tc.Function.Arguments)
		sb.WriteString("|")
	}
	h := sha256.Sum256([]byte(sb.String()))
	return hex.EncodeToString(h[:8])
}

// syntheticErrorMessage 生成一条无 ToolCalls 的 assistant 消息描述错误，
// 使图分支判定无工具调用后路由到 END 收尾，同时把错误信息暴露给最终输出。
func syntheticErrorMessage(err error) *schema.Message {
	return &schema.Message{
		Role:    schema.Assistant,
		Content: fmt.Sprintf("[模型调用失败，已终止本轮]: %v", err),
	}
}

// sleepBackoff 在退避等待期间尊重 ctx 取消。
func sleepBackoff(ctx context.Context, base time.Duration, attempt int) error {
	if base <= 0 {
		base = time.Second
	}
	wait := base * time.Duration(1<<uint(attempt-1))
	if wait > 30*time.Second {
		wait = 30 * time.Second
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// isRetryable 判断错误是否值得重试。
// eino 模型错误多为不透明包装，这里基于错误类型与消息做启发式判断。
func isRetryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true // 超时可能是瞬时压力，退避后值得重试
	}
	// 网络层错误重试
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	// io 相关瞬时错误
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return true
	}
	// 基于消息文本的启发式：限流 / 5xx / 网关超时 → 重试；4xx 参数/内容违规 → 不重试
	msg := err.Error()
	if containsAny(msg, "429", "rate limit", "rate_limit", "too many requests") {
		return true
	}
	if containsAny(msg, "500", "502", "503", "504", "internal server error",
		"bad gateway", "service unavailable", "gateway timeout") {
		return true
	}
	if containsAny(msg, "timeout", "timed out", "connection reset", "connection refused",
		"no such host", "i/o timeout", "EOF") {
		return true
	}
	// 明确不可重试：参数错误 / 内容违规 / 鉴权失败
	if containsAny(msg, "400", "401", "403", "invalid_request",
		"content policy", "content_filter", "safety", "unauthorized") {
		return false
	}
	return false
}

// containsAny 报告 s 是否包含任意子串（大小写不敏感）。
func containsAny(s string, subs ...string) bool {
	ls := strings.ToLower(s)
	for _, sub := range subs {
		if sub == "" {
			continue
		}
		if strings.Contains(ls, strings.ToLower(sub)) {
			return true
		}
	}
	return false
}

// estimateTokensForMessages 与 estimateTextLen 复用 context 包的估算逻辑。
// 这里提供轻量本地实现，避免跨包导出未导出的估算函数。
func estimateTokensForMessages(msgs []*schema.Message) int {
	total := 0
	for _, m := range msgs {
		if m == nil {
			continue
		}
		total += estimateTextLen(m.Content)
		for _, tc := range m.ToolCalls {
			total += estimateTextLen(tc.Function.Name) + estimateTextLen(tc.Function.Arguments)
		}
		total += 4
	}
	return total
}

func estimateTextLen(s string) int {
	if s == "" {
		return 0
	}
	return len([]rune(s))/2 + 1
}
