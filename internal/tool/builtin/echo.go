package builtin

import (
	"context"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

type EchoTool struct{}

func (e *EchoTool) Info(_ context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "echo",
		Desc: "回显输入内容，用于测试",
	}, nil
}

func (e *EchoTool) InvokableRun(
	_ context.Context,
	argumentsInJSON string,
	_ ...tool.Option,
) (string, error) {
	return "echo: " + argumentsInJSON, nil
}
