package eval

import (
	einotool "github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"

	"github.com/daqiaoliang-coder/agentrix/internal/harness/hitl"
	agenttool "github.com/daqiaoliang-coder/agentrix/internal/tool"
)

// Case 是一条离线评测用例：给定用户输入与模型脚本，断言 Agent 的
// 可观测行为（最终输出 / 工具调用 / 审批 / 过程信号 / 模型可见工具）。
type Case struct {
	// Name 用例唯一标识，同时作为会话 ID 后缀与报告中的展示名。
	Name string

	// SceneKey 场景 key，默认 "eval"。
	SceneKey string

	// SystemPrompt 场景系统提示，可留空。
	SystemPrompt string

	// Input 用户输入（首轮 Turn 的 user 消息）。
	Input string

	// Replies 模型脚本：按模型调用顺序返回的消息，用 ToolCall 或
	// schema.AssistantMessage 构造。
	Replies []*schema.Message

	// Tools 业务工具集（框架工具由 Agent 自动注册，无需在此列出）。
	Tools []einotool.BaseTool

	// Catalog / AllowedCommands 装配审批与命令白名单治理，
	// 用于评测 HITL 中断与白名单过滤。
	Catalog         *agenttool.Catalog
	AllowedCommands []string

	// MaxIterations ReAct 迭代上限；0 时取 len(Replies)+2。
	MaxIterations int

	// AutoResume 非 nil 时，审批中断自动按该决策恢复执行（循环上限 8 次）；
	// 为 nil 时停在首个中断，交由 ExpectApprovalRequired 断言。
	AutoResume *hitl.ApprovalDecision

	// Expect 声明式断言集合，任一失败即用例失败。
	Expect []Expectation
}
