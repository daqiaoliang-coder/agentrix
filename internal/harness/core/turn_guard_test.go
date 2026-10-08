package core

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"

	"github.com/daqiaoliang-coder/agentrix/internal/harness/hitl"
	"github.com/daqiaoliang-coder/agentrix/internal/scene"
	"github.com/daqiaoliang-coder/agentrix/internal/session"
	agenttool "github.com/daqiaoliang-coder/agentrix/internal/tool"
)

// blockingWriteTool 执行时阻塞直到 release 关闭或 ctx 取消，用于构造确定性的并发窗口。
type blockingWriteTool struct {
	name        string
	started     chan struct{}
	release     chan struct{}
	startedOnce sync.Once
	ran         atomic.Int32
}

func newBlockingWriteTool(name string) *blockingWriteTool {
	return &blockingWriteTool{name: name, started: make(chan struct{}), release: make(chan struct{})}
}

func (t *blockingWriteTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return (&fakeWriteTool{name: t.name}).Info(ctx)
}

func (t *blockingWriteTool) InvokableRun(ctx context.Context, _ string, _ ...tool.Option) (string, error) {
	t.ran.Add(1)
	t.startedOnce.Do(func() { close(t.started) })
	select {
	case <-t.release:
		return `{"success":1}`, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// flakyAppendStore 在 failAppend 置位时让 AppendHistory 失败，模拟写工具执行后的存储故障。
type flakyAppendStore struct {
	*session.MemoryStore
	failAppend atomic.Bool
}

func (s *flakyAppendStore) AppendHistory(ctx context.Context, sessionID string, msgs ...*schema.Message) (int64, error) {
	if s.failAppend.Load() {
		return 0, errors.New("storage unavailable")
	}
	return s.MemoryStore.AppendHistory(ctx, sessionID, msgs...)
}

func newApprovalAgent(t *testing.T, m model.ToolCallingChatModel, store session.Store, catalog *agenttool.Catalog, tools ...tool.BaseTool) *Agent {
	t.Helper()
	agent, err := NewAgent(context.Background(), &scene.SceneConfig{
		Key: "wbs", Model: m, Tools: tools, Catalog: catalog, MaxIterations: 4,
	}, store)
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	return agent
}

func interruptOnce(t *testing.T, agent *Agent, sessionID string) *ErrApprovalRequired {
	t.Helper()
	_, err := agent.Run(context.Background(), sessionID, "把选中两行各后移5天")
	approval, ok := ExtractApprovalRequired(err)
	if !ok {
		t.Fatalf("未触发审批中断: %v", err)
	}
	return approval
}

func approve() *hitl.ApprovalDecision {
	return &hitl.ApprovalDecision{Approved: true, Operator: "planner"}
}

func TestConcurrentResumeExecutesWriteOnce(t *testing.T) {
	ctx := context.Background()
	useCheckpointStore(t)
	catalog := wbsCatalog(t)
	writeTool := newBlockingWriteTool("edit_draft")
	store := session.NewMemoryStore()
	const sid = "sess-concurrent-resume"

	approval := interruptOnce(t, newApprovalAgent(t,
		&scriptModel{replies: []*schema.Message{toolCallMsg("edit_draft", `{"payload":"x"}`)}},
		store, catalog, writeTool), sid)

	first := newApprovalAgent(t, &scriptModel{replies: []*schema.Message{schema.AssistantMessage("完成", nil)}},
		store, catalog, writeTool)
	firstErr := make(chan error, 1)
	go func() {
		_, err := first.Resume(ctx, sid, approval.InterruptID, approve())
		firstErr <- err
	}()
	<-writeTool.started

	second := newApprovalAgent(t, &scriptModel{}, store, catalog, writeTool)
	if _, err := second.Resume(ctx, sid, approval.InterruptID, approve()); !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("并发 Resume 应返回 ErrSessionBusy，实际: %v", err)
	}
	if _, err := second.Run(ctx, sid, "插一句"); !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("执行中的会话上新 Run 应返回 ErrSessionBusy，实际: %v", err)
	}

	close(writeTool.release)
	if err := <-firstErr; err != nil {
		t.Fatalf("首个 Resume 应成功: %v", err)
	}
	if got := writeTool.ran.Load(); got != 1 {
		t.Fatalf("写工具应只执行 1 次，实际 %d 次", got)
	}
	if _, err := second.Resume(ctx, sid, approval.InterruptID, approve()); !errors.Is(err, ErrNoPendingApproval) {
		t.Fatalf("审批完成后再 Resume 应返回 ErrNoPendingApproval，实际: %v", err)
	}
}

