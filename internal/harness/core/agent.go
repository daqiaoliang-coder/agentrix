package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/cloudwego/eino/callbacks"
	einotool "github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	"go.opentelemetry.io/otel/trace"

	ctxengine "github.com/daqiaoliang-coder/agentrix/internal/context"
	"github.com/daqiaoliang-coder/agentrix/internal/harness/budget"
	"github.com/daqiaoliang-coder/agentrix/internal/harness/hitl"
	"github.com/daqiaoliang-coder/agentrix/internal/projection"
	"github.com/daqiaoliang-coder/agentrix/internal/scene"
	"github.com/daqiaoliang-coder/agentrix/internal/session"
	"github.com/daqiaoliang-coder/agentrix/internal/telemetry"
	"github.com/daqiaoliang-coder/agentrix/internal/tool/builtin"
)

var ErrCheckpointNotFound = errors.New("checkpoint not found")

// Agent 是无状态的执行器，每次请求重新装配
type Agent struct {
	cfg             *scene.SceneConfig
	runnable        compose.Runnable[[]*schema.Message, *schema.Message]
	store           session.Store
	checkpointStore hitl.CheckPointStore
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
	// 框架工具：artifact 读写 + 子代理派生。均不经 Catalog 白名单过滤
	// （FilterTools 对未登记工具放行），与 read_skill / read_result 同等待遇。
	var extra []einotool.BaseTool

	artifactStore := resolveArtifactStore(cfg)
	extra = append(extra,
		builtin.NewWriteArtifactTool(artifactStore),
		builtin.NewReadArtifactTool(artifactStore),
	)

	// 子代理工具：按 key 排序保证工具顺序稳定（map 遍历无序会让系统提示抖动）
	for _, key := range sortedSubagentKeys(cfg.Subagents) {
		st, err := newSubagentTool(ctx, key, cfg.Subagents[key], store)
		if err != nil {
			return nil, fmt.Errorf("build subagent %q: %w", key, err)
		}
		extra = append(extra, st)
	}

	checkpointStore := hitl.DefaultCheckPointStore()
	engine, assembled, r, err := buildRuntime(ctx, cfg, extra, checkpointStore)
	if err != nil {
		return nil, err
	}
	return &Agent{
		cfg:             cfg,
		runnable:        r,
		store:           store,
		checkpointStore: checkpointStore,
		engine:          engine,
		assembled:       assembled,
	}, nil
}

