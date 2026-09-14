package scene

import (
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
)

type SceneConfig struct {
	Key               string
	Name              string
	SystemPrompt      string
	Model             model.ToolCallingChatModel
	Tools             []tool.BaseTool
	MaxIterations     int
	TokenBudget       int
	CompressThreshold float64
}
