package core

import (
	"context"
	"fmt"

	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"

	"github.com/daqiaoliang-coder/agentrix/internal/scene"
	"github.com/daqiaoliang-coder/agentrix/internal/session"
)

type Agent struct {
	cfg      *scene.SceneConfig
	runnable compose.Runnable[[]*schema.Message, *schema.Message]
	store    session.Store
}

func NewAgent(
	ctx context.Context,
	cfg *scene.SceneConfig,
	store session.Store,
) (*Agent, error) {
	r, err := BuildAgentGraph(ctx, cfg.Model, cfg.Tools, cfg.MaxIterations)
	if err != nil {
		return nil, fmt.Errorf("build graph: %w", err)
	}
	return &Agent{cfg: cfg, runnable: r, store: store}, nil
}

func (a *Agent) Run(
	ctx context.Context,
	sessionID string,
	userInput string,
) (*schema.Message, error) {

	state, err := a.store.LoadState(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("load state: %w", err)
	}
	history, err := a.store.LoadHistory(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("load history: %w", err)
	}

	messages := make([]*schema.Message, 0, len(history)+3)
	messages = append(messages, schema.SystemMessage(a.cfg.SystemPrompt))
	if state.MemorySummary != "" {
		messages = append(messages, schema.SystemMessage(state.MemorySummary))
	}
	messages = append(messages, history...)
	messages = append(messages, schema.UserMessage(userInput))

	output, err := a.runnable.Invoke(ctx, messages)
	if err != nil {
		return nil, fmt.Errorf("invoke agent: %w", err)
	}

	newHistory := append(history, schema.UserMessage(userInput), output)
	if err := a.store.SaveHistory(ctx, sessionID, newHistory); err != nil {
		return nil, fmt.Errorf("save history: %w", err)
	}
	state.UpdateFromTurn(messages, output)
	if err := a.store.SaveState(ctx, sessionID, state); err != nil {
		return nil, fmt.Errorf("save state: %w", err)
	}

	return output, nil
}
