package scene

import (
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
)

// SceneConfig 定义一个场景的完整配置
type SceneConfig struct {
	Key               string                     // 场景唯一标识（固定枚举）
	Name              string                     // 场景名称
	SystemPrompt      string                     // System Prompt
	Model             model.ToolCallingChatModel // 模型实例
	Tools             []tool.BaseTool            // 可用工具
	MaxIterations     int                        // ReAct 最大循环次数
	TokenBudget       int                        // 单次 Turn Token 预算
	CompressThreshold float64                    // 压缩触发阈值（0-1）
}
