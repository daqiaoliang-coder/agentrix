package tool

import (
	"context"
	"fmt"

	einotool "github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"

	"github.com/daqiaoliang-coder/agentrix/internal/harness/hitl"
)

// ApprovalWrapper 包装一个写类工具，在其执行前触发 HITL 人工审批。
//
// 对应 Skill 共性约束中的「写入授权」：修改排期、编辑草稿等写操作
// 必须先由人确认，模型不得自行完成授权。
//
// 执行时序：
//  1. 首次运行 → hitl.RequestApproval 中断，Graph 暂停并存 Checkpoint，
//     上层拿到 ApprovalRequest（工具名 + 参数 + 原因）展示给审批人。
//  2. 审批后 → 上层用 Agent.Resume 携带 ApprovalDecision 恢复。
//  3. 恢复运行 → 本包装器读到决策：通过则执行被包装工具，拒绝则返回拒绝说明给模型。
//
// 注意：被包装工具的 InvokableRun 会在恢复后从头执行（eino 的 checkpoint 恢复语义），
// 因此被包装工具自身应当幂等，或依赖外部幂等键（如 operation_id）。
type ApprovalWrapper struct {
	inner  einotool.InvokableTool
	reason string
}

// NewApprovalWrapper 用审批包装 inner。reason 为展示给审批人的说明，
// 留空时使用默认文案。inner 必须实现 InvokableTool（当前框架工具均满足）。
func NewApprovalWrapper(inner einotool.BaseTool, reason string) (*ApprovalWrapper, error) {
	invokable, ok := inner.(einotool.InvokableTool)
	if !ok {
		return nil, fmt.Errorf("approval wrapper: tool %T does not implement InvokableTool", inner)
	}
	if reason == "" {
		reason = "该操作会修改业务数据，需要人工确认"
	}
	return &ApprovalWrapper{inner: invokable, reason: reason}, nil
}

// Info 透传被包装工具的元信息，保持模型侧看到的名称与参数 schema 不变。
func (w *ApprovalWrapper) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return w.inner.Info(ctx)
}

// InvokableRun 先审批后执行。
func (w *ApprovalWrapper) InvokableRun(
	ctx context.Context,
	argumentsInJSON string,
	opts ...einotool.Option,
) (string, error) {
	info, err := w.inner.Info(ctx)
	if err != nil {
		return "", fmt.Errorf("approval wrapper: read tool info: %w", err)
	}

	// ① 恢复流程：读取人工决策
	if hitl.IsResume(ctx) {
		decision, err := hitl.GetApprovalDecision(ctx)
		if err != nil {
			return "", fmt.Errorf("approval wrapper: %w", err)
		}
		if !decision.Approved {
			// 拒绝不报错：把拒绝事实作为工具结果回给模型，让其调整方案而不是中断 Turn
			return fmt.Sprintf("操作被拒绝，未执行任何修改。审批意见：%s", emptyAs(decision.Comment, "无")), nil
		}
		return w.inner.InvokableRun(ctx, argumentsInJSON, opts...)
	}

	// ② 首次运行：触发中断等待审批
	if err := hitl.RequestApproval(ctx, info.Name, argumentsInJSON, w.reason); err != nil {
		return "", err
	}
	// RequestApproval 正常路径必返回非 nil error（中断信号）；
	// 若返回 nil 说明底层实现变更，保守处理为拒绝执行，避免未授权写入。
	return "", fmt.Errorf("approval wrapper: unexpected nil interrupt from %s", info.Name)
}

// WrapTools 按 Catalog 元数据为需要审批的工具套上 ApprovalWrapper。
// 未登记 Catalog 或 NeedsApproval=false 的工具原样返回。
func WrapTools(ctx context.Context, catalog *Catalog, tools []einotool.BaseTool) ([]einotool.BaseTool, error) {
	if catalog == nil {
		return tools, nil
	}
	out := make([]einotool.BaseTool, 0, len(tools))
	for _, t := range tools {
		info, err := t.Info(ctx)
		if err != nil {
			return nil, fmt.Errorf("wrap tools: read tool info: %w", err)
		}
		spec, ok := catalog.SpecOf(info.Name)
		if !ok || !spec.NeedsApproval {
			out = append(out, t)
			continue
		}
		wrapped, err := NewApprovalWrapper(t, spec.ApprovalReason)
		if err != nil {
			return nil, err
		}
		out = append(out, wrapped)
	}
	return out, nil
}

func emptyAs(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
