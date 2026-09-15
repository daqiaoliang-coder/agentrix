# Harness 鲁棒性优化方案

## Context（背景）

用户提供了一份基于「手动 for 循环」伪代码的 6 条最佳实践建议（双层超时 / Token 预算+无进展 / 上下文压缩时机 / 模型 fallback 重试 / 工具结果标准化 / 终止条件精细化）。但 agentrix 的实际架构是 **eino `compose.Graph` 驱动的 ReAct 循环**——模型与工具是图节点，循环体在 eino 运行时内部，用户代码中没有显式迭代入口。

因此本方案的核心策略是：**用一个模型装饰器 `BudgetModel` 包装 `model.ToolCallingChatModel`，作为伪代码中 `CallWithFallback` 的实际落地点**。装饰器被传给 `BuildAgentGraph` 后，eino 透明地用它替换原始模型，于是每次模型调用都会经过：预算检查 → 超时 → 重试/降级 → 用量记录 → 无进展检测 → 上下文压缩。这是图架构下唯一能在单点覆盖所有按迭代关切的位置。

预期结果：在不破坏现有 ReAct 图结构的前提下，把 6 条建议中的 5 条完整落地（#5 工具标准化因 eino 内部已处理，仅加注释说明），并修复一个已存在的 Bug。

## 现状关键问题（已核实）

1. `BudgetMiddleware` 已定义但**从未接入图**，且 `BeforeModel` 有 Bug：返回 `nil, nil` 而非 `ctx, nil`，一旦启用会 panic。
2. `Budget` 有 `ConsumeTokens/NextIteration/CheckDeadline` 但**从未在 `Agent.Run` 中构造**。
3. `context.Engine.Assemble`（四阶段压缩）存在但 `Agent` 用自己的 `assembleContext`，**完全绕过了压缩**。
4. `SceneConfig` 缺少 `TotalTimeout` / `ModelCallTimeout` / `NoProgressLimit` 字段。

## 改动清单

### 1. 新建 `internal/harness/core/model_decorator.go`（核心）

定义 `BudgetModel` 实现 `model.ToolCallingChatModel`（3 个方法：`Generate`/`Stream`/`WithTools`）。

- **构造**：`NewBudgetModel(raw model.ToolCallingChatModel, cfg BudgetModelConfig) *BudgetModel`，其中 `BudgetModelConfig` 携带 `CallTimeout`、`MaxRetries`、`Backoff`、`NoProgressLimit`、`*context.Engine`。
- **预算来源**：装饰器自身**不持有** `*budget.Budget`（避免跨 Turn 状态泄漏），而是在 `Generate` 内通过 `budget.FromContext(ctx)` 读取——预算按 Turn 在 `Agent.Run` 中创建并注入 context。这复用现有 `budget.WithBudget`/`FromContext` 模式。
- **`Generate` 流程**：
  1. `b, _ := budget.FromContext(ctx)`；若 `b.IsExhausted()` 或 `CheckDeadline()` 失败 → 返回 `budgetExhaustedErr`（图终止）。
  2. 无进展检测：若 `b.IsStagnant(NoProgressLimit)` → 把「你已连续 N 轮无新进展，请总结并结束」作为**额外 system 消息注入输入**（注入输入而非输出，让模型还能再走一轮，而非直接路由到 END）。
  3. 上下文压缩：若 `Engine != nil`，对 `input` 调 `Engine.Assemble`（顶部压缩，对应建议 #3）；`trimToolOutputs` 已天然避免工具输出原样塞回。
  4. 单次超时：`callCtx, cancel := context.WithTimeout(ctx, CallTimeout)`（建议 #1 内层）。
  5. 重试分类（建议 #4）：`isRetryable(err)`（网络/限流/超时）→ 指数退避重试至 `MaxRetries`；非可重试（参数/内容违规）→ 不重试，返回一个**合成的 assistant 消息**（无 `ToolCalls`，内容为错误说明），让图分支走 END 收尾。
  6. 用量记录：从 `output.ResponseMeta.Usage` 提取（`nil` 时用 `estimateTokens` 估算兜底）→ `b.ConsumeTokens(...)`。
  7. 无进展签名记录：把本轮 `ToolCalls` 的签名哈希 `b.RecordProgress(sig)`。
- **`WithTools`**：必须返回**新的 `*BudgetModel`**（包装派生后的模型），否则后续调用会丢失预算/超时/重试。
- **`Stream`**：第一版简化为直接委托（加单次超时即可，不做复杂重试——流式重试语义复杂）。

### 2. 修改 `internal/harness/budget/budget.go`

新增方法（不改现有方法签名）：
- `IsExhausted() bool`：token 或迭代任一超限即 true。
- `RecordProgress(signature string)`：维护长度为 N 的环形签名缓冲（mutex 保护）。
- `IsStagnant(limit int) bool`：`limit <= 0` 时返回 false；最近 `limit` 轮签名完全相同即 true。