func TestInterruptPersistsSuspendedStateAndHistoryWithoutDuplicates(t *testing.T) {
	ctx := context.Background()
	useCheckpointStore(t)
	catalog := wbsCatalog(t)
	mustRegisterSpec(t, catalog, agenttool.CommandSpec{
		ToolName: "publish_draft", Resource: "wbs-arrangement", Command: "publish-draft",
		Exec: agenttool.ExecInProcess, NeedsApproval: true, ApprovalReason: "发布排期需人工授权",
	})
	editTool := &fakeWriteTool{name: "edit_draft"}
	publishTool := &fakeWriteTool{name: "publish_draft"}
	store := session.NewMemoryStore()
	const sid = "sess-suspend-history"
	newAgent := func(replies ...*schema.Message) *Agent {
		return newApprovalAgent(t, &scriptModel{replies: replies}, store, catalog, editTool, publishTool)
	}

	first := interruptOnce(t, newAgent(toolCallMsg("edit_draft", `{"payload":"edit"}`)), sid)

	state, _ := store.LoadState(ctx, sid)
	hs := state.HitlState
	if hs.Status != session.HitlSuspended || hs.InterruptID != first.InterruptID {
		t.Fatalf("中断后应为 SUSPENDED 且记录 interrupt_id: %+v", hs)
	}
	if hs.SuspendedToolCall == nil || hs.SuspendedToolCall.ToolName != "edit_draft" ||
		hs.SuspendedToolCall.CallID != "call_edit_draft" || hs.SuspendedToolCall.Arguments != `{"payload":"edit"}` {
		t.Fatalf("挂起调用快照不正确: %+v", hs.SuspendedToolCall)
	}
	history, _ := store.LoadHistory(ctx, sid)
	assertRoles(t, history, schema.User, schema.Assistant)

	_, err := newAgent(toolCallMsg("publish_draft", `{"payload":"publish"}`)).
		Resume(ctx, sid, first.InterruptID, approve())
	second, ok := ExtractApprovalRequired(err)
	if !ok {
		t.Fatalf("第二次审批中断未识别: %v", err)
	}
	state, _ = store.LoadState(ctx, sid)
	if state.HitlState.Status != session.HitlSuspended || state.HitlState.InterruptID != second.InterruptID {
		t.Fatalf("二次中断后应以新 interrupt_id 挂起: %+v", state.HitlState)
	}
	history, _ = store.LoadHistory(ctx, sid)
	assertRoles(t, history, schema.User, schema.Assistant, schema.Tool, schema.Assistant)

	if _, err := newAgent(schema.AssistantMessage("已编辑并发布", nil)).
		Resume(ctx, sid, second.InterruptID, approve()); err != nil {
		t.Fatalf("二次审批恢复失败: %v", err)
	}
	history, _ = store.LoadHistory(ctx, sid)
	assertRoles(t, history, schema.User, schema.Assistant, schema.Tool, schema.Assistant, schema.Tool, schema.Assistant)
	if history[1].ToolCalls[0].Function.Name != "edit_draft" || history[3].ToolCalls[0].Function.Name != "publish_draft" {
		t.Fatalf("工具交互顺序不正确: %v / %v", history[1].ToolCalls, history[3].ToolCalls)
	}
	state, _ = store.LoadState(ctx, sid)
	if state.HitlState.Status != session.HitlIdle || state.HitlState.InterruptID != "" {
		t.Fatalf("终态后 HitlState 应回到 IDLE: %+v", state.HitlState)
	}
	if editTool.ran != 1 || publishTool.ran != 1 {
		t.Fatalf("工具执行次数不正确: edit=%d publish=%d", editTool.ran, publishTool.ran)
	}
}

func TestResumeRejectsMismatchedInterruptID(t *testing.T) {
	ctx := context.Background()
	useCheckpointStore(t)
	catalog := wbsCatalog(t)
	writeTool := &fakeWriteTool{name: "edit_draft"}
	store := session.NewMemoryStore()
	const sid = "sess-mismatch"

	approval := interruptOnce(t, newApprovalAgent(t,
		&scriptModel{replies: []*schema.Message{toolCallMsg("edit_draft", `{"payload":"x"}`)}},
		store, catalog, writeTool), sid)

	agent := newApprovalAgent(t, &scriptModel{replies: []*schema.Message{schema.AssistantMessage("ok", nil)}},
		store, catalog, writeTool)
	if _, err := agent.Resume(ctx, sid, "bogus-interrupt", approve()); !errors.Is(err, ErrInterruptMismatch) {
		t.Fatalf("interrupt_id 不匹配应返回 ErrInterruptMismatch，实际: %v", err)
	}
	if writeTool.ran != 0 {
		t.Fatalf("interrupt_id 不匹配时写工具不得执行，实际 %d 次", writeTool.ran)
	}
	if _, err := agent.Resume(ctx, sid, approval.InterruptID, approve()); err != nil {
		t.Fatalf("不匹配被拒后，正确 interrupt_id 仍应可恢复: %v", err)
	}
	if writeTool.ran != 1 {
		t.Fatalf("写工具应执行 1 次，实际 %d 次", writeTool.ran)
	}
}

