package context

import (
	"context"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/daqiaoliang-coder/agentrix/internal/session"
)

const (
	defaultTokenBudget     = 128000
	defaultCompressRatio   = 0.8
	defaultTailTokenBudget = 20000
	defaultSummaryRatio    = 0.2
	defaultMaxSummaryToken = 12000
	trimKeepRecentTools    = 4
)

// Engine 负责上下文装配与压缩
type Engine struct {
	TokenBudget       int     // 模型上下文窗口预算
	CompressThreshold float64 // 触发压缩的阈值（占 TokenBudget 比例）
	TailTokenBudget   int     // 尾部保护区大小
	MaxSummaryRatio   float64 // 摘要占被压缩内容的上限比例
	MaxSummaryTokens  int     // 摘要 token 绝对上限

	summaryModel model.BaseChatModel // 用于结构化摘要，可为 nil（降级为规则摘要）
}

func NewEngine() *Engine {
	return &Engine{
		TokenBudget:       defaultTokenBudget,
		CompressThreshold: defaultCompressRatio,
		TailTokenBudget:   defaultTailTokenBudget,
		MaxSummaryRatio:   defaultSummaryRatio,
		MaxSummaryTokens:  defaultMaxSummaryToken,
	}
}

// NewEngineWithModel 注入摘要模型，启用 LLM 结构化摘要
func NewEngineWithModel(m model.BaseChatModel) *Engine {
	e := NewEngine()
	e.summaryModel = m
	return e
}

// CompressInPlace 对消息序列做规则压缩（工具输出裁剪 + 工具调用对修复），
// 不调用 LLM、不需要 session.State，适合在模型调用前对上下文做轻量维护。
// 保留最近 trimKeepRecentTools 条工具消息原文，较早的替换为占位符。
// 这是 Agent.Run 级 Assemble（含 LLM 摘要）的补充：循环内每轮只做规则裁剪，
// 避免工具输出原样累积污染上下文。
func (e *Engine) CompressInPlace(messages []*schema.Message) []*schema.Message {
	trimmed := trimToolOutputs(messages, trimKeepRecentTools)
	return fixToolCallPairs(trimmed)
}

// Assemble 装配模型可见上下文，必要时触发压缩
func (e *Engine) Assemble(
	ctx context.Context,
	systemPrompt string,
	state *session.State,
	history []*schema.Message,
	userInput string,
) ([]*schema.Message, error) {

	messages := make([]*schema.Message, 0, len(history)+3)
	messages = append(messages, schema.SystemMessage(systemPrompt))
	if state != nil && state.MemorySummary != "" {
		messages = append(messages, schema.SystemMessage(state.MemorySummary))
	}
	messages = append(messages, history...)
	messages = append(messages, schema.UserMessage(userInput))

	if !e.shouldCompress(messages) {
		return messages, nil
	}

	return e.compress(ctx, messages, state)
}

// ----------------------------------------------------------------------------
// 四阶段压缩主流程（规则优先，LLM 仅调用一次）
// 1. 优先裁剪工具输出（最占空间、最易过期）
// 2. 保护头和尾（头定义任务起点，尾包含最新状态）
// 3. 中间段做结构化摘要（七个字段：目标、约束、进度、关键决策、相关文件、下一步、关键上下文）
// 4. 最后修复被切断的 tool_call/tool_result 配对
// ----------------------------------------------------------------------------

func (e *Engine) compress(
	ctx context.Context,
	messages []*schema.Message,
	state *session.State,
) ([]*schema.Message, error) {

	// 阶段①：工具输出修剪（规则，不调 LLM）
	trimmed := trimToolOutputs(messages, trimKeepRecentTools)

	// 阶段②：边界确定，保护头尾
	head, middle, tail := splitByBoundary(trimmed, e.TailTokenBudget)
	if len(middle) == 0 {
		return fixToolCallPairs(trimmed), nil
	}

	// 阶段③：结构化摘要（仅调用一次 LLM，增量更新）
	var prevSummary string
	if state != nil {
		prevSummary = state.MemorySummary
	}
	summary, err := e.summarize(ctx, middle, prevSummary)
	if err != nil {
		return nil, err
	}
	summary = e.truncateSummary(summary, middle)

	if state != nil {
		state.MemorySummary = summary
	}

	// 阶段④：工具调用对修复
	merged := make([]*schema.Message, 0, len(head)+1+len(tail))
	merged = append(merged, head...)
	merged = append(merged, schema.SystemMessage(summary))
	merged = append(merged, tail...)

	return fixToolCallPairs(merged), nil
}

// ----------------------------------------------------------------------------
// 阶段判断
// ----------------------------------------------------------------------------