// buildRuntime 装配单个场景的运行时：压缩引擎 → 场景装配 → 框架工具追加 →
// 开销快照 → BudgetModel 装饰 → 图编译。NewAgent 与 subagentTool 共用，
// 保证子代理与顶层 Agent 走完全一致的治理链路（审批/白名单/预算/压缩）。
// extra 为调用方追加的框架工具（顶层 Agent 的 artifact/子代理工具）；
// 子代理构建时传 nil，天然阻断嵌套派生。
func buildRuntime(
	ctx context.Context,
	cfg *scene.SceneConfig,
	extra []einotool.BaseTool,
	checkpointStore hitl.CheckPointStore,
) (*ctxengine.Engine, *scene.AssembleResult, compose.Runnable[[]*schema.Message, *schema.Message], error) {
	// 装配上下文压缩引擎：上下文窗口与 Turn 累计 Token 预算相互独立。
	engine := ctxengine.NewEngine()
	if cfg.ModelContextWindow > 0 {
		engine.ModelContextWindow = cfg.ModelContextWindow
	}
	if cfg.CompressThreshold > 0 {
		engine.CompressThreshold = cfg.CompressThreshold
	}

	// 场景装配：注入技能索引（阶段 0）→ 审批包装（HITL）→ 命令白名单过滤（Catalog）
	// 必须先于开销快照计算：快照要基于装配后的真实系统提示与最终工具集。
	assembled, err := cfg.Assemble(ctx)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("assemble scene: %w", err)
	}

	// 溢出存储：承接被压缩淘汰的工具结果原文，使淘汰「可恢复」而非「删除」。
	// 进程内实现，作用域为单个运行时（即单次 Turn 的 ReAct 循环）；
	// 生产环境可替换为对象存储实现以支持跨 Turn 回读。
	spill := ctxengine.NewMemorySpillStore()
	engine.SetSpillStore(spill)

	// 回读工具：与淘汰逻辑闭环。模型看到 stub 后可凭 tool_call_id 取回原文。
	// 作为框架工具追加，不经 Catalog 白名单过滤（与 read_skill 同等待遇）。
	assembled.Tools = append(assembled.Tools, builtin.NewReadResultTool(spill, readResultMaxChars))
	assembled.Tools = append(assembled.Tools, extra...)

	// 写/非幂等工具标记：复用 Catalog 的 NeedsApproval（写类命令）作为判定依据。
	// 这类工具结果无法重放，淘汰即永久丢失，故排除在淘汰之外。
	if catalog := cfg.Catalog; catalog != nil {
		engine.IsNonIdempotent = func(toolName string) bool {
			spec, ok := catalog.SpecOf(toolName)
			return ok && spec.NeedsApproval
		}
	}

	// 固定开销快照只包含不在 messages 列表中的活跃工具 schema；system prompt
	// 已由 Assemble 作为首条消息计入，不能在这里重复计算。
	engine.SetOverhead(computeOverhead(ctx, assembled.Tools))

	// 用 BudgetModel 装饰器包装原始模型：双层超时 / 重试降级 / 预算 / 无进展 / 循环内压缩
	decorated := NewBudgetModel(cfg.Model, BudgetModelConfig{
		CallTimeout:     cfg.ModelCallTimeout,
		MaxRetries:      3,                        // 默认重试上限，可按场景覆盖
		Backoff:         cfg.ModelCallTimeout / 2, // 退避基数与调用超时联动
		NoProgressLimit: cfg.NoProgressLimit,
		FallbackModel:   cfg.FallbackModel,
		Engine:          engine,
	})

	r, err := BuildAgentGraph(ctx, decorated, assembled.Tools, cfg.MaxIterations, checkpointStore)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("build graph: %w", err)
	}
	return engine, assembled, r, nil
}

// artifactStores 按场景共享进程内 ArtifactStore：SceneConfig 未显式配置时，
// 同一场景的多轮、多 Agent 实例共享同一存储，artifact 才能跨轮累积。
var artifactStores sync.Map // sceneKey -> projection.ArtifactStore

func resolveArtifactStore(cfg *scene.SceneConfig) projection.ArtifactStore {
	if cfg.ArtifactStore != nil {
		return cfg.ArtifactStore
	}
	actual, _ := artifactStores.LoadOrStore(cfg.Key, projection.NewMemoryArtifactStore())
	return actual.(projection.ArtifactStore)
}

// RegisteredArtifactStores 返回所有已解析的默认 ArtifactStore 快照，
// 供 API 层按会话列举 artifact（显式配置的 cfg.ArtifactStore 由调用方自管）。
func RegisteredArtifactStores() []projection.ArtifactStore {
	var out []projection.ArtifactStore
	artifactStores.Range(func(_, v any) bool {
		out = append(out, v.(projection.ArtifactStore))
		return true
	})
	return out
}

// readResultMaxChars 是 read_result 单次回读的字符上限，防止一条超大结果回读后
// 又把上下文顶爆（对应设计中的「read_result 页上限」）。
//
// 按 EstimateTextTokens 的密度换算，5 万字符约等于 1.4 万 token（ASCII/JSON）
// 到 3.5 万 token（中文）。此前注释按「2 字符/token」估成约 25k token，
// 对工具结果这种以 JSON 为主的内容偏高近一倍——而偏高的估算会让人误以为
// 还有余量，从而把这个上限设得过大。
const readResultMaxChars = 50000

