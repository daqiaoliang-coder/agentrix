package core

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"

	"github.com/daqiaoliang-coder/agentrix/internal/projection"
	"github.com/daqiaoliang-coder/agentrix/internal/scene"
	"github.com/daqiaoliang-coder/agentrix/internal/session"
	agenttool "github.com/daqiaoliang-coder/agentrix/internal/tool"
)

// signalRecorder 收集 sink 推出的信号（图回调可能并发，须加锁）。
type signalRecorder struct {
	mu   sync.Mutex
	sigs []projection.Signal
}

func (r *signalRecorder) sink() func(projection.Signal) {
	return func(s projection.Signal) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.sigs = append(r.sigs, s)
	}
}

func (r *signalRecorder) types() []projection.SignalType {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]projection.SignalType, 0, len(r.sigs))
	for _, s := range r.sigs {
		out = append(out, s.Type)
	}
	return out
}

// indexOf 返回某信号类型首次出现的位置，未出现返回 -1。
func indexOf(types []projection.SignalType, typ projection.SignalType) int {
	for i, t := range types {
		if t == typ {
			return i
		}
	}
	return -1
}

// TestRunStreamEmitsLifecycleSignals 验证流式路径发射完整的 Turn 生命周期信号，
// 且信号顺序符合执行事实：TurnStart → 模型/工具交互 → TurnEnd。
func TestRunStreamEmitsLifecycleSignals(t *testing.T) {
	ctx := context.Background()
	m := &scriptModel{replies: []*schema.Message{
		toolCallMsg("list_draft", `{}`),
		schema.AssistantMessage("已完成", nil),
	}}
	agent, err := NewAgent(ctx, &scene.SceneConfig{
		Key: "wbs", Model: m,
		Tools:         []tool.BaseTool{&fakeReadTool{name: "list_draft"}},
		MaxIterations: 4,
	}, session.NewMemoryStore())
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	rec := &signalRecorder{}
	out, err := agent.RunStream(ctx, "sess-stream", "列出草稿", rec.sink())
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}
	if out == nil || out.Content != "已完成" {
		t.Fatalf("流式路径最终输出应与同步一致，实际 %+v", out)
	}

	types := rec.types()
	for _, want := range []projection.SignalType{
		projection.TurnStart, projection.LLMRequesting, projection.ToolStart,
		projection.ToolEnd, projection.TurnEnd,
	} {
		if indexOf(types, want) < 0 {
			t.Errorf("缺少信号 %s，实际序列 %v", want, types)
		}
	}
	// 顺序约束：TurnStart 最先，TurnEnd 最后，ToolStart 先于 ToolEnd
	if len(types) == 0 || types[0] != projection.TurnStart {
		t.Errorf("首信号应为 turn_start，实际 %v", types)
	}
	if types[len(types)-1] != projection.TurnEnd {
		t.Errorf("尾信号应为 turn_end，实际 %v", types)
	}
	if i, j := indexOf(types, projection.ToolStart), indexOf(types, projection.ToolEnd); i >= j {
		t.Errorf("tool_start(%d) 应先于 tool_end(%d)", i, j)
	}

	// 信号归属：ThreadID 应为会话 ID，Payload 携带工具名
	for _, s := range rec.sigs {
		if s.ThreadID != "sess-stream" {
			t.Errorf("ThreadID 应为 sess-stream，实际 %q", s.ThreadID)
		}
		if s.Type == projection.ToolStart {
			payload, _ := s.Payload.(map[string]any)
			if payload["tool"] != "list_draft" {
				t.Errorf("tool_start 应携带工具名 list_draft，实际 %v", payload)
			}
		}
	}
}

