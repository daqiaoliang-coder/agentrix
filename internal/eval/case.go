package eval

import (
	einotool "github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"

	"github.com/daqiaoliang-coder/agentrix/internal/harness/hitl"
	"github.com/daqiaoliang-coder/agentrix/internal/scene"
	agenttool "github.com/daqiaoliang-coder/agentrix/internal/tool"
)

// Case 是一条离线评测用例。用 AgentConfig 或 Runner.ConfigFactory 接入真实
// SceneConfig；仅做框架回归时可以继续用 Replies 驱动 ScriptModel。
type Case struct {
	// Name 用例唯一标识，同时作为报告展示名。
	Name string

	// SceneKey 可用于按业务场景选择配置；为空时使用 AgentConfig.Key 或 "eval"。
	SceneKey string

	// Tags 用于按业务能力、风险级别等维度汇总结果。
	Tags []string

	// AgentMetadata 记录模型快照、prompt/skills/tools 版本或哈希，便于复现。
	AgentMetadata map[string]string `json:"-"`

	// SystemPrompt / Tools / Catalog / AllowedCommands 主要用于脚本回归。
	// 提供 AgentConfig 时，非零值会覆盖对应配置。
	SystemPrompt string
	Input        string

	// AgentConfig 携带业务侧完整的 prompts / skills / tools / model 配置。
	// 配合重复或并行评测时，优先使用 Runner.ConfigFactory 创建隔离实例。
	AgentConfig *scene.SceneConfig `json:"-"`

	// Replies 是按模型调用顺序返回的模型脚本。存在 AgentConfig 时，非 nil
	// Replies 会覆盖其中的 Model，以继续使用确定性脚本回放。
	Replies []*schema.Message `json:"-"`

	Tools []einotool.BaseTool `json:"-"`

	// Catalog / AllowedCommands 装配审批与命令白名单治理。
	Catalog         *agenttool.Catalog `json:"-"`
	AllowedCommands []string

	// MaxIterations 为 ReAct 迭代上限；0 时保留 AgentConfig 值，
	// 脚本模式默认取 len(Replies)+2。
	MaxIterations int

	// AutoResume 非 nil 时，审批中断按该决策自动恢复（循环上限 8 次）。
	AutoResume *hitl.ApprovalDecision `json:"-"`

	// Expect 声明式断言集合，任一失败即用例失败。
	Expect []Expectation `json:"-"`

	// Oracle 对静态配置做业务状态校验。需要每次 trial 独立创建环境时，
	// 将 Oracle 放入 AgentConfigFactory 返回的 AgentSetup.Verify。
	Oracle Oracle `json:"-"`
}
