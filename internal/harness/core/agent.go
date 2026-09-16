package core

import (
	"context"
	"encoding/json"
	"fmt"

	einotool "github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"

	ctxengine "github.com/daqiaoliang-coder/agentrix/internal/context"
	"github.com/daqiaoliang-coder/agentrix/internal/harness/budget"
	"github.com/daqiaoliang-coder/agentrix/internal/harness/hitl"
	"github.com/daqiaoliang-coder/agentrix/internal/scene"
	"github.com/daqiaoliang-coder/agentrix/internal/session"
	"github.com/daqiaoliang-coder/agentrix/internal/tool/builtin"
)

// Agent 是无状态的执行器，每次请求重新装配
type Agent struct {
	cfg      *scene.SceneConfig
	runnable compose.Runnable[[]*schema.Message, *schema.Message]
	store    session.Store
	// engine 负责上下文装配与四阶段压缩，激活此前为死代码的压缩能力
	engine *ctxengine.Engine
	// assembled 保存装配产物：注入技能索引后的系统提示 + 过滤/包装后的工具集
	assembled *scene.AssembleResult
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

	// 场景装配：注入技能索引（阶段 0）→ 审批包装（HITL）→ 命令白名单过滤（Catalog）
	// 必须先于开销快照计算：快照要基于装配后的真实系统提示与最终工具集。
	assembled, err := cfg.Assemble(ctx)
	if err != nil {
		return nil, fmt.Errorf("assemble scene: %w", err)
	}

	// 溢出存储：承接被压缩淘汰的工具结果原文，使淘汰「可恢复」而非「删除」。
	// 进程内实现，作用域为单个 Agent 实例（即单次 Turn 的 ReAct 循环）；
	// 生产环境可替换为对象存储实现以支持跨 Turn 回读。
	spill := ctxengine.NewMemorySpillStore()
	engine.SetSpillStore(spill)

	// 回读工具：与淘汰逻辑闭环。模型看到 stub 后可凭 tool_call_id 取回原文。
	// 作为框架工具追加，不经 Catalog 白名单过滤（与 read_skill 同等待遇）。
	assembled.Tools = append(assembled.Tools, builtin.NewReadResultTool(spill, readResultMaxChars))

	// 写/非幂等工具标记：复用 Catalog 的 NeedsApproval（写类命令）作为判定依据。
	// 这类工具结果无法重放，淘汰即永久丢失，故排除在淘汰之外。
	if catalog := cfg.Catalog; catalog != nil {
		engine.IsNonIdempotent = func(toolName string) bool {
			spec, ok := catalog.SpecOf(toolName)
			return ok && spec.NeedsApproval
		}
	}

	// 固定开销快照：system prompt + 活跃工具 schema。这两块是模型每次调用都要付、
	// 却不在 messages 列表里的真实输入；漏算会让压缩阈值失真、触发过晚。
	// 由装配层（此处）计算并注入，engine 只消费快照，不反向依赖工具注册表。
	engine.SetOverhead(computeOverhead(ctx, assembled.SystemPrompt, assembled.Tools))

	// 用 BudgetModel 装饰器包装原始模型：双层超时 / 重试降级 / 预算 / 无进展 / 循环内压缩
	decorated := NewBudgetModel(cfg.Model, BudgetModelConfig{
		CallTimeout:     cfg.ModelCallTimeout,
		MaxRetries:      3,                        // 默认重试上限，可按场景覆盖
		Backoff:         cfg.ModelCallTimeout / 2, // 退避基数与调用超时联动
		NoProgressLimit: cfg.NoProgressLimit,
		FallbackModel:   cfg.FallbackModel,
		Engine:          engine,
	})

	r, err := BuildAgentGraph(ctx, decorated, assembled.Tools, cfg.MaxIterations)
	if err != nil {
		return nil, fmt.Errorf("build graph: %w", err)
	}
	return &Agent{cfg: cfg, runnable: r, store: store, engine: engine, assembled: assembled}, nil
}

// readResultMaxChars 是 read_result 单次回读的字符上限，防止一条超大结果回读后
// 又把上下文顶爆（对应设计中的「read_result 页上限」）。约 25k token（2 字符/token）。
const readResultMaxChars = 50000

