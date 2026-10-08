package core

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/cloudwego/eino/schema"

	"github.com/daqiaoliang-coder/agentrix/internal/harness/hitl"
	"github.com/daqiaoliang-coder/agentrix/internal/session"
)

var (
	// ErrSessionBusy 表示同一会话已有 Turn 在执行（租约被占用）。
	ErrSessionBusy = errors.New("session is busy")
	// ErrSessionLeaseLost 表示执行中租约续约失败或被接管，Turn 已被取消。
	ErrSessionLeaseLost = errors.New("session lease lost")
	// ErrNoPendingApproval 表示会话没有待恢复的审批（已完成、已放弃或从未中断）。
	ErrNoPendingApproval = errors.New("no pending approval")
	// ErrInterruptMismatch 表示 Resume 携带的 interrupt_id 与挂起中断点不符。
	ErrInterruptMismatch = errors.New("interrupt id mismatch")
	// ErrResumeInProgress 表示该审批已被认领恢复；写工具可能已执行，不允许重放，
	// 需发起新 Turn 继续。
	ErrResumeInProgress = errors.New("approval already claimed for resume")
)

// sessionLeaseTTL 为租约有效期，每 1/3 TTL 续约一次。测试可缩短。
var sessionLeaseTTL = 30 * time.Second

const leaseReleaseTimeout = 5 * time.Second

// processLeaser 兜底未实现 session.Leaser 的自定义 Store，仅保证单进程互斥。
var processLeaser session.Leaser = session.NewMemoryLeaser()

type resumeRequest struct {
	interruptID string
	decision    *hitl.ApprovalDecision
}

func (a *Agent) leaser() session.Leaser {
	if l, ok := a.store.(session.Leaser); ok {
		return l
	}
	return processLeaser
}

// holdSessionLease 获取会话租约并在后台续约。续约确认失去租约、或连续续约
// 失败超过一个 TTL 时以 ErrSessionLeaseLost 取消返回的 ctx，避免两个执行方
// 同时推进同一会话。release 停止续约并释放租约，须在 Turn 结束时调用。
func (a *Agent) holdSessionLease(ctx context.Context, sessionID string) (context.Context, func(), error) {
	leaser := a.leaser()
	owner, err := newLeaseOwner()
	if err != nil {
		return nil, nil, err
	}
	ttl := sessionLeaseTTL
	ok, err := leaser.AcquireLease(ctx, sessionID, owner, ttl)
	if err != nil {
		return nil, nil, fmt.Errorf("acquire session lease: %w", err)
	}
	if !ok {
		return nil, nil, fmt.Errorf("%w: %s", ErrSessionBusy, sessionID)
	}

	runCtx, cancel := context.WithCancelCause(ctx)
	stop := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(ttl / 3)
		defer ticker.Stop()
		lastRenew := time.Now()
		for {
			select {
			case <-stop:
				return
			case <-runCtx.Done():
				return
			case <-ticker.C:
			}
			held, err := leaser.RenewLease(runCtx, sessionID, owner, ttl)
			switch {
			case err == nil && held:
				lastRenew = time.Now()
			case err == nil:
				cancel(ErrSessionLeaseLost)
				return
			case time.Since(lastRenew) >= ttl:
				cancel(fmt.Errorf("%w: renew: %v", ErrSessionLeaseLost, err))
				return
			}
		}
	}()

	release := func() {
		close(stop)
		<-stopped
		cancel(nil)
		rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), leaseReleaseTimeout)
		defer rcancel()
		if err := leaser.ReleaseLease(rctx, sessionID, owner); err != nil {
			log.Printf("[lease] release %s failed (expires in %s): %v", sessionID, ttl, err)
		}
	}
	return runCtx, release, nil
}

// wrapLeaseLost 让租约丢失导致的 context canceled 可被 errors.Is(ErrSessionLeaseLost) 识别。
func wrapLeaseLost(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if cause := context.Cause(ctx); errors.Is(cause, ErrSessionLeaseLost) && !errors.Is(err, ErrSessionLeaseLost) {
		return fmt.Errorf("%w: %w", cause, err)
	}
	return err
}

func newLeaseOwner() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate lease owner: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// claimResume 校验挂起审批并认领：SUSPENDED 且 interrupt_id 匹配、检查点存在时，
// 置为 RESUMING 落库后才允许执行。MySQL 下 SaveState 的 CAS 是租约之外的第二道
// 互斥：并发认领只有一方能成功。
func (a *Agent) claimResume(ctx context.Context, sessionID string, state *session.State, interruptID string) error {
	switch state.HitlState.Status {
	case session.HitlSuspended:
	case session.HitlResuming:
		return fmt.Errorf("%w: %s", ErrResumeInProgress, sessionID)
	default:
		return fmt.Errorf("%w: %s", ErrNoPendingApproval, sessionID)
	}
	if state.HitlState.InterruptID != interruptID {
		return fmt.Errorf("%w: session %s", ErrInterruptMismatch, sessionID)
	}
	if err := a.requireCheckpoint(ctx, sessionID); err != nil {
		return err
	}
	state.HitlState.Status = session.HitlResuming
	if err := a.store.SaveState(ctx, sessionID, state); err != nil {
		return fmt.Errorf("claim resume: %w", err)
	}
	return nil
}

