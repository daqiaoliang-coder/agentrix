package builtin

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

type EchoTool struct{}

type echoArgs struct {
	Content string `json:"content"`
}

func (e *EchoTool) Info(_ context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "echo",
		Desc: "回显输入内容，用于测试",
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"content": {
				Type:     schema.String,
				Desc:     "需要原样回显的文本内容",
				Required: true,
			},
		}),
	}, nil
}

func (e *EchoTool) InvokableRun(
	_ context.Context,
	argumentsInJSON string,
	_ ...tool.Option,
) (string, error) {
	var args echoArgs
	if err := json.Unmarshal([]byte(argumentsInJSON), &args); err != nil {
		return "", fmt.Errorf("invalid echo arguments: %w", err)
	}
	return "echo: " + args.Content, nil
}
