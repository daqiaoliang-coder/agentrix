package core

import (
	"context"
	"fmt"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	"github.com/daqiaoliang-coder/agentrix/internal/harness/hitl"
)

// BuildAgentGraph 构建 ReAct 循环图
// “Graph 手搓 ReAct 循环”范式，用 compose.Graph 的有环能力实现“模型决策 → 工具执行 → 结果回填 → 继续决策”
// 拓扑：START → Model → Branch(有ToolCall?) → Tools → Model(回环) → END
// 关键设计点：
//
// compose.NewGraph[[]*schema.Message, *schema.Message]() 使用泛型指定输入输出类型，节点间类型不匹配在编译期报错。
//
// AddChatModelNode 直接接受 model.ToolCallingChatModel，Eino 自动处理模型调用和 ToolCall 解析。
//
// AddToolsNode 接受 compose.ToolsNodeConfig，自动执行工具并回填结果。
//
// WithMaxRunSteps 是防止有环图无限循环的必要选项，meego-ai 的迭代预算控制也对应此处。
//
// WithNodeTriggerMode(AnyPredecessor) 是默认模式，节点在所有前驱完成后触发
func BuildAgentGraph(
	ctx context.Context,
	chatModel model.ToolCallingChatModel,
	tools []tool.BaseTool,
	maxIterations int,
) (compose.Runnable[[]*schema.Message, *schema.Message], error) {

	// ① 创建有环图：输入消息历史，输出最终回复
	graph := compose.NewGraph[[]*schema.Message, *schema.Message]()

	// ② 添加模型节点（LLM 决策）
	if err := graph.AddChatModelNode("model", chatModel); err != nil {
		return nil, fmt.Errorf("add model node: %w", err)
	}

	// ② 构造 ToolsNode：直接传 []tool.BaseTool，Eino 内部自动分派
	toolsNode, err := compose.NewToolNode(ctx, &compose.ToolsNodeConfig{
		Tools: tools,
	})
	if err != nil {
		return nil, fmt.Errorf("new tool node: %w", err)
	}

	// ③ 添加工具执行节点
	if err := graph.AddToolsNode("tools", toolsNode); err != nil {
		return nil, fmt.Errorf("add tools node: %w", err)
	}

	// ④ 添加分支：模型输出后判断是否有 ToolCall
	branch := compose.NewGraphBranch(
		func(ctx context.Context, msg *schema.Message) (string, error) {
			if len(msg.ToolCalls) > 0 {
				return "tools", nil
			}
			return compose.END, nil
		},
		map[string]bool{"tools": true, compose.END: true},
	)
	if err := graph.AddBranch("model", branch); err != nil {
		return nil, fmt.Errorf("add branch: %w", err)
	}

	// ⑤ 添加边：START → model，tools → model（回环）
	graph.AddEdge(compose.START, "model")
	graph.AddEdge("tools", "model")

	if maxIterations <= 0 {
		maxIterations = 8
	}

	store := hitl.NewMemoryCheckPointStore()

	// ⑥ 编译：有环图必须设置 MaxRunSteps 防止无限循环
	runnable, err := graph.Compile(ctx,
		compose.WithMaxRunSteps(maxIterations*2+2), // 每轮约 2 步
		compose.WithNodeTriggerMode(compose.AnyPredecessor),
		compose.WithCheckPointStore(store), // 检查点
	)
	if err != nil {
		return nil, fmt.Errorf("compile graph: %w", err)
	}

	return runnable, nil
}
