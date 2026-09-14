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

type BudgetMiddleware struct{ BaseMiddleware }

func (BudgetMiddleware) Name() string { return "budget" }

func (BudgetMiddleware) BeforeModel(_ context.Context, state *AgentState) (context.Context, error) {
	if state.Budget == nil {
		return nil, nil
	}
	if err := state.Budget.CheckDeadline(); err != nil {
		return nil, err
	}
	if err := state.Budget.NextIteration(); err != nil {
		return nil, err
	}
	return nil, nil
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
