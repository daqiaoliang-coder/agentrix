package tool

import (
	"context"
	"errors"
	"fmt"
	"time"

	einotool "github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

// TimeoutWrapper 为单个工具调用设置超时上限。
//
// 与模型调用超时的区别：模型超时由 BudgetModel 处理（重试/降级），
// 工具超时只约束单次 InvokableRun，不重试——工具是否可重放由业务决定，
// 框架层盲目重试可能造成重复写。
//
// 超时后向模型返回说明性文本而不是 error：让模型看到「慢/卡住」的事实并
// 自行换方案，而不是让整个 Turn 失败。调用方 ctx 被取消（客户端断开）
// 时仍返回 ctx 错误，交给上层终止执行。
type TimeoutWrapper struct {
	inner   einotool.InvokableTool
	name    string
	timeout time.Duration
}

// NewTimeoutWrapper 包装 inner。timeout <= 0 时由调用方跳过包装。
func NewTimeoutWrapper(ctx context.Context, inner einotool.BaseTool, timeout time.Duration) (*TimeoutWrapper, error) {
	invokable, ok := inner.(einotool.InvokableTool)
	if !ok {
		return nil, fmt.Errorf("timeout wrapper: tool %T does not implement InvokableTool", inner)
	}
	info, err := inner.Info(ctx)
	if err != nil {
		return nil, fmt.Errorf("timeout wrapper: read tool info: %w", err)
	}
	return &TimeoutWrapper{inner: invokable, name: info.Name, timeout: timeout}, nil
}

// Info 透传被包装工具的元信息。
func (w *TimeoutWrapper) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return w.inner.Info(ctx)
}

// InvokableRun 在独立 goroutine 中执行工具并以超时 ctx 兜底。
// 工具忽略 ctx 时该 goroutine 会滞留到工具自然返回，channel 带缓冲避免泄漏。
func (w *TimeoutWrapper) InvokableRun(
	ctx context.Context,
	argumentsInJSON string,
	opts ...einotool.Option,
) (string, error) {
	runCtx, cancel := context.WithTimeout(ctx, w.timeout)
	defer cancel()

	type result struct {
		out string
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := w.inner.InvokableRun(runCtx, argumentsInJSON, opts...)
		done <- result{out: out, err: err}
	}()

	select {
	case r := <-done:
		return r.out, r.err
	case <-runCtx.Done():
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			return fmt.Sprintf(
				"工具 %s 执行超时（上限 %s），未返回结果。不要原样重试；请简化参数、拆分任务或改用其他方案。",
				w.name, w.timeout,
			), nil
		}
		return "", runCtx.Err()
	}
}

// WrapTimeouts 为工具集统一套上超时包装。timeout <= 0 时原样返回。
func WrapTimeouts(ctx context.Context, tools []einotool.BaseTool, timeout time.Duration) ([]einotool.BaseTool, error) {
	if timeout <= 0 {
		return tools, nil
	}
	out := make([]einotool.BaseTool, 0, len(tools))
	for _, t := range tools {
		if t == nil {
			continue
		}
		wrapped, err := NewTimeoutWrapper(ctx, t, timeout)
		if err != nil {
			return nil, err
		}
		out = append(out, wrapped)
	}
	return out, nil
}
