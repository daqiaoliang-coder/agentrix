package core

import (
	"fmt"

	"github.com/daqiaoliang-coder/agentrix/internal/harness/hitl"
)

// ErrApprovalRequired 表示 Turn 因写操作等待人工授权而中断。
//
// 上层（HTTP handler / 前端）拿到它之后应当：
//  1. 把 Request 展示给审批人（工具名、参数、原因）；
//  2. 取得决策后调用 Agent.Resume(ctx, sessionID, InterruptID, decision) 继续。
//
// InterruptID 是恢复时的必填参数，由 eino 的中断点地址生成，必须原样回传。
type ErrApprovalRequired struct {
	// InterruptID 恢复该中断点所需的唯一标识。
	InterruptID string
	// Request 展示给审批人的请求信息。
	Request *hitl.ApprovalRequest
}

func (e *ErrApprovalRequired) Error() string {
	if e.Request != nil {
		return fmt.Sprintf("approval required for tool %q: %s", e.Request.ToolName, e.Request.Reason)
	}
	return "approval required"
}

// ExtractApprovalRequired 从 Run/Resume 返回的 error 中提取审批中断。
//
// eino 的中断以 error 形式冒泡（StatefulInterrupt 返回特殊 error），
// 其中 InterruptInfo.InterruptContexts 携带每个中断点的 ID 与 Info；
// Info 即 ApprovalWrapper 传入的 *hitl.ApprovalRequest。
//
// 非审批中断返回 nil, false，上层据此区分「等待授权」与「真实失败」。
func ExtractApprovalRequired(err error) (*ErrApprovalRequired, bool) {
	info, existed := hitl.ExtractInterruptInfo(err)
	if !existed || info == nil {
		return nil, false
	}
	for _, ictx := range info.InterruptContexts {
		if ictx == nil {
			continue
		}
		if req, ok := ictx.Info.(*hitl.ApprovalRequest); ok {
			return &ErrApprovalRequired{InterruptID: ictx.ID, Request: req}, true
		}
	}
	return nil, false
}