// computeOverhead 计算固定开销快照。
//
// 工具 schema 用 Info() 的 JSON 序列化长度估算，与消息体共用
// ctxengine.EstimateTextTokens 这唯一一套估算口径。
//
// 这一点是压缩决策正确性的前提：overhead 与消息本体分别用不同公式估算时，
// effectiveTokens 的两部分误差方向可能相反，softLimit 就变成一条位置不明的线——
// 压缩可能在远未接近窗口时触发（白白作废缓存前缀），也可能在真要爆窗时才触发。
func computeOverhead(
	ctx context.Context,
	tools []einotool.BaseTool,
) ctxengine.PromptOverheadSnapshot {
	var snap ctxengine.PromptOverheadSnapshot
	for _, t := range tools {
		if t == nil {
			continue
		}
		info, err := t.Info(ctx)
		if err != nil || info == nil {
			continue
		}
		if raw, mErr := json.Marshal(info); mErr == nil {
			snap.ToolsTokens += ctxengine.EstimateTextTokens(string(raw))
		} else {
			// 序列化失败时退化为按名称+描述估算，不阻断装配
			snap.ToolsTokens += ctxengine.EstimateTextTokens(info.Name) + ctxengine.EstimateTextTokens(info.Desc)
		}
	}
	return snap
}

// toolMsgCollector 通过 eino callbacks 捕获图内的工具交互，补齐 RawHistory 审计完整性。
//
// 过滤规则：只收 ToolsNode 组件的回调——
//   - OnStart 输入为携带 tool_calls 的 assistant 消息（工具调用发起侧）；
//   - OnEnd 输出为 []*schema.Message 的 tool 结果消息（工具结果回填侧）。
//
// 多轮 ReAct 时按时间顺序收集：[assistant_tc, results, assistant_tc2, results2, ...]。
// 仅在 Invoke 成功后消费；中断/失败路径丢弃，避免 Resume 重放同一段交互造成重复追加
// （中断点前的历史轮次不入审计，装配阶段由 Engine 的 tool 配对修复逻辑兜底）。
type toolMsgCollector struct {
	mu   sync.Mutex
	msgs []*schema.Message
}

func newToolMsgCollector() *toolMsgCollector {
	return &toolMsgCollector{}
}

func (c *toolMsgCollector) handler() callbacks.Handler {
	return callbacks.NewHandlerBuilder().
		OnStartFn(func(ctx context.Context, info *callbacks.RunInfo, input callbacks.CallbackInput) context.Context {
			c.collectStart(info, input)
			return ctx
		}).
		OnStartWithStreamInputFn(func(ctx context.Context, info *callbacks.RunInfo, input *schema.StreamReader[callbacks.CallbackInput]) context.Context {
			c.collectStart(info, drainInputMessages(input))
			return ctx
		}).
		OnEndFn(func(ctx context.Context, info *callbacks.RunInfo, output callbacks.CallbackOutput) context.Context {
			c.collectEnd(info, output)
			return ctx
		}).
		OnEndWithStreamOutputFn(func(ctx context.Context, info *callbacks.RunInfo, output *schema.StreamReader[callbacks.CallbackOutput]) context.Context {
			c.collectEnd(info, drainOutputMessages(output))
			return ctx
		}).
		Build()
}

// collectStart/collectEnd 是普通与流式时机共用的收集逻辑。
// 流式模式下工具节点输入是增量帧，已由 drain 拼接还原为完整消息。
func (c *toolMsgCollector) collectStart(info *callbacks.RunInfo, input callbacks.CallbackInput) {
	if info == nil || info.Component != compose.ComponentOfToolsNode {
		return
	}
	if m, ok := input.(*schema.Message); ok && m != nil && len(m.ToolCalls) > 0 {
		c.mu.Lock()
		c.msgs = append(c.msgs, m)
		c.mu.Unlock()
	}
}

func (c *toolMsgCollector) collectEnd(info *callbacks.RunInfo, output callbacks.CallbackOutput) {
	if info == nil || info.Component != compose.ComponentOfToolsNode {
		return
	}
	if msgs, ok := output.([]*schema.Message); ok && len(msgs) > 0 {
		c.mu.Lock()
		c.msgs = append(c.msgs, msgs...)
		c.mu.Unlock()
	}
}

func (c *toolMsgCollector) snapshot() []*schema.Message {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*schema.Message, len(c.msgs))
	copy(out, c.msgs)
	return out
}

// Run 执行一次 Turn
func (a *Agent) Run(
	ctx context.Context,
	sessionID string,
	userInput string,
) (*schema.Message, error) {
	return a.run(ctx, sessionID, userInput, false, nil)
}

