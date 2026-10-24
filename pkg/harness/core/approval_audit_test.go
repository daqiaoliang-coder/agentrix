package core

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"

	"github.com/daqiaoliang-coder/agentrix/pkg/harness/hitl"
	"github.com/daqiaoliang-coder/agentrix/pkg/scene"
	"github.com/daqiaoliang-coder/agentrix/pkg/session"
	agenttool "github.com/daqiaoliang-coder/agentrix/pkg/tool"
)

func useAuditStore(t *testing.T) *hitl.MemoryApprovalAuditStore {
	t.Helper()
	previous := hitl.DefaultApprovalAuditStore()
	store := hitl.NewMemoryApprovalAuditStore()
	hitl.SetDefaultApprovalAuditStore(store)
	t.Cleanup(func() { hitl.SetDefaultApprovalAuditStore(previous) })
	return store
}

func TestApprovalDecisionIsAudited(t *testing.T) {
	ctx := context.Background()
	useCheckpointStore(t)
	audits := useAuditStore(t)
	catalog := wbsCatalog(t)
	writeTool := &fakeWriteTool{name: "edit_draft"}
	store := session.NewMemoryStore()

	newCfg := func(m model.ToolCallingChatModel) *scene.SceneConfig {
		return &scene.SceneConfig{
			Key: "wbs", Model: m, Tools: []tool.BaseTool{writeTool},
			Catalog: catalog, MaxIterations: 3,
		}
	}

	m1 := &scriptModel{replies: []*schema.Message{
		toolCallMsg("edit_draft", `{"payload":"shift 5 days"}`),
	}}
	agent1, err := NewAgent(ctx, newCfg(m1), store)
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	_, runErr := agent1.Run(ctx, "sess-audit", "把选中两行各后移5天")
	approval, ok := ExtractApprovalRequired(runErr)
	if !ok {
		t.Fatalf("未识别为审批中断，err=%v", runErr)
	}

	rec, ok := audits.Get(approval.InterruptID)
	if !ok {
		t.Fatal("审批中断后应写入审计记录")
	}
	if rec.Status != hitl.ApprovalPending || rec.ToolName != "edit_draft" ||
		rec.Scene != "wbs" || rec.SessionID != "sess-audit" || rec.TurnID == "" {
		t.Errorf("pending 审计记录不完整: %+v", rec)
	}
	if !strings.Contains(rec.Arguments, "shift 5 days") {
		t.Errorf("审计缺少工具参数原文: %q", rec.Arguments)
	}

	m2 := &scriptModel{replies: []*schema.Message{schema.AssistantMessage("已完成", nil)}}
	agent2, err := NewAgent(ctx, newCfg(m2), store)
	if err != nil {
		t.Fatalf("NewAgent(resume): %v", err)
	}
	if _, err := agent2.Resume(ctx, "sess-audit", approval.InterruptID,
		&hitl.ApprovalDecision{Approved: true, Operator: "alice", Comment: "变更窗口内"}); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	rec, _ = audits.Get(approval.InterruptID)
	if rec.Status != hitl.ApprovalApproved || rec.Operator != "alice" ||
		rec.Comment != "变更窗口内" || rec.DecidedAt.IsZero() {
		t.Errorf("决策审计记录不正确: %+v", rec)
	}
}

func TestParamPolicyDeniesBeforeApproval(t *testing.T) {
	ctx := context.Background()
	useCheckpointStore(t)
	catalog := wbsCatalog(t)
	writeTool := &fakeWriteTool{name: "edit_draft"}
	store := session.NewMemoryStore()

	cfg := &scene.SceneConfig{
		Key: "wbs",
		Model: &scriptModel{replies: []*schema.Message{
			toolCallMsg("edit_draft", `{"payload":"forbidden"}`),
			schema.AssistantMessage("已取消该写入", nil),
		}},
		Tools:   []tool.BaseTool{writeTool},
		Catalog: catalog,
		ParamPolicies: []agenttool.ParamRule{{
			Tool: "edit_draft", Argument: "payload",
			Operator: agenttool.PolicyOpEQ, Value: "forbidden",
			Message: "该内容禁止写入",
		}},
		MaxIterations: 3,
	}
	agent, err := NewAgent(ctx, cfg, store)
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	out, err := agent.Run(ctx, "sess-policy", "写入 forbidden")
	if err != nil {
		t.Fatalf("策略拒绝不应中断 Turn（也不应进入审批）: %v", err)
	}
	if writeTool.ran != 0 {
		t.Errorf("命中参数策略后写工具仍被执行 %d 次", writeTool.ran)
	}
	if out == nil || !strings.Contains(out.Content, "已取消") {
		t.Errorf("未返回模型收尾输出: %+v", out)
	}
}