// shouldCompress 判断当前消息序列是否接近模型窗口上限
func (e *Engine) shouldCompress(messages []*schema.Message) bool {
	budget := e.TokenBudget
	if budget <= 0 {
		budget = defaultTokenBudget
	}
	ratio := e.CompressThreshold
	if ratio <= 0 || ratio > 1 {
		ratio = defaultCompressRatio
	}
	threshold := int(float64(budget) * ratio)
	return estimateTokens(messages) >= threshold
}

// ----------------------------------------------------------------------------
// 阶段①：工具输出修剪
// ----------------------------------------------------------------------------

// trimToolOutputs 把较早的工具输出替换为占位符。
// 保留最近 keepRecent 条工具消息原文，其余按确定性规则裁剪。
// 这是成本最低、收益最高的一步，通常能省掉绝大多数 token。
func trimToolOutputs(messages []*schema.Message, keepRecent int) []*schema.Message {
	toolIdxs := make([]int, 0)
	for i, m := range messages {
		if m != nil && m.Role == schema.Tool {
			toolIdxs = append(toolIdxs, i)
		}
	}
	if len(toolIdxs) <= keepRecent {
		return messages
	}

	cut := make(map[int]struct{}, len(toolIdxs)-keepRecent)
	for _, idx := range toolIdxs[:len(toolIdxs)-keepRecent] {
		cut[idx] = struct{}{}
	}

	out := make([]*schema.Message, len(messages))
	copy(out, messages)
	for i := range out {
		if _, ok := cut[i]; !ok {
			continue
		}
		orig := out[i]
		out[i] = &schema.Message{
			Role:       orig.Role,
			ToolCallID: orig.ToolCallID,
			ToolName:   orig.ToolName,
			Name:       orig.Name,
			Content: fmt.Sprintf(
				"[工具 %s 的输出已裁剪，原始长度 %d 字符]",
				orig.ToolName, len([]rune(orig.Content)),
			),
		}
	}
	return out
}

// ----------------------------------------------------------------------------
// 阶段②：边界确定
// ----------------------------------------------------------------------------

// splitByBoundary 保护头部（System Prompt + 首次交互）和尾部（最近 tailBudget token），
// 中间段才是允许被改写的区域。
func splitByBoundary(
	messages []*schema.Message,
	tailBudget int,
) (head, middle, tail []*schema.Message) {

	if len(messages) == 0 {
		return nil, nil, nil
	}
	if tailBudget <= 0 {
		tailBudget = defaultTailTokenBudget
	}

	// 头部：起始连续的 System 消息 + 第一条非 System 消息（首次交互）
	headEnd := 0
	for headEnd < len(messages) && messages[headEnd].Role == schema.System {
		headEnd++
	}
	if headEnd < len(messages) {
		headEnd++
	}
	head = messages[:headEnd]

	if headEnd >= len(messages) {
		return head, nil, nil
	}

	// 尾部：从末尾向前累加，直到超过 tailBudget
	rest := messages[headEnd:]
	tailStart := len(rest)
	used := 0
	for i := len(rest) - 1; i >= 0; i-- {
		t := estimateMessageTokens(rest[i])
		if used+t > tailBudget && tailStart < len(rest) {
			break
		}
		used += t
		tailStart = i
	}

	middle = rest[:tailStart]
	tail = rest[tailStart:]
	return head, middle, tail
}

// ----------------------------------------------------------------------------
// 阶段③：结构化摘要
// ----------------------------------------------------------------------------

const summarySchemaPrompt = `你是对话上下文压缩器。请把给定的历史对话压缩为结构化摘要，
严格输出以下七个字段，每个字段一行，不要输出任何额外内容：

目标: <用户最终想达成什么>
约束: <必须遵守的规则、限制>
进度: <已经完成了什么>
关键决策: <做过哪些重要决定>
相关文件: <涉及的文件、表、资源>
下一步: <接下来应该做什么>
关键上下文: <其他必须保留的信息>`