// RunStream 与 Run 行为一致，额外通过 sink 实时推出过程信号（projection.Signal）。
// sink 在图回调里被调用，须快速返回；需要慢消费（如网络推送）时自行缓冲。
func (a *Agent) RunStream(
	ctx context.Context,
	sessionID string,
	userInput string,
	sink func(projection.Signal),
) (*schema.Message, error) {
	return a.run(ctx, sessionID, userInput, false, sink)
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
	return a.run(resumeCtx, sessionID, "", true, nil)
}

// ResumeStream 是 Resume 的流式变体，恢复执行的过程信号经 sink 推出。
func (a *Agent) ResumeStream(
	ctx context.Context,
	sessionID string,
	interruptID string,
	decision *hitl.ApprovalDecision,
	sink func(projection.Signal),
) (*schema.Message, error) {
	resumeCtx := hitl.ResumeWithDecision(ctx, interruptID, decision)
	return a.run(resumeCtx, sessionID, "", true, sink)
}

// run 是 Run/Resume 及各自流式变体的共享实现。resuming 为 true 时从检查点
// 恢复而非重新开始；sink 非 nil 时发射过程信号，为 nil 时信号发射零成本。
func (a *Agent) run(
	ctx context.Context,
	sessionID string,
	userInput string,
	resuming bool,
	sink func(projection.Signal),
) (out *schema.Message, retErr error) {

	// ① 外层超时（双层超时之外层）：覆盖整个 Turn 的总时间预算
	if a.cfg.TotalTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, a.cfg.TotalTimeout)
		defer cancel()
	}

	if resuming {
		if err := a.requireCheckpoint(ctx, sessionID); err != nil {
			return nil, err
		}
	}

	// ② 构造并注入 Budget（按 Turn）：token 预算 + 迭代上限 + deadline
	b := budget.NewBudget(a.cfg.TokenBudget, a.cfg.MaxIterations)
	if a.cfg.TotalTimeout > 0 {
		b.WithDeadline(a.cfg.TotalTimeout)
	}
	ctx = budget.WithBudget(ctx, b)

	// Turn 归属信息注入 ctx：write_artifact / spawn_* 等框架工具在图回调深处
	// 凭它取回 sessionID/turnID，避免让模型自报归属。
	turnID := fmt.Sprintf("turn_%d", time.Now().UnixNano())
	ctx = projection.WithTurnScope(ctx, sessionID, turnID)

	// 可观测导出：开启 Turn 根 span（图回调里的 LLM/工具 span 经 ctx 挂到其下）。
	// 未启用时 Default() 为 nil，Enabled() 为 false，整块零开销。
	// outcome 供 defer 读取：审批中断是暂停而非失败，需与真实错误区分。
	tel := telemetry.Default()
	outcome := "ok"
	if tel.Enabled() {
		turnStart := time.Now()
		var turnSpan trace.Span
		ctx, turnSpan = tel.StartTurn(ctx, a.cfg.Key, sessionID, turnID, resuming)
		defer func() {
			oc := outcome
			if retErr != nil && oc == "ok" {
				oc = "error"
			}
			tel.EndTurn(ctx, turnSpan, a.cfg.Key, oc, time.Since(turnStart), retErr)
		}()
	}

	emitter := projection.NewEmitter(sessionID, turnID, sink)
	emitter.Emit(projection.TurnStart, map[string]any{"input": userInput, "resuming": resuming})

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
	//
	// toolMsgCollector 挂在图回调上捕获工具交互，供 ⑥ 追加进 RawHistory。
	// 流式模式下追加 signalCallback，把同一批回调翻译为过程信号。
	// 启用遥测时追加 telemetry.Callback（同步/流式都挂，与 sink 无关），
	// 其 Close 兜底关闭审批中断路径上悬挂的节点 span——须在 EndTurn 之前
	// 执行（defer LIFO，后注册先执行）。
	capture := newToolMsgCollector()
	invokeOpts := []compose.Option{compose.WithCheckPointID(sessionID)}
	handlers := []callbacks.Handler{capture.handler()}
	if tel.Enabled() {
		telCb := tel.NewCallback(a.cfg.Key)
		handlers = append(handlers, telCb.Handler())
		defer telCb.Close()
	}
	if sink != nil {
		handlers = append(handlers, newSignalCallback(emitter).handler())
	}
	invokeOpts = append(invokeOpts, compose.WithCallbacks(handlers...))
	if !resuming {
		// Eino v0.9.19 会用每个后续 Option 的零值覆盖 forceNewRun，故必须最后追加。
		invokeOpts = append(invokeOpts, compose.WithForceNewRun())
	}

	// 流式模式（sink != nil）走 runnable.Stream：模型节点以流式产出，
	// signalCallback 经 OnEndWithStreamOutput 逐帧发射 llm_token；
	// 图输出流拼接后还原最终消息，与 Invoke 路径语义一致。
	output, err := a.execute(ctx, messages, sink != nil, invokeOpts)
	if err != nil {
		// 审批中断也是运行事实：让外层能实时感知「正在等待人工授权」
		if approval, ok := ExtractApprovalRequired(err); ok {
			emitter.Emit(projection.ApproveRequested, approval.Request)
			outcome = "approval_required"
		}
		return nil, fmt.Errorf("invoke agent: %w", err)
	}

	// ⑥ 追加 RawHistory（append-only）：user 输入 → 图内工具交互 → 最终输出
	//    恢复模式没有新的用户输入，跳过 user 消息避免写入空记录。
	newMsgs := make([]*schema.Message, 0, len(capture.msgs)+2)
	if !resuming {
		newMsgs = append(newMsgs, schema.UserMessage(userInput))
	}
	newMsgs = append(newMsgs, capture.snapshot()...)
	newMsgs = append(newMsgs, output)
	lastSeq, err := a.store.AppendHistory(ctx, sessionID, newMsgs...)
	if err != nil {
		return nil, fmt.Errorf("append history: %w", err)
	}

	// 游标指向本次追加的最后一条记录 seq（本 Turn 已全量消费历史）
	state.UpdateFromTurn()
	state.HistoryCursor = int(lastSeq)
	if err := a.store.SaveState(ctx, sessionID, state); err != nil {
		return nil, fmt.Errorf("save state: %w", err)
	}
	if err := a.deleteCheckpoint(ctx, sessionID); err != nil {
		return nil, err
	}

	emitter.Emit(projection.TurnEnd, map[string]any{"content_length": len(output.Content)})
	return output, nil
}

