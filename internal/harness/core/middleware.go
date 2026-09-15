package core

import (
	"context"

	"github.com/cloudwego/eino/schema"

	"github.com/daqiaoliang-coder/agentrix/internal/harness/budget"
)

type Middleware interface {
	Name() string
	BeforeAgent(ctx context.Context, state *AgentState) (context.Context, error)
	AfterAgent(ctx context.Context, state *AgentState, output *schema.Message) error
	BeforeModel(ctx context.Context, state *AgentState) (context.Context, error)
	AfterModel(ctx context.Context, state *AgentState, output *schema.Message) error
}

type AgentState struct {
	SessionID string
	SceneKey  string
	TurnIndex int
	Budget    *budget.Budget
	Messages  []*schema.Message
}

type BaseMiddleware struct{}

func (BaseMiddleware) Name() string { return "base" }
func (BaseMiddleware) BeforeAgent(ctx context.Context, _ *AgentState) (context.Context, error) { return ctx, nil }
func (BaseMiddleware) AfterAgent(_ context.Context, _ *AgentState, _ *schema.Message) error { return nil }
func (BaseMiddleware) BeforeModel(ctx context.Context, _ *AgentState) (context.Context, error) { return ctx, nil }
func (BaseMiddleware) AfterModel(_ context.Context, _ *AgentState, _ *schema.Message) error { return nil }

// BudgetMiddleware 已被 BudgetModel 装饰器取代，当前未接入 Graph。
// 装饰器在每次模型调用处统一处理预算检查 / 超时 / 重试 / 用量记录 / 无进展，
// 比中间件更贴近 eino 图运行时。保留本类型作为未来若需独立中间件链的参考实现。
type BudgetMiddleware struct{ BaseMiddleware }

func (BudgetMiddleware) Name() string { return "budget" }

func (BudgetMiddleware) BeforeModel(ctx context.Context, state *AgentState) (context.Context, error) {
	if state.Budget == nil {
		return ctx, nil
	}
	if err := state.Budget.CheckDeadline(); err != nil {
		return nil, err
	}
	if err := state.Budget.NextIteration(); err != nil {
		return nil, err
	}
	return ctx, nil
}

func (BudgetMiddleware) AfterModel(_ context.Context, state *AgentState, output *schema.Message) error {
	if state.Budget == nil || output == nil {
		return nil
	}
	// 从模型输出提取用量消费预算；ResponseMeta 缺失时跳过（装饰器有估算兜底）
	if output.ResponseMeta != nil && output.ResponseMeta.Usage != nil {
		_ = state.Budget.ConsumeTokens(budget.TokenUsage{
			PromptTokens:     output.ResponseMeta.Usage.PromptTokens,
			CompletionTokens: output.ResponseMeta.Usage.CompletionTokens,
			TotalTokens:      output.ResponseMeta.Usage.TotalTokens,
		})
	}
	return nil
}

type TraceMiddleware struct {
	BaseMiddleware
	OnTrace func(name string, state *AgentState)
}

func (TraceMiddleware) Name() string { return "trace" }

func (m TraceMiddleware) BeforeAgent(ctx context.Context, state *AgentState) (context.Context, error) {
	if m.OnTrace != nil {
		m.OnTrace("agent_start", state)
	}
	return ctx, nil
}

func (m TraceMiddleware) AfterAgent(_ context.Context, state *AgentState, _ *schema.Message) error {
	if m.OnTrace != nil {
		m.OnTrace("agent_end", state)
	}
	return nil
}
