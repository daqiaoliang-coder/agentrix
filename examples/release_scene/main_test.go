package main

import (
	"context"
	"strings"
	"testing"

	einomodel "github.com/cloudwego/eino/components/model"
	einotool "github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"

	"github.com/daqiaoliang-coder/agentrix/internal/harness/core"
	"github.com/daqiaoliang-coder/agentrix/internal/harness/hitl"
	"github.com/daqiaoliang-coder/agentrix/internal/session"
)

// ── 脚本化假模型 ──
//
// 为什么用指针共享状态：BudgetModel.WithTools（model_decorator.go）会克隆底层模型，
// 若轮次计数器是值语义，克隆后归零，多轮脚本会从头重放导致错乱。
// eino 在绑定工具时会触发 WithTools，故所有克隆必须共享同一 *scriptState。

type scriptState struct {
	call      int
	replies   []*schema.Message
	seenTools [][]string // 每次 Generate 时模型实际绑定的工具名
	bound     []*schema.ToolInfo
}

type scriptModel struct{ st *scriptState }

func newScriptModel(replies ...*schema.Message) *scriptModel {
	return &scriptModel{st: &scriptState{replies: replies}}
}

func (m *scriptModel) Generate(_ context.Context, _ []*schema.Message, _ ...einomodel.Option) (*schema.Message, error) {
	names := make([]string, 0, len(m.st.bound))
	for _, info := range m.st.bound {
		names = append(names, info.Name)
	}
	m.st.seenTools = append(m.st.seenTools, names)

	if m.st.call >= len(m.st.replies) {
		// 脚本耗尽：返回无 ToolCall 的收尾消息，避免图无限循环
		return schema.AssistantMessage("（脚本结束）", nil), nil
	}
	out := m.st.replies[m.st.call]
	m.st.call++
	return out, nil
}

func (m *scriptModel) Stream(_ context.Context, _ []*schema.Message, _ ...einomodel.Option) (*schema.StreamReader[*schema.Message], error) {
	return nil, context.DeadlineExceeded
}

func (m *scriptModel) WithTools(tools []*schema.ToolInfo) (einomodel.ToolCallingChatModel, error) {
	// 关键：共享同一个 *scriptState，克隆不影响轮次计数
	clone := &scriptModel{st: m.st}
	clone.st.bound = tools
	return clone, nil
}

func toolCall(name, argsJSON string) *schema.Message {
	return &schema.Message{
		Role: schema.Assistant,
		ToolCalls: []schema.ToolCall{{
			ID:       "call_" + name,
			Function: schema.FunctionCall{Name: name, Arguments: argsJSON},
		}},
	}
}

// ── 测试 1：离线装配（不依赖模型，验证阶段 0 + 白名单）──