// TestRunStreamApprovalEmitsApproveRequested 验证审批中断经流式路径
// 发射 approve_requested 信号，且错误语义与同步路径一致。
func TestRunStreamApprovalEmitsApproveRequested(t *testing.T) {
	ctx := context.Background()
	catalog := agenttool.NewCatalog()
	mustRegisterSpec(t, catalog, agenttool.CommandSpec{
		ToolName: "edit_draft", Resource: "wbs-arrangement", Command: "edit-draft",
		Exec: agenttool.ExecInProcess, NeedsApproval: true, ApprovalReason: "修改排期需人工授权",
	})
	writeTool := &fakeWriteTool{name: "edit_draft"}
	m := &scriptModel{replies: []*schema.Message{toolCallMsg("edit_draft", `{"payload":"x"}`)}}

	agent, err := NewAgent(ctx, &scene.SceneConfig{
		Key: "wbs", Model: m, Tools: []tool.BaseTool{writeTool},
		Catalog: catalog, MaxIterations: 3,
	}, session.NewMemoryStore())
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	rec := &signalRecorder{}
	_, runErr := agent.RunStream(ctx, "sess-approval-stream", "改一下", rec.sink())
	if runErr == nil {
		t.Fatal("应触发审批中断")
	}
	if _, ok := ExtractApprovalRequired(runErr); !ok {
		t.Fatalf("错误语义应与同步路径一致（可提取审批请求），err=%v", runErr)
	}
	if indexOf(rec.types(), projection.ApproveRequested) < 0 {
		t.Errorf("缺少 approve_requested 信号，实际序列 %v", rec.types())
	}
	if writeTool.ran != 0 {
		t.Errorf("未授权时写工具不应执行，实际 %d 次", writeTool.ran)
	}
}

// TestRunStreamEmitsLLMTokens 验证 token 级流式：模型增量帧逐帧发射 llm_token，
// 全部 delta 依序拼接应等于最终输出，且 token 信号位于请求与结束信号之间。
func TestRunStreamEmitsLLMTokens(t *testing.T) {
	ctx := context.Background()
	const reply = "你好，世界！这是流式输出。"
	m := &scriptModel{replies: []*schema.Message{schema.AssistantMessage(reply, nil)}}
	agent, err := NewAgent(ctx, &scene.SceneConfig{
		Key: "plain", Model: m, MaxIterations: 2,
	}, session.NewMemoryStore())
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	rec := &signalRecorder{}
	out, err := agent.RunStream(ctx, "sess-token", "打个招呼", rec.sink())
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}
	if out == nil || out.Content != reply {
		t.Fatalf("流式拼接后的最终输出应与完整回复一致，实际 %+v", out)
	}

	// 收集 llm_token 增量，依序拼接应还原完整回复
	var deltas []string
	for _, s := range rec.sigs {
		if s.Type != projection.LLMToken {
			continue
		}
		payload, _ := s.Payload.(map[string]any)
		deltas = append(deltas, payload["delta"].(string))
	}
	if len(deltas) < 2 {
		t.Fatalf("长回复应切为多帧产生多个 llm_token，实际 %d 个", len(deltas))
	}
	if joined := strings.Join(deltas, ""); joined != reply {
		t.Errorf("delta 拼接应还原回复：得 %q，期望 %q", joined, reply)
	}

	// 位置约束：llm_token 夹在 llm_requesting 与 turn_end 之间
	types := rec.types()
	first, last := indexOf(types, projection.LLMToken), -1
	for i, typ := range types {
		if typ == projection.LLMToken {
			last = i
		}
	}
	if first <= indexOf(types, projection.LLMRequesting) {
		t.Errorf("llm_token 应在 llm_requesting 之后，序列 %v", types)
	}
	if last >= len(types)-1 || types[len(types)-1] != projection.TurnEnd {
		t.Errorf("llm_token 应在 turn_end 之前，序列 %v", types)
	}
}

// TestRunWithoutSinkEmitsNothing 验证同步路径（sink=nil）信号发射零成本且不 panic。
func TestRunWithoutSinkEmitsNothing(t *testing.T) {
	ctx := context.Background()
	m := &scriptModel{replies: []*schema.Message{schema.AssistantMessage("ok", nil)}}
	agent, err := NewAgent(ctx, &scene.SceneConfig{
		Key: "plain", Model: m, MaxIterations: 2,
	}, session.NewMemoryStore())
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	out, err := agent.Run(ctx, "sess-sync", "hi")
	if err != nil || out == nil || !strings.Contains(out.Content, "ok") {
		t.Fatalf("同步路径行为不应受信号接线影响: out=%v err=%v", out, err)
	}
}
