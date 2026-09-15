package core

import (
	"context"
	"fmt"

	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"

	ctxengine "github.com/daqiaoliang-coder/agentrix/internal/context"
	"github.com/daqiaoliang-coder/agentrix/internal/harness/budget"
	"github.com/daqiaoliang-coder/agentrix/internal/harness/hitl"
	"github.com/daqiaoliang-coder/agentrix/internal/scene"
	"github.com/daqiaoliang-coder/agentrix/internal/session"
)

// Agent 是无状态的执行器，每次请求重新装配
type Agent struct {
	cfg      *scene.SceneConfig
	runnable compose.Runnable[[]*schema.Message, *schema.Message]
	store    session.Store
	// engine 负责上下文装配与四阶段压缩，激活此前为死代码的压缩能力
	engine *ctxengine.Engine
}

// NewAgent 按 Scene 装配 Agent 实例
// 每次请求重新装配 Agent 实例，但 Session 保存跨轮状态。实例重建不等于会话重置。
// 这意味着 NewAgent 每次创建新的 Graph 实例，但 SessionState 和 RawHistory 从外部存储加载，保证了无状态部署和有状态会话的统一。
func NewAgent(
	ctx context.Context,
	cfg *scene.SceneConfig,
	store session.Store,
) (*Agent, error) {
	// 装配上下文压缩引擎：用 SceneConfig 的 TokenBudget/CompressThreshold 调参
	engine := ctxengine.NewEngine()
	if cfg.TokenBudget > 0 {
		engine.TokenBudget = cfg.TokenBudget
	}
	if cfg.CompressThreshold > 0 {
		engine.CompressThreshold = cfg.CompressThreshold
	}

	// 用 BudgetModel 装饰器包装原始模型：双层超时 / 重试降级 / 预算 / 无进展 / 循环内压缩
	decorated := NewBudgetModel(cfg.Model, BudgetModelConfig{
		CallTimeout:     cfg.ModelCallTimeout,
		MaxRetries:      3, // 默认重试上限，可按场景覆盖
		Backoff:         cfg.ModelCallTimeout / 2, // 退避基数与调用超时联动
		NoProgressLimit: cfg.NoProgressLimit,
		FallbackModel:   cfg.FallbackModel,
		Engine:          engine,
	})

	r, err := BuildAgentGraph(ctx, decorated, cfg.Tools, cfg.MaxIterations)
	if err != nil {
		return nil, fmt.Errorf("build graph: %w", err)
	}
	return &Agent{cfg: cfg, runnable: r, store: store, engine: engine}, nil
}

// Run 执行一次 Turn
func (a *Agent) Run(
	ctx context.Context,
	sessionID string,
	userInput string,
) (*schema.Message, error) {

	// ① 外层超时（双层超时之外层）：覆盖整个 Turn 的总时间预算
	if a.cfg.TotalTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, a.cfg.TotalTimeout)
		defer cancel()
	}

	// ② 构造并注入 Budget（按 Turn）：token 预算 + 迭代上限 + deadline
	b := budget.NewBudget(a.cfg.TokenBudget, a.cfg.MaxIterations)
	if a.cfg.TotalTimeout > 0 {
		b.WithDeadline(a.cfg.TotalTimeout)
	}
	ctx = budget.WithBudget(ctx, b)

	// ③ 加载 SessionState + RawHistory
	state, err := a.store.LoadState(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("load state: %w", err)
	}
	history, err := a.store.LoadHistory(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("load history: %w", err)
	}

	// ④ 装配上下文：经 Engine.Assemble 做（必要时四阶段）压缩装配
	//    替代原先绕过压缩的 assembleContext，激活死代码
	messages, err := a.engine.Assemble(ctx, a.cfg.SystemPrompt, state, history, userInput)
	if err != nil {
		return nil, fmt.Errorf("assemble context: %w", err)
	}

	// ⑤ 执行 Graph（装饰器在循环内每轮处理超时/重试/预算/无进展/压缩）
	output, err := a.runnable.Invoke(ctx, messages)
	if err != nil {
		return nil, fmt.Errorf("invoke agent: %w", err)
	}

	// ⑥ 更新 SessionState 和 RawHistory
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

// Resume 从 HITL 中断点恢复执行
func (a *Agent) Resume(
	ctx context.Context,
	sessionID string,
	interruptID string,
	decision *hitl.ApprovalDecision,
) (*schema.Message, error) {
	resumeCtx := hitl.ResumeWithDecision(ctx, interruptID, decision)
	return a.Run(resumeCtx, sessionID, "")
}
