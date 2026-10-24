package hitl

import (
	"context"
	"testing"
)

func TestMemoryApprovalAuditLifecycle(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryApprovalAuditStore()

	if err := store.RecordRequest(ctx, &ApprovalAudit{
		InterruptID: "int-1",
		Scene:       "release",
		SessionID:   "alice::s1",
		TurnID:      "turn_1",
		ToolName:    "release",
		Arguments:   `{"ratio":30}`,
	}); err != nil {
		t.Fatalf("record request: %v", err)
	}
	rec, ok := store.Get("int-1")
	if !ok || rec.Status != ApprovalPending || rec.ToolName != "release" {
		t.Fatalf("pending 记录不正确: %+v ok=%v", rec, ok)
	}

	if err := store.RecordDecision(ctx, "int-1", "alice", "变更窗口内", true); err != nil {
		t.Fatalf("record decision: %v", err)
	}
	rec, _ = store.Get("int-1")
	if rec.Status != ApprovalApproved || rec.Operator != "alice" ||
		rec.Comment != "变更窗口内" || rec.DecidedAt.IsZero() {
		t.Errorf("approved 记录不正确: %+v", rec)
	}

	if err := store.RecordRequest(ctx, &ApprovalAudit{InterruptID: "int-2"}); err != nil {
		t.Fatalf("record request 2: %v", err)
	}
	if err := store.MarkAbandoned(ctx, "int-2"); err != nil {
		t.Fatalf("mark abandoned: %v", err)
	}
	if rec, _ = store.Get("int-2"); rec.Status != ApprovalAbandoned {
		t.Errorf("abandoned 状态不正确: %+v", rec)
	}

	// 已决策的记录不应被后续放弃覆盖。
	if err := store.MarkAbandoned(ctx, "int-1"); err != nil {
		t.Fatalf("mark approved abandoned: %v", err)
	}
	if rec, _ = store.Get("int-1"); rec.Status != ApprovalApproved {
		t.Errorf("approved 记录被错误改写: %+v", rec)
	}
}

func TestApprovalAuditRequiresInterruptID(t *testing.T) {
	store := NewMemoryApprovalAuditStore()
	if err := store.RecordRequest(context.Background(), &ApprovalAudit{}); err == nil {
		t.Fatal("缺少 interrupt_id 应报错")
	}
}
