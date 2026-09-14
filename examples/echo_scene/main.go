package main

import (
	"context"
	"fmt"
	"log"
	"os"

	openai "github.com/cloudwego/eino-ext/components/model/openai"

	"github.com/daqiaoliang-coder/agentrix/internal/harness/core"
	"github.com/daqiaoliang-coder/agentrix/internal/scene"
	"github.com/daqiaoliang-coder/agentrix/internal/session"
	"github.com/daqiaoliang-coder/agentrix/internal/tool"
	"github.com/daqiaoliang-coder/agentrix/internal/tool/builtin"
)

func main() {
	ctx := context.Background()

	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		log.Fatal("请设置 OPENAI_API_KEY")
	}

	chatModel, err := openai.NewChatModel(ctx, &openai.ChatModelConfig{
		Model:  "gpt-4o-mini",
		APIKey: apiKey,
	})
	if err != nil {
		log.Fatalf("create chat model: %v", err)
	}

	reg := tool.NewRegistry()
	reg.Register("echo", &builtin.EchoTool{})

	cfg := &scene.SceneConfig{
		Key:           "echo_scene",
		Name:          "回显场景",
		SystemPrompt:  "你是一个测试助手，可以使用 echo 工具回显内容。",
		Model:         chatModel,
		Tools:         reg.List(),
		MaxIterations: 5,
		TokenBudget:   8192,
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
