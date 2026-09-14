package context

import (
	"github.com/cloudwego/eino/schema"

	"github.com/daqiaoliang-coder/agentrix/internal/session"
)

type Engine struct {
	TokenBudget       int
	CompressThreshold float64
	MaxSummaryRatio   float64
}

func NewEngine() *Engine {
	return &Engine{
		TokenBudget:       128000,
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
