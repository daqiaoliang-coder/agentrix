package projection

import "time"

type SignalType string

const (
	TurnStart        SignalType = "turn_start"
	TurnEnd          SignalType = "turn_end"
	LLMRequesting    SignalType = "llm_requesting"
	LLMToken         SignalType = "llm_token"
	LLMEnd           SignalType = "llm_end"
	ToolStart        SignalType = "tool_start"
	ToolEnd          SignalType = "tool_end"
	ApproveRequested SignalType = "approve_requested"
	ContextCompacted SignalType = "context_compacted"
)

type Signal struct {
	ID             string     `json:"id"`
	TS             time.Time  `json:"ts"`
	ThreadID       string     `json:"thread_id"`
	RoundID        string     `json:"round_id"`
	Type           SignalType `json:"type"`
	Payload        any        `json:"payload"`
	ConsumedInputs []string   `json:"consumed_inputs"`
}