// computeOverhead 计算固定开销快照。
// 工具 schema 用 Info() 的 JSON 序列化长度估算，与消息体共用同一套粗略 token 估算
// （estimateTextLen，约 2 字符/token）。该估算对 ASCII 偏保守（高估），
// 生产环境建议替换为 tiktoken 精确计量。
func computeOverhead(
	ctx context.Context,
	systemPrompt string,
	tools []einotool.BaseTool,
) ctxengine.PromptOverheadSnapshot {
	snap := ctxengine.PromptOverheadSnapshot{
		SystemTokens: estimateTextLen(systemPrompt),
	}
	for _, t := range tools {
		if t == nil {
			continue
		}
		info, err := t.Info(ctx)
		if err != nil || info == nil {
			continue
		}
		if raw, mErr := json.Marshal(info); mErr == nil {
			snap.ToolsTokens += estimateTextLen(string(raw))
		} else {
			// 序列化失败时退化为按名称+描述估算，不阻断装配
			snap.ToolsTokens += estimateTextLen(info.Name) + estimateTextLen(info.Desc)
		}
	}
	return snap
}

// Run 执行一次 Turn
func (a *Agent) Run(
	ctx context.Context,
	sessionID string,
	userInput string,
) (*schema.Message, error) {
	return a.run(ctx, sessionID, userInput, false)
}

// Resume 从 HITL 中断点恢复执行。
//
// 与 Run 的关键区别：不带 ForceNewRun，eino 会从 CheckPointStore 载入
// 中断时保存的图状态继续执行；input 由检查点提供，故此处传空字符串。
func (a *Agent) Resume(
	ctx context.Context,
	sessionID string,
	interruptID string,
	decision *hitl.ApprovalDecision,
) (*schema.Message, error) {
	resumeCtx := hitl.ResumeWithDecision(ctx, interruptID, decision)
	return a.run(resumeCtx, sessionID, "", true)
}

// run 是 Run/Resume 的共享实现。resuming 为 true 时从检查点恢复而非重新开始。
func (a *Agent) run(
	ctx context.Context,
	sessionID string,
	userInput string,
	resuming bool,
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
	//    系统提示取装配产物（已注入技能索引），而非原始 cfg.SystemPrompt
	//
	//    恢复模式下 eino 使用检查点内保存的输入，这里装配的 messages 仅作占位，
	//    但仍需构造合法输入以满足 Runnable 的类型约束。
	messages, err := a.engine.Assemble(ctx, a.assembled.SystemPrompt, state, history, userInput)
	if err != nil {
		return nil, fmt.Errorf("assemble context: %w", err)
	}

	// ⑤ 执行 Graph（装饰器在循环内每轮处理超时/重试/预算/无进展/压缩）
	//
	// 必须显式传 CheckPointID：eino 在 checkPointID 为 nil 时既不写也不读检查点，
	// 审批中断的状态将无法保存，Resume 必然失败。以 sessionID 作为检查点标识，
	// 使同一会话的中断可被定位。
	//
	// ForceNewRun 只用于全新 Turn，避免误从上一轮残留的检查点恢复；
	// 恢复模式必须省略它，否则中断状态被丢弃。
	invokeOpts := []compose.Option{compose.WithCheckPointID(sessionID)}
	if !resuming {
		invokeOpts = append(invokeOpts, compose.WithForceNewRun())
	}
	output, err := a.runnable.Invoke(ctx, messages, invokeOpts...)
	if err != nil {
		return nil, fmt.Errorf("invoke agent: %w", err)
	}

	// ⑥ 更新 SessionState 和 RawHistory
	//    恢复模式没有新的用户输入，只追加模型输出，避免写入空 user 消息。
	if resuming {
		history = append(history, output)
	} else {
		history = append(history, schema.UserMessage(userInput), output)
	}
	if err := a.store.SaveHistory(ctx, sessionID, history); err != nil {
		return nil, fmt.Errorf("save history: %w", err)
	}
	state.UpdateFromTurn(messages, output)
	if err := a.store.SaveState(ctx, sessionID, state); err != nil {
		return nil, fmt.Errorf("save state: %w", err)
	}

	return output, nil
}