// summarize 对中间段做结构化摘要。
// 第二次及以后在前一次摘要基础上增量更新，避免语义漂移。
func (e *Engine) summarize(
	ctx context.Context,
	middle []*schema.Message,
	prevSummary string,
) (string, error) {

	if len(middle) == 0 {
		return prevSummary, nil
	}

	// 无摘要模型时降级为规则摘要，保证链路可用
	if e.summaryModel == nil {
		return fmt.Sprintf(
			"[历史压缩] 已省略 %d 条较早消息，最早一条为 %s 角色。",
			len(middle), middle[0].Role,
		), nil
	}

	var sb strings.Builder
	if prevSummary != "" {
		sb.WriteString("已有摘要（请在此基础上增量更新，不要从头重写）：\n")
		sb.WriteString(prevSummary)
		sb.WriteString("\n\n新增对话：\n")
	}
	for _, m := range middle {
		if m == nil {
			continue
		}
		content := m.Content
		if len(m.ToolCalls) > 0 {
			names := make([]string, 0, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				names = append(names, tc.Function.Name)
			}
			content += fmt.Sprintf(" [tool_calls: %s]", strings.Join(names, ","))
		}
		sb.WriteString(fmt.Sprintf("[%s] %s\n", m.Role, content))
	}

	req := []*schema.Message{
		schema.SystemMessage(summarySchemaPrompt),
		schema.UserMessage(sb.String()),
	}

	out, err := e.summaryModel.Generate(ctx, req)
	if err != nil {
		return "", fmt.Errorf("summarize: %w", err)
	}
	if out == nil {
		return prevSummary, nil
	}
	return out.Content, nil
}

// truncateSummary 执行摘要预算：
// 摘要长度 ≤ 被压缩内容的 MaxSummaryRatio，且 ≤ MaxSummaryTokens，取最严约束。
func (e *Engine) truncateSummary(summary string, source []*schema.Message) string {
	if summary == "" {
		return summary
	}

	budget := e.MaxSummaryTokens
	if budget <= 0 {
		budget = defaultMaxSummaryToken
	}
	if e.MaxSummaryRatio > 0 {
		srcTokens := estimateTokens(source)
		ratioBudget := int(float64(srcTokens) * e.MaxSummaryRatio)
		if ratioBudget > 0 && ratioBudget < budget {
			budget = ratioBudget
		}
	}

	if estimateTextTokens(summary) <= budget {
		return summary
	}

	// 按 rune 保守截断（粗略按 1 token ≈ 2 字符）
	maxChars := budget * 2
	runes := []rune(summary)
	if len(runes) > maxChars {
		runes = runes[:maxChars]
	}
	return string(runes)
}

// ----------------------------------------------------------------------------
// 阶段④：工具调用对修复
// ----------------------------------------------------------------------------

// fixToolCallPairs 修复被压缩切断的 tool_call / tool_result 配对。
// 保证压缩后的消息序列在协议层自洽，否则下次模型调用会直接报错。
func fixToolCallPairs(messages []*schema.Message) []*schema.Message {
	declared := make(map[string]struct{})
	for _, m := range messages {
		if m == nil || m.Role != schema.Assistant {
			continue
		}
		for _, tc := range m.ToolCalls {
			if tc.ID != "" {
				declared[tc.ID] = struct{}{}
			}
		}
	}

	answered := make(map[string]struct{})
	for _, m := range messages {
		if m == nil || m.Role != schema.Tool || m.ToolCallID == "" {
			continue
		}
		answered[m.ToolCallID] = struct{}{}
	}

	out := make([]*schema.Message, 0, len(messages))
	for _, m := range messages {
		if m == nil {
			continue
		}

		// 丢弃孤立的 tool 结果：找不到对应的 tool_call 声明
		if m.Role == schema.Tool {
			if _, ok := declared[m.ToolCallID]; !ok {
				continue
			}
			out = append(out, m)
			continue
		}

		out = append(out, m)

		// 为缺失的 tool 结果补占位，避免 assistant.tool_calls 悬空
		if m.Role == schema.Assistant {
			for _, tc := range m.ToolCalls {
				if tc.ID == "" {
					continue
				}
				if _, ok := answered[tc.ID]; ok {
					continue
				}
				out = append(out, &schema.Message{
					Role:       schema.Tool,
					ToolCallID: tc.ID,
					ToolName:   tc.Function.Name,
					Content:    "[该工具结果已被上下文压缩省略]",
				})
				answered[tc.ID] = struct{}{}
			}
		}
	}
	return out
}

// ----------------------------------------------------------------------------
// Token 估算（粗略，生产环境建议替换为 tiktoken）
// ----------------------------------------------------------------------------

func estimateTokens(messages []*schema.Message) int {
	total := 0
	for _, m := range messages {
		total += estimateMessageTokens(m)
	}
	return total
}

func estimateMessageTokens(m *schema.Message) int {
	if m == nil {
		return 0
	}
	n := estimateTextTokens(m.Content)
	for _, tc := range m.ToolCalls {
		n += estimateTextTokens(tc.Function.Name)
		n += estimateTextTokens(tc.Function.Arguments)
	}
	return n + 4 // 消息结构固定开销
}

// estimateTextTokens 中英混排的粗略估算：约 2 字符 = 1 token
func estimateTextTokens(s string) int {
	if s == "" {
		return 0
	}
	return len([]rune(s))/2 + 1
}