### 3. 修改 `internal/scene/config.go`

新增字段（零值 = 禁用，向后兼容）：
- `TotalTimeout time.Duration`
- `ModelCallTimeout time.Duration`
- `NoProgressLimit int`
- `FallbackModel model.ToolCallingChatModel`（#4 降级模型，默认 nil；非 nil 时重试耗尽后切到 fallback 再试一次）

### 4. 修改 `internal/harness/core/agent.go`

- `NewAgent`：构造 `*context.Engine`（用 `cfg.TokenBudget`/`cfg.CompressThreshold`，`NewEngine` + 调参）；构造 `BudgetModelConfig`；用 `NewBudgetModel(cfg.Model, budgetCfg)` 包装模型；把**装饰后的模型**传给 `BuildAgentGraph`（替换原来的 `cfg.Model`）。`Engine` 作为字段存到 `Agent`。
- `Run`：
  1. 若 `cfg.TotalTimeout > 0` → `ctx, cancel := context.WithTimeout(ctx, cfg.TotalTimeout); defer cancel()`（建议 #1 外层）。
  2. 构造 `b := budget.NewBudget(cfg.TokenBudget, cfg.MaxIterations)`；若 `cfg.TotalTimeout > 0` → `b.WithDeadline(cfg.TotalTimeout)`；`ctx = budget.WithBudget(ctx, b)`。
  3. 用 `a.engine.Assemble(ctx, cfg.SystemPrompt, state, history, userInput)` **替换** `assembleContext`（落地建议 #3，激活目前是死代码的压缩引擎）。
  4. 其余逻辑不变。

### 5. 修改 `internal/harness/core/middleware.go`

- 修 Bug：`BudgetMiddleware.BeforeModel` 的 `return nil, nil` → `return ctx, nil`。
- `AfterModel` 补 `state.Budget.ConsumeTokens(...)`（从 `output.ResponseMeta.Usage` 提取）。
- 加注释：**已被 `BudgetModel` 装饰器取代，不再接入图**；保留作为未来若需独立中间件链时的参考实现。

### 6. 关于建议 #5（工具结果标准化）——仅加注释

在 `internal/harness/core/builder.go` 的 `AddToolsNode` 处加注释：eino `ToolsNode` 内部并发执行工具并独立处理单个工具失败（部分失败不阻断整体）；每工具 `context.WithTimeout` 需包装 `tool.BaseTool`，当前 ROI 不足，暂不实现，留作后续增强。

## 各建议落地情况

| # | 建议 | 状态 | 机制 |
|---|------|------|------|
| 1 | 双层超时 | 完整 | 外层 `WithTimeout` 在 `Agent.Run`；内层 `WithTimeout` 在装饰器 `Generate` |
| 2 | Token 预算+迭代+无进展 | 完整 | 预算在 `Run` 构造注入；装饰器每轮 `IsExhausted`/`ConsumeTokens`/`RecordProgress`/`IsStagnant`；迭代硬上限 = `WithMaxRunSteps`（已有） |
| 3 | 上下文压缩时机 | 完整 | 装饰器在模型调用前对输入调 `Engine.Assemble`（顶部压缩）；`trimToolOutputs` 已做工具输出截断 |
| 4 | Fallback 重试 | 完整 | 装饰器错误分类：可重试→退避重试；非可重试→合成 assistant 消息收尾；`FallbackModel` 字段预留降级 |
| 5 | 工具结果标准化 | 仅注释 | eino 内部已处理部分失败；标注设计决策，不实现工具装饰器 |
| 6 | 终止条件精细化 | 完整 | 迭代上限=`WithMaxRunSteps`；预算迭代上限=装饰器；无进展=装饰器 nudge；用户中断=`runCtx` 取消透传图 |

## 风险与缓解

- **`WithTools` 必须返回新装饰器**：已纳入实现，否则预算/超时静默丢失。
- **`ResponseMeta.Usage` 可能为 nil**：用 `estimateTokens` 估算兜底（引擎已有此函数）。
- **装饰器无状态**：预算按 Turn 经 context 注入，装饰器实例可复用，无跨 Turn 泄漏。
- **合成错误消息 + 分支**：非可重试错误返回无 `ToolCalls` 的 assistant 消息 → 图分支走 END 收尾，不破坏协议。

## 验证

1. `go build ./...`：每改一个文件后增量编译检查。
2. `examples/echo_scene/main.go`：在 `SceneConfig` 加 `TotalTimeout: 30*time.Second`、`ModelCallTimeout: 10*time.Second`、`NoProgressLimit: 3`，运行 `go run ./examples/echo_scene` 确认正常 Turn 仍能完成（验证端到端接线无破坏）。
3. 不编写单元测试（按用户确认）。
