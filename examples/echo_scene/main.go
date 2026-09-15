package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	openai "github.com/cloudwego/eino-ext/components/model/openai"

	"github.com/daqiaoliang-coder/agentrix/internal/harness/core"
	"github.com/daqiaoliang-coder/agentrix/internal/scene"
	"github.com/daqiaoliang-coder/agentrix/internal/session"
	"github.com/daqiaoliang-coder/agentrix/internal/tool"
	"github.com/daqiaoliang-coder/agentrix/internal/tool/builtin"
)

func main() {
	ctx := context.Background()

	// 默认走火山方舟（Ark，OpenAI 兼容）；也可用 OPENAI_API_KEY/OPENAI_BASE_URL 覆盖
	apiKey := os.Getenv("ARK_API_KEY")
	if apiKey == "" {
		apiKey = os.Getenv("OPENAI_API_KEY")
	}
	if apiKey == "" {
		log.Fatal("请设置 ARK_API_KEY（火山方舟 API Key）")
	}

	baseURL := os.Getenv("OPENAI_BASE_URL")
	if baseURL == "" {
		baseURL = "https://ark.cn-beijing.volces.com/api/v3"
	}

	// ARK_MODEL 填方舟接入点 ID（ep-xxxx）或模型版本号（如 doubao-1-5-pro-32k-250115）
	model := os.Getenv("ARK_MODEL")
	if model == "" {
		model = os.Getenv("OPENAI_MODEL")
	}
	if model == "" {
		log.Fatal("请设置 ARK_MODEL（方舟接入点 ID，如 ep-20250xxx）")
	}

	chatModel, err := openai.NewChatModel(ctx, &openai.ChatModelConfig{
		Model:   model,
		APIKey:  apiKey,
		BaseURL: baseURL,
	})
	if err != nil {
		log.Fatalf("create chat model: %v", err)
	}

	reg := tool.NewRegistry()
	reg.Register("echo", &builtin.EchoTool{})

	cfg := &scene.SceneConfig{
		Key:              "echo_scene",
		Name:             "回显场景",
		SystemPrompt:     "你是一个测试助手，可以使用 echo 工具回显内容。",
		Model:            chatModel,
		Tools:            reg.List(),
		MaxIterations:    5,
		TokenBudget:      8192,
		TotalTimeout:     30 * time.Second, // 外层：整个 Turn 总时间预算
		ModelCallTimeout: 10 * time.Second, // 内层：单次模型调用超时
		NoProgressLimit:  3,                // 连续 3 轮无新进展则注入收尾提示
	}

	store := session.NewMemoryStore()
	agent, err := core.NewAgent(ctx, cfg, store)
	if err != nil {
		log.Fatalf("new agent: %v", err)
	}

	output, err := agent.Run(ctx, "session-1", "请回显: Hello Agentrix")
	if err != nil {
		log.Fatalf("run agent: %v", err)
	}

	fmt.Println("Agent:", output.Content)
}