func TestReleaseSceneAssemble(t *testing.T) {
	ctx := context.Background()

	probe := NewSceneConfig(nil, DefaultTools())
	res, err := probe.Assemble(ctx)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}

	// 阶段 0：系统提示注入两个技能的 frontmatter
	for _, want := range []string{
		"meego-release-deploy",
		"meego-release-inspect",
		"read_skill",
	} {
		if !strings.Contains(res.SystemPrompt, want) {
			t.Errorf("系统提示缺少 %q", want)
		}
	}

	// 白名单过滤后的可见工具集
	visible := map[string]bool{}
	for _, tl := range res.Tools {
		info, err := tl.Info(ctx)
		if err != nil {
			t.Fatalf("tool info: %v", err)
		}
		visible[info.Name] = true
	}
	for _, want := range []string{"get_release_pipeline", "deploy_canary", "read_skill"} {
		if !visible[want] {
			t.Errorf("白名单内工具 %q 不可见，实际可见: %v", want, keys(visible))
		}
	}
	// deploy_full 已登记 Catalog 但本场景未放行 → 必须被过滤
	if visible["deploy_full"] {
		t.Errorf("白名单外的 deploy_full 未被过滤，实际可见: %v", keys(visible))
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// ── 测试 2：端到端（四阶段 + 授权放行）──
//
// 脚本驱动模型走完：读 SKILL.md → 读灰度细则 → 只读巡检 → 灰度发布(中断) → 授权 → 收尾。

func TestReleaseCanaryEndToEnd(t *testing.T) {
	ctx := context.Background()

	inspect := &GetReleasePipelineTool{}
	canary := &DeployCanaryTool{}
	full := &DeployFullTool{}
	tools := []einotool.BaseTool{inspect, canary, full}

	// 5 轮脚本：阶段1 读正文、阶段2 读细则、阶段3 只读巡检、阶段3 写(触发中断)、恢复后收尾
	m := newScriptModel(
		toolCall("read_skill", `{"name":"meego-release-deploy"}`),
		toolCall("read_skill", `{"name":"meego-release-deploy","reference":"canary_deploy_flow.md"}`),
		toolCall("get_release_pipeline", `{"change_id":"CHG-20260915-003"}`),
		toolCall("deploy_canary", `{"change_id":"CHG-20260915-003","ratio":10}`),
		schema.AssistantMessage("已将 CHG-20260915-003 灰度发布到 10% 流量，观察窗口 15 分钟。", nil),
	)

	cfg := NewSceneConfig(m, tools)
	store := session.NewMemoryStore()
	agent, err := core.NewAgent(ctx, cfg, store)
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	const sessionID = "release-e2e"
	_, runErr := agent.Run(ctx, sessionID, "把变更单 CHG-20260915-003 灰度发布到 10% 流量")

	// 阶段 3 写操作应中断等待授权
	approval, needApproval := core.ExtractApprovalRequired(runErr)
	if !needApproval {
		t.Fatalf("灰度发布未触发审批中断，err=%v", runErr)
	}
	if approval.Request == nil || approval.Request.ToolName != "deploy_canary" {
		t.Errorf("审批请求工具名不正确: %+v", approval.Request)
	}
	if approval.InterruptID == "" {
		t.Error("审批中断缺少 InterruptID")
	}

	// 中断时：只读巡检已执行，写操作尚未执行
	if inspect.Calls() != 1 {
		t.Errorf("只读巡检应已执行 1 次，实际 %d", inspect.Calls())
	}
	if canary.Calls() != 0 {
		t.Errorf("授权前灰度发布不得执行，实际 %d 次", canary.Calls())
	}
	if full.Calls() != 0 {
		t.Errorf("白名单外的全量上线不得执行，实际 %d 次", full.Calls())
	}

	// 授权放行 → 恢复执行
	out, err := agent.Resume(ctx, sessionID, approval.InterruptID,
		&hitl.ApprovalDecision{Approved: true, Operator: "release-manager", Comment: "变更窗口内"})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}

	if canary.Calls() != 1 {
		t.Errorf("授权后灰度发布应执行 1 次，实际 %d", canary.Calls())
	}
	if full.Calls() != 0 {
		t.Errorf("全量上线始终不得执行，实际 %d 次", full.Calls())
	}
	if out == nil || !strings.Contains(out.Content, "灰度发布") {
		t.Errorf("恢复后未返回预期收尾，得到: %+v", out)
	}

	// 白名单治理须真正作用到模型侧：任何一轮模型都不应看到 deploy_full
	for i, bound := range m.st.seenTools {
		for _, name := range bound {
			if name == "deploy_full" {
				t.Errorf("第 %d 轮模型仍看到白名单外的 deploy_full: %v", i, bound)
			}
		}
	}
}

// ── 测试 3：拒绝授权分支 ──

func TestReleaseCanaryRejectApproval(t *testing.T) {
	ctx := context.Background()

	inspect := &GetReleasePipelineTool{}
	canary := &DeployCanaryTool{}
	tools := []einotool.BaseTool{inspect, canary}

	m := newScriptModel(
		toolCall("get_release_pipeline", `{"change_id":"CHG-X"}`),
		toolCall("deploy_canary", `{"change_id":"CHG-X","ratio":50}`),
		schema.AssistantMessage("已取消发布。", nil),
	)

	cfg := NewSceneConfig(m, tools)
	agent, err := core.NewAgent(ctx, cfg, session.NewMemoryStore())
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	const sessionID = "release-reject"
	_, runErr := agent.Run(ctx, sessionID, "灰度发布到 50%")
	approval, needApproval := core.ExtractApprovalRequired(runErr)
	if !needApproval {
		t.Fatalf("未触发审批中断，err=%v", runErr)
	}

	// 拒绝：不应报错，且写工具不得执行
	if _, err := agent.Resume(ctx, sessionID, approval.InterruptID,
		&hitl.ApprovalDecision{Approved: false, Comment: "超出单次灰度上限", Operator: "release-manager"}); err != nil {
		t.Fatalf("拒绝授权不应返回错误（拒绝作为工具结果回给模型）: %v", err)
	}
	if canary.Calls() != 0 {
		t.Errorf("拒绝授权后灰度发布不得执行，实际 %d 次", canary.Calls())
	}
}
