package core

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	einotool "github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"

	ctxengine "github.com/daqiaoliang-coder/agentrix/internal/context"
	"github.com/daqiaoliang-coder/agentrix/internal/harness/hitl"
	"github.com/daqiaoliang-coder/agentrix/internal/projection"
	"github.com/daqiaoliang-coder/agentrix/internal/scene"
	"github.com/daqiaoliang-coder/agentrix/internal/session"
)

// subagentTool 把子代理包装为父模型可调用的 spawn_<key> 工具。
//
// 设计要点：
//   - 图只建一次：子 runnable 在父 NewAgent 装配期编译，多并发调用共享
//     （eino Runnable 本身并发安全），不为每次调用重复付图编译成本。
//   - 治理链路与顶层一致：子运行时经 buildRuntime 装配，审批包装、
//     命令白名单、预算、循环内压缩全部生效，子代理不是治理盲区。
//   - 会话隔离：子代理以 父会话::key（可再追加 suffix）为独立会话执行，
//     历史/状态/检查点与父会话互不污染，父会话 RawHistory 只留一条工具结果。
//   - 信号隔离：子代理 run 的 sink 为 nil，不向父 SSE 流过程信号；
//     父视角只有一对 tool_start/tool_end，子代理内部展开对父不可见。
//   - 防递归：构造时丢弃子场景的 Subagents 字段，子代理不能再派生孙代理。
type subagentTool struct {
	name            string
	key             string
	desc            string
	cfg             *scene.SceneConfig
	engine          *ctxengine.Engine
	assembled       *scene.AssembleResult
	runnable        compose.Runnable[[]*schema.Message, *schema.Message]
	store           session.Store
	checkpointStore hitl.CheckPointStore
}

func newSubagentTool(
	ctx context.Context,
	key string,
	childCfg *scene.SceneConfig,
	store session.Store,
) (*subagentTool, error) {
	if childCfg == nil {
		return nil, fmt.Errorf("subagent %q: nil scene config", key)
	}
	// 浅拷贝后置空 Subagents：阻断嵌套派生，且不改调用方持有的原配置
	child := *childCfg
	child.Subagents = nil

	checkpointStore := hitl.DefaultCheckPointStore()
	engine, assembled, r, err := buildRuntime(ctx, &child, nil, checkpointStore)
	if err != nil {
		return nil, err
	}

	desc := child.Name
	if desc == "" {
		desc = key
	}
	return &subagentTool{
		name:            "spawn_" + key,
		key:             key,
		desc:            desc,
		cfg:             &child,
		engine:          engine,
		assembled:       assembled,
		runnable:        r,
		store:           store,
		checkpointStore: checkpointStore,
	}, nil
}

func sortedSubagentKeys(subagents map[string]*scene.SceneConfig) []string {
	keys := make([]string, 0, len(subagents))
	for k := range subagents {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

type subagentArgs struct {
	Task          string `json:"task"`
	SessionSuffix string `json:"session_suffix,omitempty"`
}

func (t *subagentTool) Info(_ context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: t.name,
		Desc: fmt.Sprintf("派生子代理「%s」同步执行一个独立子任务并返回其结果。子代理拥有独立会话与工具链，适合隔离的长尾子任务；下发后等待其完成。", t.desc),
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"task": {
				Type:     schema.String,
				Desc:     "下发给子代理的完整任务描述（子代理看不到父会话上下文，须自包含）",
				Required: true,
			},
			"session_suffix": {
				Type:     schema.String,
				Desc:     "可选的会话后缀：同轮内并发派生多个同类子代理时用于区分会话，避免检查点冲突",
				Required: false,
			},
		}),
	}, nil
}

func (t *subagentTool) InvokableRun(
	ctx context.Context,
	argumentsInJSON string,
	_ ...einotool.Option,
) (string, error) {
	scope, ok := projection.TurnScopeFrom(ctx)
	if !ok || scope.SessionID == "" {
		return "", fmt.Errorf("%s: turn scope unavailable (tool must run inside an Agent turn)", t.name)
	}
	var args subagentArgs
	if err := json.Unmarshal([]byte(argumentsInJSON), &args); err != nil {
		return "", fmt.Errorf("invalid %s arguments: %w", t.name, err)
	}
	if strings.TrimSpace(args.Task) == "" {
		return "", fmt.Errorf("%s: task is required", t.name)
	}

	childSession := scope.SessionID + "::" + t.key
	if suffix := strings.TrimSpace(args.SessionSuffix); suffix != "" {
		childSession += "::" + suffix
	}

	child := &Agent{
		cfg:             t.cfg,
		runnable:        t.runnable,
		store:           t.store,
		checkpointStore: t.checkpointStore,
		engine:          t.engine,
		assembled:       t.assembled,
	}
	out, err := child.run(ctx, childSession, args.Task, false, nil)
	if err != nil {
		return "", fmt.Errorf("%s: %w", t.name, err)
	}
	return out.Content, nil
}