func (a *Agent) requireCheckpoint(ctx context.Context, sessionID string) error {
	_, ok, err := a.checkpointStore.Get(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("get checkpoint: %w", err)
	}
	if !ok {
		return fmt.Errorf("%w: %s", ErrCheckpointNotFound, sessionID)
	}
	return nil
}

func (a *Agent) deleteCheckpoint(ctx context.Context, sessionID string) error {
	deleter, ok := a.checkpointStore.(hitl.CheckPointDeleter)
	if !ok {
		return fmt.Errorf("checkpoint store %T does not support deletion", a.checkpointStore)
	}
	if err := deleter.Delete(ctx, sessionID); err != nil {
		return fmt.Errorf("delete checkpoint: %w", err)
	}
	return nil
}

// execute 按模式执行图：同步走 Invoke；流式走 Stream 并读空输出流、
// 拼接帧还原最终消息。流式模式下运行时错误（含审批中断）经输出流的
// Recv 返回，与 Invoke 的返回错误统一处理。
func (a *Agent) execute(
	ctx context.Context,
	messages []*schema.Message,
	stream bool,
	opts []compose.Option,
) (*schema.Message, error) {
	if !stream {
		return a.runnable.Invoke(ctx, messages, opts...)
	}
	sr, err := a.runnable.Stream(ctx, messages, opts...)
	if err != nil {
		return nil, err
	}
	defer sr.Close()
	var frames []*schema.Message
	for {
		frame, recvErr := sr.Recv()
		if recvErr == io.EOF {
			break
		}
		if recvErr != nil {
			return nil, recvErr
		}
		if frame != nil {
			frames = append(frames, frame)
		}
	}
	if len(frames) == 0 {
		return nil, fmt.Errorf("agent stream produced no output")
	}
	out, err := schema.ConcatMessages(frames)
	if err != nil {
		return nil, fmt.Errorf("concat stream output: %w", err)
	}
	return out, nil
}
