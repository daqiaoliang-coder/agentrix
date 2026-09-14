package core

import (
	"context"
	"fmt"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
)

func BuildAgentGraph(
	ctx context.Context,
	chatModel model.ToolCallingChatModel,
	tools []tool.BaseTool,
	maxIterations int,
) (compose.Runnable[[]*schema.Message, *schema.Message], error) {

	g := compose.NewGraph[[]*schema.Message, *schema.Message]()

	if err := g.AddChatModelNode("model", chatModel); err != nil {
		return nil, fmt.Errorf("add model node: %w", err)
	}

	toolsNode, err := compose.NewToolNode(ctx, &compose.ToolsNodeConfig{
		Tools: tools,
	})
	if err != nil {
		return nil, fmt.Errorf("create tools node: %w", err)
	}

	if err := g.AddToolsNode("tools", toolsNode); err != nil {
		return nil, fmt.Errorf("add tools node: %w", err)
	}

	if err := g.AddEdge(compose.START, "model"); err != nil {
		return nil, fmt.Errorf("add edge start->model: %w", err)
	}

	branch := compose.NewGraphBranch(
		func(ctx context.Context, msg *schema.Message) (string, error) {
			if len(msg.ToolCalls) > 0 {
				return "tools", nil
			}
			return compose.END, nil
		},
		map[string]bool{"tools": true, compose.END: true},
	)
	if err := g.AddBranch("model", branch); err != nil {
		return nil, fmt.Errorf("add branch: %w", err)
	}

	if err := g.AddEdge("tools", "model"); err != nil {
		return nil, fmt.Errorf("add edge tools->model: %w", err)
	}

	if maxIterations <= 0 {
		maxIterations = 8
	}

	r, err := g.Compile(ctx,
		compose.WithMaxRunSteps(maxIterations*2+2),
		compose.WithNodeTriggerMode(compose.AnyPredecessor),
	)
	if err != nil {
		return nil, fmt.Errorf("compile: %w", err)
	}
	return r, nil
}
