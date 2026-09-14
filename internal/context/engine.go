package context

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/daqiaoliang-coder/agentrix/internal/session"
)

var errSummarizerNotConfigured = errors.New("context: summarizer model is not configured")

// trimKeepRecentTools 是压缩时保留完整内容的最近 tool 消息条数。
const trimKeepRecentTools = 4

const trimmedToolPlaceholder = "[tool output trimmed]"

const summarizeInstruction = "请将以下多轮对话压缩为简洁摘要，保留用户意图、关键决策、结论与待办，不要添加对话中未出现的信息。"

type Engine struct {
	// Model 用于生成中段历史的摘要；为 nil 时 Compress 会返回错误。
	Model model.ToolCallingChatModel

	TokenBudget       int
	TailTokenBudget   int
	CompressThreshold float64
	MaxSummaryRatio   float64
}

func NewEngine() *Engine {
	const tokenBudget = 128000
	return &Engine{
		TokenBudget:       tokenBudget,
		TailTokenBudget:   tokenBudget / 4,
		CompressThreshold: 0.8,
		MaxSummaryRatio:   0.2,
	}
}

func (e *Engine) Assemble(
	systemPrompt string,
	state *session.State,
	history []*schema.Message,
	userInput string,
) []*schema.Message {

	messages := make([]*schema.Message, 0, len(history)+3)
	messages = append(messages, schema.SystemMessage(systemPrompt))
	if state.MemorySummary != "" {
		messages = append(messages, schema.SystemMessage(state.MemorySummary))
	}
	messages = append(messages, history...)
	messages = append(messages, schema.UserMessage(userInput))
	return messages
}

// summarize 调用模型把中段消息压缩为摘要。
func (e *Engine) summarize(ctx context.Context, msgs []*schema.Message, prevSummary string) (string, error) {
	if e.Model == nil {
		return "", errSummarizerNotConfigured
	}

	var b strings.Builder
	b.WriteString(summarizeInstruction)
	if prevSummary != "" {
		b.WriteString("\n\n已有摘要：\n")
		b.WriteString(prevSummary)
	}
	b.WriteString("\n\n对话记录：\n")
	for _, m := range msgs {
		fmt.Fprintf(&b, "[%s] %s\n", m.Role, m.Content)
	}

	resp, err := e.Model.Generate(ctx, []*schema.Message{schema.UserMessage(b.String())})
	if err != nil {
		return "", fmt.Errorf("generate summary: %w", err)
	}
	return resp.Content, nil
}

// truncateSummary 将摘要长度限制在中段消息估算 token 总量的 MaxSummaryRatio 以内。
func (e *Engine) truncateSummary(summary string, middle []*schema.Message) string {
	var total int
	for _, m := range middle {
		total += estimateTokens(m)
	}
	limit := int(float64(total) * e.MaxSummaryRatio)
	if limit <= 0 || utf8.RuneCountInString(summary) <= limit {
		return summary
	}
	runes := []rune(summary)
	return string(runes[:limit]) + "…"
}

// trimToolOutputs 截断较早 tool 消息的内容，仅保留最近 keepRecent 条完整输出。
func trimToolOutputs(messages []*schema.Message, keepRecent int) []*schema.Message {
	if keepRecent < 0 {
		keepRecent = 0
	}
	total := 0
	for _, m := range messages {
		if m.Role == schema.Tool {
			total++
		}
	}
	cutoff := total - keepRecent

	out := make([]*schema.Message, len(messages))
	seen := 0
	for i, m := range messages {
		if m.Role == schema.Tool {
			seen++
			if seen <= cutoff {
				cp := *m
				cp.Content = trimmedToolPlaceholder
				m = &cp
			}
		}
		out[i] = m
	}
	return out
}

// splitByBoundary 将消息切为 head（系统前缀）、middle（待摘要）、
// tail（从尾部累计、不超过 tailTokenBudget 的最近消息）。
func splitByBoundary(messages []*schema.Message, tailTokenBudget int) (head, middle, tail []*schema.Message) {
	n := 0
	for n < len(messages) && messages[n].Role == schema.System {
		n++
	}
	head = messages[:n]
	rest := messages[n:]

	i := len(rest)
	budget := tailTokenBudget
	for i > 0 {
		cost := estimateTokens(rest[i-1])
		if cost > budget {
			break
		}
		budget -= cost
		i--
	}
	return head, rest[:i], rest[i:]
}

// fixToolCallPairs 移除孤立的 tool 响应和未被响应的 assistant tool_calls，
// 避免模型 API 因配对不完整而报错。
func fixToolCallPairs(messages []*schema.Message) []*schema.Message {
	called := make(map[string]bool)
	answered := make(map[string]bool)
	for _, m := range messages {
		switch m.Role {
		case schema.Assistant:
			for _, tc := range m.ToolCalls {
				called[tc.ID] = true
			}
		case schema.Tool:
			answered[m.ToolCallID] = true
		}
	}

	out := make([]*schema.Message, 0, len(messages))
	for _, m := range messages {
		switch {
		case m.Role == schema.Tool:
			if called[m.ToolCallID] {
				out = append(out, m)
			}
		case m.Role == schema.Assistant && len(m.ToolCalls) > 0:
			paired := make([]schema.ToolCall, 0, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				if answered[tc.ID] {
					paired = append(paired, tc)
				}
			}
			if len(paired) == 0 {
				continue
			}
			if len(paired) < len(m.ToolCalls) {
				cp := *m
				cp.ToolCalls = paired
				out = append(out, &cp)
			} else {
				out = append(out, m)
			}
		default:
			out = append(out, m)
		}
	}
	return out
}

// estimateTokens 粗略估算消息 token 数（以 rune 计，对中文偏保守）。
func estimateTokens(m *schema.Message) int {
	n := utf8.RuneCountInString(m.Content)
	for _, tc := range m.ToolCalls {
		n += utf8.RuneCountInString(tc.Function.Name) +
			utf8.RuneCountInString(tc.Function.Arguments) + 8
	}
	return n
}
