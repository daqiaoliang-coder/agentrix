package hitl

import (
	"context"
	"fmt"
	"time"

	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
)

// compose.StatefulInterrupt	中断并保存组件内部状态到 Checkpoint
// compose.ExtractInterruptInfo	从 error 中提取所有中断点信息
// compose.ResumeWithData	携带数据恢复指定中断点
// compose.Resume	隐式恢复所有中断点
// compose.GetResumeContext[T]	在恢复的组件中获取 Resume 数据

// ApprovalRequest 是中断时传给外部系统的信息
type ApprovalRequest struct {
	ToolName  string    `json:"tool_name"`
	Arguments string    `json:"arguments"`
	Reason    string    `json:"reason"`
	Timestamp time.Time `json:"timestamp"`
}

// ApprovalDecision 是人工审批的决策结果
type ApprovalDecision struct {
	Approved bool   `json:"approved"`
	Comment  string `json:"comment"`
	Operator string `json:"operator"`
}

// ApprovalState 是中断时需要持久化的组件内部状态
type ApprovalState struct {
	PendingToolCallID string `json:"pending_tool_call_id"`
	ToolName          string `json:"tool_name"`
}

// RequestApproval 在工具内部调用，触发中断并保存 Checkpoint。
// 调用后 Eino 引擎会暂停 Graph 执行，返回 InterruptInfo 给上层。
//
// 使用方式：在需要人工审批的 Tool.InvokableRun 中调用
//
//	func (t *WriteTool) InvokableRun(ctx context.Context, args string, opts ...tool.Option) (string, error) {
//	    if err := hitl.RequestApproval(ctx, t.Name(), args, "写操作需要人工审批"); err != nil {
//	        return "", err
//	    }
//	    // 审批通过后的正常执行逻辑
//	    return t.doWrite(ctx, args)
//	}
func RequestApproval(ctx context.Context, toolName, args, reason string) error {
	req := &ApprovalRequest{
		ToolName:  toolName,
		Arguments: args,
		Reason:    reason,
		Timestamp: time.Now(),
	}
	state := &ApprovalState{
		ToolName: toolName,
	}
	// StatefulInterrupt 同时保存 info 和组件内部 state
	return compose.StatefulInterrupt(ctx, req, state)
}

// IsResume 检查当前是否处于恢复模式
func IsResume(ctx context.Context) bool {
	isResumeFlow, _, _ := compose.GetResumeContext[any](ctx)
	return isResumeFlow
}

// GetApprovalDecision 在恢复时从 ResumeContext 中获取人工决策
func GetApprovalDecision(ctx context.Context) (*ApprovalDecision, error) {
	_, hasData, decision := compose.GetResumeContext[*ApprovalDecision](ctx)
	if !hasData {
		return nil, fmt.Errorf("no resume decision found in context")
	}
	if decision == nil {
		return nil, fmt.Errorf("resume decision is nil")
	}
	return decision, nil
}

// ExtractInterruptInfo 从 Invoke 返回的 error 中提取中断信息
func ExtractInterruptInfo(err error) (*compose.InterruptInfo, bool) {
	return compose.ExtractInterruptInfo(err)
}

// ResumeWithDecision 构造恢复上下文，携带人工决策继续执行
//
// 使用方式：上层应用收到中断后，等待人工决策，然后调用
//
//	resumeCtx := hitl.ResumeWithDecision(ctx, interruptID, &hitl.ApprovalDecision{
//	    Approved: true,
//	    Operator: "admin",
//	})
//	output, err := agent.Run(resumeCtx, sessionID, "")
func ResumeWithDecision(
	ctx context.Context,
	interruptID string,
	decision *ApprovalDecision,
) context.Context {
	return compose.ResumeWithData(ctx, interruptID, decision)
}

// ResumeAll 隐式恢复所有中断点（单个"继续"按钮场景）
func ResumeAll(ctx context.Context) context.Context {
	return compose.Resume(ctx)
}

// 确保 schema 被使用（避免 import 未使用）
var _ = schema.Message{}
