package core

import (
	"context"
	"fmt"

	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"

	"github.com/daqiaoliang-coder/agentrix/internal/scene"
	"github.com/daqiaoliang-coder/agentrix/internal/session"
)

// Agent 是无状态的执行器，每次请求重新装配
type Agent struct {
	cfg      *scene.SceneConfig
	runnable compose.Runnable[[]*schema.Message, *schema.Message]
	store    session.Store
}

// NewAgent 按 Scene 装配 Agent 实例
// 每次请求重新装配 Agent 实例，但 Session 保存跨轮状态。实例重建不等于会话重置。
// 这意味着 NewAgent 每次创建新的 Graph 实例，但 SessionState 和 RawHistory 从外部存储加载，保证了无状态部署和有状态会话的统一。
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

// Run 执行一次 Turn
func (a *Agent) Run(
	ctx context.Context,
	sessionID string,
	userInput string,
) (*schema.Message, error) {

	// ① 加载 SessionState + RawHistory
	state, err := a.store.LoadState(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("load state: %w", err)
	}
	history, err := a.store.LoadHistory(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("load history: %w", err)
	}

	// ② 装配上下文：System Prompt + 记忆摘要 + 最近历史 + 当前输入
	messages := a.assembleContext(state, history, userInput)

	// ③ 执行 Graph
	output, err := a.runnable.Invoke(ctx, messages)
	if err != nil {
		return nil, fmt.Errorf("invoke agent: %w", err)
	}

	// ④ 更新 SessionState 和 RawHistory
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

func (a *Agent) assembleContext(state *session.State, history []*schema.Message, userInput string) []*schema.Message {
	var messages []*schema.Message

	// System Prompt
	messages = append(messages, schema.SystemMessage(a.cfg.SystemPrompt))

	// 记忆摘要（压缩后的历史摘要）
	if state.MemorySummary != "" {
		messages = append(messages, schema.SystemMessage(
			"以下是之前对话的摘要：\n"+state.MemorySummary,
		))
	}

	// 最近历史（按游标加载）
	if len(history) > 0 {
		messages = append(messages, history...)
	}

	// 当前用户输入
	messages = append(messages, schema.UserMessage(userInput))

	return messages
}