// abandonPendingApproval 在新 Turn 开始时放弃未完成的审批：用户已转向新输入，
// 旧审批若之后被恢复，会在过时的上下文里执行写操作。
//
// 中断时落盘的 assistant tool_call 此时没有配对结果，模型协议要求每个 tool_call
// 都有结果，故为未应答的调用补一条终结结果；这条记录同时是放弃审批的审计事实。
func (a *Agent) abandonPendingApproval(ctx context.Context, sessionID string, state *session.State) error {
	status := state.HitlState.Status
	if status == "" || status == session.HitlIdle {
		return nil
	}
	content := "[审批已放弃：用户发起了新一轮对话，该工具未执行]"
	if status == session.HitlResuming {
		content = "[审批恢复未完成：该工具可能已执行，结果未知，请先核实再继续操作]"
	}
	history, err := a.store.LoadHistory(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("abandon pending approval: load history: %w", err)
	}
	if closing := closeUnansweredToolCalls(history, content); len(closing) > 0 {
		lastSeq, err := a.store.AppendHistory(ctx, sessionID, closing...)
		if err != nil {
			return fmt.Errorf("abandon pending approval: append history: %w", err)
		}
		state.HistoryCursor = int(lastSeq)
	}
	state.HitlState = session.HitlState{Status: session.HitlIdle}
	if err := a.store.SaveState(ctx, sessionID, state); err != nil {
		return fmt.Errorf("abandon pending approval: %w", err)
	}
	a.discardCheckpoint(ctx, sessionID)
	return nil
}

// closeUnansweredToolCalls 为最后一条带 tool_calls 的 assistant 消息中未应答的调用生成终结结果。
func closeUnansweredToolCalls(history []*schema.Message, content string) []*schema.Message {
	for i := len(history) - 1; i >= 0; i-- {
		m := history[i]
		if m.Role != schema.Assistant || len(m.ToolCalls) == 0 {
			continue
		}
		answered := make(map[string]bool)
		for _, r := range history[i+1:] {
			if r.Role == schema.Tool {
				answered[r.ToolCallID] = true
			}
		}
		var out []*schema.Message
		for _, tc := range m.ToolCalls {
			if !answered[tc.ID] {
				msg := schema.ToolMessage(content, tc.ID)
				msg.ToolName = tc.Function.Name
				out = append(out, msg)
			}
		}
		return out
	}
	return nil
}

// suspend 在审批中断时落盘：本次执行已捕获的交互（含末尾等待审批的 assistant
// tool_call）追加进 RawHistory，工作记忆置为 SUSPENDED 并记录 interrupt_id 与
// 挂起调用快照。
//
// Resume 重跑 ToolsNode 时 eino 不再回放其输入消息（回调入参为 nil），
// 故末尾 tool_call 必须在此落盘，恢复后只追加结果，不会重复。
func (a *Agent) suspend(
	ctx context.Context,
	sessionID string,
	state *session.State,
	userInput string,
	resuming bool,
	captured []*schema.Message,
	approval *ErrApprovalRequired,
) error {
	var msgs []*schema.Message
	if !resuming {
		msgs = append(msgs, schema.UserMessage(userInput))
	}
	msgs = append(msgs, captured...)
	if len(msgs) > 0 {
		lastSeq, err := a.store.AppendHistory(ctx, sessionID, msgs...)
		if err != nil {
			return fmt.Errorf("append history on interrupt: %w", err)
		}
		state.HistoryCursor = int(lastSeq)
	}

	state.UpdateFromTurn()
	state.HitlState = session.HitlState{
		Status:      session.HitlSuspended,
		InterruptID: approval.InterruptID,
	}
	if req := approval.Request; req != nil {
		state.HitlState.SuspendedToolCall = &session.ToolCallSnapshot{
			ToolName:  req.ToolName,
			Arguments: req.Arguments,
			CallID:    req.ToolCallID,
		}
	}
	if err := a.store.SaveState(ctx, sessionID, state); err != nil {
		return fmt.Errorf("save state on interrupt: %w", err)
	}
	return nil
}

func (a *Agent) requireCheckpoint(ctx context.Context, sessionID string) error {
	_, ok, err := a.checkpointStore.Get(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("get checkpoint: %w", err)
	}
	if !ok {
		return fmt.Errorf("%w: %s", ErrCheckpointNotFound, sessionID)
	}
	return nil
}

// discardCheckpoint 尽力删除终态检查点。失败不影响 Turn 结果：HitlState 已不是
// SUSPENDED，残留检查点无法被 Resume 使用，新 Run 又以 ForceNewRun 忽略它。
func (a *Agent) discardCheckpoint(ctx context.Context, sessionID string) {
	deleter, ok := a.checkpointStore.(hitl.CheckPointDeleter)
	if !ok {
		log.Printf("[checkpoint] store %T does not support deletion; %s left behind", a.checkpointStore, sessionID)
		return
	}
	if err := deleter.Delete(ctx, sessionID); err != nil {
		log.Printf("[checkpoint] delete %s failed: %v", sessionID, err)
	}
}
