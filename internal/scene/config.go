package scene

import (
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"

	"github.com/daqiaoliang-coder/agentrix/internal/skill"
	agenttool "github.com/daqiaoliang-coder/agentrix/internal/tool"
)

// SceneConfig 定义一个场景的完整配置
type SceneConfig struct {
	Key               string                     // 场景唯一标识（固定枚举）
	Name              string                     // 场景名称
	SystemPrompt      string                     // System Prompt
	Model             model.ToolCallingChatModel // 模型实例
	FallbackModel     model.ToolCallingChatModel // 降级模型（可选）：主模型重试耗尽后切到该模型再试一次
	Tools             []tool.BaseTool            // 可用工具
	MaxIterations     int                        // ReAct 最大循环次数
	TokenBudget       int                        // 单次 Turn Token 预算
	CompressThreshold float64                    // 压缩触发阈值（0-1）

	// Skills 为该场景可见的技能装载器（渐进式加载阶段 0）。
	// 非 nil 时，装配 Agent 会把全部技能的 name+description 注入系统提示，
	// 并自动注册 read_skill 工具供模型按需读取正文与细则。
	// nil 表示该场景不启用技能体系，行为与之前完全一致。
	Skills *skill.Loader

	// AllowedCommands 是场景级命令白名单（CLI Catalog 治理）。
	// 非空时，装配阶段按 Catalog 元数据过滤工具：只保留命令在白名单内的工具。
	// 空表示不过滤（向后兼容，Tools 全量可见）。
	AllowedCommands []string

	// Catalog 是命令目录（命令 → 工具映射 + 执行位置 + 审批标记）。
	// 与 AllowedCommands 配合完成白名单过滤，并为 NeedsApproval 的工具套上审批包装。
	// nil 表示不启用 Catalog 治理，Tools 原样透传（向后兼容）。
	Catalog *agenttool.Catalog

	// 双层超时：外层覆盖整个 Turn，内层覆盖单次模型调用。
	// 零值表示不限制，向后兼容已有配置。
	TotalTimeout     time.Duration // 外层：整个 Turn 的总时间预算
	ModelCallTimeout time.Duration // 内层：单次模型调用的超时

	// NoProgressLimit 无进展检测阈值：连续 N 轮工具调用签名相同则注入收尾提示。
	// 零值表示禁用无进展检测。
	NoProgressLimit int
}
