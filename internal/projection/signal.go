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

// Signal 是运行事实，不是 UI 文案，也不是模型上下文。它的目标是让外层系统观察 Agent 正在发生什么。
// 事件来源三条路径：Turn 生命周期、模型回调、工具和 middleware。
type Signal struct {
	ID             string     `json:"id"`
	TS             time.Time  `json:"ts"`
	ThreadID       string     `json:"thread_id"`
	RoundID        string     `json:"round_id"`
	Type           SignalType `json:"type"`
	Payload        any        `json:"payload"`
	ConsumedInputs []string   `json:"consumed_inputs"`
}