func TestFailedResumeIsNotReplayedAndNewRunAbandonsApproval(t *testing.T) {
	ctx := context.Background()
	checkpointStore := useCheckpointStore(t)
	catalog := wbsCatalog(t)
	writeTool := &fakeWriteTool{name: "edit_draft"}
	store := &flakyAppendStore{MemoryStore: session.NewMemoryStore()}
	const sid = "sess-failed-resume"

	approval := interruptOnce(t, newApprovalAgent(t,
		&scriptModel{replies: []*schema.Message{toolCallMsg("edit_draft", `{"payload":"x"}`)}},
		store, catalog, writeTool), sid)

	// 写工具执行后存储故障：Resume 失败，但写操作已发生
	store.failAppend.Store(true)
	if _, err := newApprovalAgent(t, &scriptModel{}, store, catalog, writeTool).
		Resume(ctx, sid, approval.InterruptID, approve()); err == nil {
		t.Fatal("存储故障时 Resume 应失败")
	}
	store.failAppend.Store(false)
	if writeTool.ran != 1 {
		t.Fatalf("前置条件：写工具应已执行 1 次，实际 %d 次", writeTool.ran)
	}

	agent := newApprovalAgent(t, &scriptModel{replies: []*schema.Message{schema.AssistantMessage("新一轮", nil)}},
		store, catalog, writeTool)
	if _, err := agent.Resume(ctx, sid, approval.InterruptID, approve()); !errors.Is(err, ErrResumeInProgress) {
		t.Fatalf("已认领的审批重试应返回 ErrResumeInProgress，实际: %v", err)
	}
	if writeTool.ran != 1 {
		t.Fatalf("失败后重试不得重放写工具，实际执行 %d 次", writeTool.ran)
	}

	if _, err := agent.Run(ctx, sid, "继续"); err != nil {
		t.Fatalf("新 Turn 应放弃挂起审批并正常执行: %v", err)
	}
	if _, exists, _ := checkpointStore.Get(ctx, sid); exists {
		t.Fatal("放弃审批后 checkpoint 应被删除")
	}
	// 悬空 tool_call 须被终结结果闭合，否则后续模型调用违反协议
	history, _ := store.LoadHistory(ctx, sid)
	assertRoles(t, history, schema.User, schema.Assistant, schema.Tool, schema.User, schema.Assistant)
	if history[2].ToolCallID != "call_edit_draft" || !strings.Contains(history[2].Content, "结果未知") {
		t.Fatalf("放弃认领中的审批应以「结果未知」闭合 tool_call: %+v", history[2])
	}
	if _, err := agent.Resume(ctx, sid, approval.InterruptID, approve()); !errors.Is(err, ErrNoPendingApproval) {
		t.Fatalf("放弃后的审批不可再恢复，期望 ErrNoPendingApproval，实际: %v", err)
	}
}

// lossyLeaseStore 续约恒失败，模拟租约过期后被其他副本接管。
type lossyLeaseStore struct{ *session.MemoryStore }

func (lossyLeaseStore) RenewLease(context.Context, string, string, time.Duration) (bool, error) {
	return false, nil
}

func TestLeaseLossCancelsTurn(t *testing.T) {
	useCheckpointStore(t)
	previousTTL := sessionLeaseTTL
	sessionLeaseTTL = 30 * time.Millisecond
	t.Cleanup(func() { sessionLeaseTTL = previousTTL })

	slowTool := newBlockingWriteTool("slow_write")
	agent, err := NewAgent(context.Background(), &scene.SceneConfig{
		Key:           "lease",
		Model:         &scriptModel{replies: []*schema.Message{toolCallMsg("slow_write", `{"payload":"x"}`)}},
		Tools:         []tool.BaseTool{slowTool},
		MaxIterations: 3,
	}, lossyLeaseStore{session.NewMemoryStore()})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := agent.Run(context.Background(), "sess-lease-lost", "go")
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrSessionLeaseLost) {
			t.Fatalf("租约丢失应取消 Turn 并返回 ErrSessionLeaseLost，实际: %v", err)
		}
	case <-time.After(5 * time.Second):
		close(slowTool.release)
		t.Fatal("租约丢失后 Turn 未被取消")
	}
}

func assertRoles(t *testing.T, msgs []*schema.Message, want ...schema.RoleType) {
	t.Helper()
	got := make([]schema.RoleType, len(msgs))
	for i, m := range msgs {
		got[i] = m.Role
	}
	if len(got) != len(want) {
		t.Fatalf("历史角色序列 = %v，期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("历史角色序列 = %v，期望 %v", got, want)
		}
	}
}
