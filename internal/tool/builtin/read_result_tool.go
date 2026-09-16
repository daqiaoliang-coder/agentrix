package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"

	ctxengine "github.com/daqiaoliang-coder/agentrix/internal/context"
)

// ReadResultTool 把 SpillStore 的回读能力暴露为模型可调用的工具。
//
// 它是「offload 而非 delete」这条压缩纪律的闭环：上下文压缩淘汰老工具结果时，
// 原文落入 SpillStore、上下文里只留带恢复路径的 stub。模型看到 stub 后，
// 用本工具传 tool_call_id 就能取回完整结果，而不是对着占位符幻觉续编。
//
// 未配置 SpillStore 时本工具不注册（见 scene 装配），stub 也会明确告知不可回读。
type ReadResultTool struct {
	spill ctxengine.SpillStore
	// maxChars 是单次回读的字符上限，防止一条超大结果回读后又把上下文顶爆。
	// 零值表示不限制。
	maxChars int
}

// NewReadResultTool 构造回读工具。spill 为 nil 时工具仍可注册，但调用会返回错误。
func NewReadResultTool(spill ctxengine.SpillStore, maxChars int) *ReadResultTool {
	return &ReadResultTool{spill: spill, maxChars: maxChars}
}

type readResultArgs struct {
	// 被 offload 的工具调用 ID，取自 stub 中的「调用: xxx」行
	ToolCallID string `json:"tool_call_id"`
}

func (t *ReadResultTool) Info(_ context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "read_result",
		Desc: "回读已被上下文压缩移出窗口的工具结果原文。当你在上下文中看到形如" +
			"「[工具 X 的结果已移出上下文…] 恢复方式: 调用 read_result 工具（传 tool_call_id=…）」的占位符，" +
			"且确实需要该结果的完整内容时，用本工具传入对应的 tool_call_id 取回原文。" +
			"若占位符标注了「不可回读」，说明原文未保存，应重新调用原工具获取数据，不要用本工具。",
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"tool_call_id": {
				Type:     schema.String,
				Desc:     "被移出上下文的工具调用 ID，须与占位符中标注的 ID 完全一致",
				Required: true,
			},
		}),
	}, nil
}

func (t *ReadResultTool) InvokableRun(
	ctx context.Context,
	argumentsInJSON string,
	_ ...tool.Option,
) (string, error) {
	if t.spill == nil {
		return "", fmt.Errorf("read_result: spill store not configured")
	}
	var args readResultArgs
	if err := json.Unmarshal([]byte(argumentsInJSON), &args); err != nil {
		return "", fmt.Errorf("invalid read_result arguments: %w", err)
	}
	id := strings.TrimSpace(args.ToolCallID)
	if id == "" {
		return "", fmt.Errorf("read_result: tool_call_id is required")
	}

	content, err := t.spill.Read(ctx, ctxengine.SpillRef(id))
	if err != nil {
		return "", fmt.Errorf("read_result: %w", err)
	}
	if t.maxChars <= 0 {
		return content, nil
	}

	// 超长回读截断，并显式告知模型被截断，避免它误以为拿到了全量
	runes := []rune(content)
	if len(runes) <= t.maxChars {
		return content, nil
	}
	return string(runes[:t.maxChars]) +
		fmt.Sprintf("\n\n[结果过长，已截断至前 %d 字符，原文共 %d 字符]", t.maxChars, len(runes)), nil
}
