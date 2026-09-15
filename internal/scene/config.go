package scene

import (
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
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

	// 双层超时：外层覆盖整个 Turn，内层覆盖单次模型调用。
	// 零值表示不限制，向后兼容已有配置。
	TotalTimeout     time.Duration // 外层：整个 Turn 的总时间预算
	ModelCallTimeout time.Duration // 内层：单次模型调用的超时

	// NoProgressLimit 无进展检测阈值：连续 N 轮工具调用签名相同则注入收尾提示。
	// 零值表示禁用无进展检测。
	NoProgressLimit int
}
