# agentrix Token 消耗优化差距分析

参照物：DeepSeek Harness 的两条主线——**输入端做减法**（压缩 / 裁剪 / 懒加载）、**计算端做复用**（稳定前缀 + 分层记忆，最大化 KV Cache 命中）。

结论先说：agentrix 在「超窗后补救」这条路上已经做得比较扎实（need-driven 压缩、精确 overhead 账本、offload + read_result 闭环），但在**「让内容根本不进上下文」和「让前缀真正稳定」这两件事上还有系统性缺口**。其中 3 项是配置失配 / 实现疏漏级别的，不改的话已有的压缩能力大部分是空转的。

---

## 一、失配与疏漏（当前代码即错，优先修）

### 1. LLM 摘要路径永不执行 —— TailTokenBudget 大于 TokenBudget

- `internal/context/engine.go:19` `defaultTailTokenBudget = 20000`
- `internal/harness/core/agent.go:41-46` `NewAgent` 只覆盖 `TokenBudget` 和 `CompressThreshold`，**从不覆盖 `TailTokenBudget`**
- `examples/release_scene/main.go:352` 场景配 `TokenBudget: 16384`

推导链：压缩触发线 `softLimit = 16384 × 0.8 = 13107`。触发时消息总量约 13107 − overhead，必然小于 20000。`splitByBoundary`（engine.go:396-407）从末尾累加直到超过 tailBudget，于是 `tailStart` 一路退到 0，`middle` 恒为空。`compress` 在 engine.go:195-197 提前返回，**阶段③结构化摘要（七字段）从未运行过**，`state.MemorySummary` 永远是空串。

后果：整套「四阶段压缩」实际只剩阶段①的工具结果淘汰。跨轮记忆完全失效——模型每轮都从零开始理解任务。

修法：`TailTokenBudget` 必须随 `TokenBudget` 联动，建议 `min(cfg.TailTokenBudget, TokenBudget × 0.25)`；并把 `TailTokenBudget` / `LowWaterRatio` / `MaxSummaryTokens` 提升为 `SceneConfig` 可配字段。

### 2. system prompt 每轮乱序 —— KV 缓存前缀每轮作废

`internal/skill/loader.go:33-41` 的 `Frontmatter()` 直接 `for _, s := range l.skills` 遍历 map，**没有排序**。同文件的 `Names()`（:66-72）和 `ReferenceNames()`（:75-85）都做了 `sort.Strings`，唯独这个漏了。

叠加 `internal/api/handler.go:45`：每个 HTTP 请求都 `core.NewAgent` → 重新 `cfg.Assemble()` → 重新拼 system prompt。Go 的 map 迭代顺序每次随机，所以**每一轮请求拼出的技能索引顺序都不同**。

system prompt 是 prompt 的最前段，它一变，整个前缀哈希失配，DeepSeek 侧 97%–99.3% 的缓存命中率直接归零，每轮都按全价重算全部输入。这是当前**单条最贵的 bug**，而修复成本是一行 `sort`。

修法：`Frontmatter()` 按 `Name` 排序后再拼；并给 `Loader` 增加稳定的注册序号，让索引顺序与注册顺序一致而非字典序（便于人工控制重要技能靠前）。

### 3. 单条巨型工具结果无人处理 —— keepRecent 变成免死金牌

`internal/context/engine.go:262-264`：

```go
if len(toolIdxs) <= keepRecent {   // keepRecent = 4，硬编码
    return messages
}
```

工具结果少于 4 条时，无论超窗多严重都**原样返回**。而爆窗最常见的成因恰恰是「一两条巨型结果」（一次大文件读取、一次全量 JSON 查询）。此时 `toolIdxs` 只有 1-2 条，压缩直接放弃。

更进一步：即使超过 4 条，最近 4 条也受 `keepRecent` 保护（engine.go:272）。如果第 4 新的那条就是 50k 字符的巨型结果，它永远不会被碰。

修法（对应 DeepSeek 的 `outputLadder` 头尾采样）：给 `evictToolResults` 加一条独立分支——**单条估算 token 超过阈值（如 softLimit 的 15%）的结果，即使在 keepRecent 保护区内，也做「头尾保留 + 中间折叠」的采样压缩**，而不是整体淘汰。采样保留头尾能让模型知道数据形状，中间用一行 metadata 替代（原文仍进 SpillStore 可回读）。这正是 `dsh-token-saver` 的默认策略。

---

## 二、时机前移：把 spill 从「超窗后补救」改成「出生点分流」

### 4. 工具结果应在产生时就减负，而不是等超窗

现状：`SpillStore` + `read_result` 闭环已经很完整（`internal/context/spill.go`、`internal/tool/builtin/read_result_tool.go`），但**唯一的触发点在 `evictToolResults` 内部**——也就是必须等到 `effectiveTokens > softLimit` 才开始 offload。

代价：一条 50k 字符的结果会原封不动进上下文，然后跟着走完剩余的所有 ReAct 轮次。假设它产生后还跑了 5 轮，这 5 轮每轮都为它付全价 prompt。等压缩终于触发时，钱已经花完了。

DeepSeek 的 `outputLadder` 是在工具输出「出生点」就分流：错误结果压成 300 字符摘要、大 JSON 做结构感知压缩、shell 输出做头尾采样，**原文一律落盘可逆**。

修法（改动面很小，ROI 最高的一项）：在 `internal/harness/core/builder.go` 的 ToolsNode 之前/之后加一层 `tool.BaseTool` 包装器（与已有的 `ApprovalWrapper` 同构，可复用 `agenttool.WrapTools` 的接线方式），在 `InvokableRun` 返回处按规则分流：

| 结果特征 | 处理 | 上下文占用 |
|---|---|---|
| 执行报错 | 只留错误类型 + 首 300 字符 | ~150 token |
| > N 字符的 JSON | 保留 schema 骨架 + 前若干条 + 计数 | 可降 80%+ |
| > N 字符的纯文本 | 头 2k + 尾 1k + 中间 metadata 行 | 可降 90% |
| 小于阈值 | 原样透传 | 不变 |

原文全部写 `SpillStore`，压缩后的内容尾部附 `tool_call_id`，模型需要全量时用现成的 `read_result` 取回。**已有的回读链路一行都不用改**。

### 5. read_result 单页 50k 字符与预算冲突，且不支持分页

- `internal/harness/core/agent.go:98` `readResultMaxChars = 50000`（注释自估约 25k token）
- `examples/release_scene/main.go:352` `TokenBudget: 16384`

一次回读就能把整个 Turn 预算顶爆近两倍。而且截断是「砍尾巴」（read_result_tool.go:82-87），模型拿不到后半段，也不知道后面还有什么。

修法：
- 页大小动态化：`min(配置上限, budget.Remaining() / 4)`，从 context 里取 Budget（`budget.FromContext`）
- `read_result` 增加 `offset` / `limit` 参数，支持分页回读
- 截断时同时返回**中段结构摘要**（如 JSON 的顶层 key 列表），而不是只说「已截断」

### 6. SpillStore 是 Agent 实例级，跨 Turn 回读必然失败

`internal/harness/core/agent.go:58` 在 `NewAgent` 内部 `NewMemorySpillStore()`。注释自己也承认「作用域为单个 Agent 实例（即单次 Turn）」。

配合 `handler.go:45` 每请求重建 Agent，意味着：本 Turn offload 的原文，下一 Turn 随旧 Agent 一起被 GC。当前因为 RawHistory 不存 tool_result（见 §4.7），stub 也不跨轮存活，所以问题被掩盖了——但一旦修好 RawHistory，这个洞会立刻暴露成「模型按 stub 提示去回读，拿到 ref not found」。

修法：`SpillStore` 由 `SceneConfig` 或 `Handler` 注入（进程级单例 / Redis / 对象存储），而不是 Agent 自建。ref 已经用 `toolresult:<tool_call_id>` 做成稳定的（spill.go:27-29），换存储实现不需要改 ref 生成逻辑。

---

## 三、缓存命中率：拉开压缩间距 + 补齐观测

### 7. softLimit 80% / lowWater 65% 间距太窄，前缀反复作废

`internal/context/engine.go:17-18`。触发线 80%，一次清到 65%，间距只有 15%。长会话里几轮就又顶到 80%，于是**每隔几轮就改写一次历史中段，前缀反复失配**。

DeepSeek 的 `compactionDriver` 主动在 **45%** 压力就驱动压缩，正是为了拉开这个间距：一次性付费，随后多轮命中缓存。

修法：
- `LowWaterRatio` 降到 0.4–0.5（可配）
- 引入 **watermark 冻结**：记录上次压缩覆盖到的消息位置，该位置之前的内容不再重复改写。这样前缀在两次压缩之间是字节级稳定的
- 压缩后发出 `ContextCompacted` 信号（见 §8）

### 8. 没有缓存命中率指标，所有「稳前缀」的努力都无法验证

`internal/harness/budget/budget.go:12-16` 的 `TokenUsage` 只有三个字段：

```go
type TokenUsage struct {
    PromptTokens     int
    CompletionTokens int
    TotalTokens      int
}
```

**没有 `CachedTokens`**。DeepSeek 侧 97%–99.3% 的命中率是靠持续观测驱动的；agentrix 现在既看不到命中率，也看不到「压缩省了多少」。上面第 2、7 条这类问题之所以能潜伏，正是因为没有指标暴露它们。

同时 `internal/projection/signal.go:16` 声明了 `ContextCompacted` 信号类型，但**全仓库无任何发出点**（grep 确认只有定义处）。

修法（这是所有优化的前置条件，建议第一个做）：
- `TokenUsage` 增加 `CachedTokens int`，`recordUsage`（model_decorator.go:222-234）从 `ResponseMeta.Usage` 里读 provider 返回的 cache 命中字段
- `Budget` 暴露 `CacheHitRate()`，进 `BudgetSnapshot`
- `CompressInPlace` / `compress` 实际发生改写时发出 `ContextCompacted` 信号，payload 带上「压缩前后 token 数、淘汰条数、offload 条数、当前前缀 watermark」

没有这个埋点，后续任何调参都是盲调。

### 9. token 估算 /2 对 JSON 高估近 2 倍，导致压缩过早触发

`internal/context/engine.go:633-638` 与 `internal/harness/core/model_decorator.go:350-355`（两份重复实现）：

```go
return len([]rune(s))/2 + 1   // 约 2 字符 = 1 token
```

实际分布：英文 / JSON / 代码约 **3.5–4 字符 = 1 token**，中文约 **1–1.5 字符 = 1 token**。所以这个公式对工具结果（绝大多数是 JSON、日志、代码）**高估约 1.8 倍**，对中文对话**低估约 2 倍**。

高估的直接后果：`effectiveTokens` 虚高 → 压缩在远未真正接近窗口时就触发 → 无谓改写历史 → 破坏缓存前缀。**估算不准会直接转化为真金白银的缓存损失**，不只是数字难看。

修法（低成本版，不必上 tiktoken）：分别统计 CJK 与 ASCII 码点数，`tokens ≈ cjk × 0.7 + ascii × 0.28`。同时把两份重复实现合并为 `context` 包的单一导出函数，避免继续分叉。

---

## 四、输入端做减法：懒加载与档位

### 10. 工具 schema 只有场景级静态裁剪，没有按轮次懒加载

`internal/scene/assemble.go:66-73` 的白名单过滤是**装配期一次性**的：一个场景内所有放行工具的 schema，每一轮都全量送给模型。

DeepSeek 的 `toolTrim` / `dsh-mcp-lazy` 实测**每请求省约 9.4k token**——靠的是把当前轮次用不到的工具 schema 藏起来，需要时再加载。

agentrix 的工具规模（release_scene 是 3 个业务工具 + read_skill + read_result）还看不出痛感，但框架定位是「Agent 应用框架」，接入方挂 20+ MCP 工具是常态，届时 schema 会成为最大的单项固定开销。而且 `computeOverhead`（agent.go:104-128）已经在精确统计这块了，说明架构上认了它是成本项，只是没做优化。

修法（两级）：
- **近期**：把 `read_result` 改为条件注册——只在 `SpillStore` 里确实有内容时才追加（现在 agent.go:63 是无条件 append），并在没有技能时不注册 `read_skill`
- **中期**：引入「工具组」概念，system prompt 里只暴露组名 + 一句话用途（几十 token），模型选中组后再动态绑定该组的完整 schema。这需要 eino 侧支持轮次间变更 bound tools，`BudgetModel.WithTools`（model_decorator.go:209-215）已经是每轮透传的，具备改造点

### 11. 技能索引全量注入，无场景相关性筛选

`internal/scene/assemble.go:38-55` 把 `Skills.Frontmatter()` 的**全部**技能无条件拼进 system prompt。技能一多就线性膨胀，且这部分位于缓存前缀最前段——虽然稳定（修好 #2 之后），但是纯固定成本。

DeepSeek 的「极简模式」思路是做减法：把上下文从 1 万+ 压到 1000+。

修法：
- `SceneConfig` 增加 `EnabledSkills []string`，只注入该场景相关技能的 frontmatter（与 `AllowedCommands` 同构，一致性最好）
- 提供 `SceneConfig.Minimal bool` 档位：不注入技能索引、不挂 read_skill / read_result、跳过 Catalog 包装，用于简单任务的轻量路径

### 12. 重试时用同一份超大输入反复付费

`internal/harness/core/model_decorator.go:115-132`：`generateWithRetry` 用完全相同的 `input` 重试 `MaxRetries` 次（默认 3）。

`isRetryable`（:282-317）对超时 / 网络错误 / 5xx 返回 true——而这些错误里，**有相当比例是「输入太大导致处理超时」**。此时用同样的超大输入重试 3 次，等于把最贵的 prompt 付 4 遍，且大概率全部失败。

修法：从第 2 次重试起，若错误属于超时类，先强制调用一次 `Engine.CompressInPlace`（临时把 softLimit 下调）缩减输入再重试。降级到 `FallbackModel` 时（:135-139）同理——降级模型往往窗口更小，更应该先减负。

### 13. Stream 路径不记用量，流式场景下预算控制形同虚设

`internal/harness/core/model_decorator.go:166-176`：`Stream` 只做输入压缩后直通委托，**不调 `recordUsage`、不 `RecordProgress`、不检查 `IsExhausted`**。

如果生产走流式（Agent 框架的常态），整个 token 账本是空的：预算不会耗尽、无进展检测不生效、用量统计全为 0，第 8 条要加的缓存命中率指标也拿不到数据。

修法：包装返回的 `StreamReader`，在 `Recv` 到最后一帧（或 reader 关闭）时提取 `ResponseMeta.Usage` 记账。注释里提到的「defer cancel 会切断 reader」是真问题，但记账不需要 cancel——只需在 reader 消费完成的回调里做。

---

## 五、分层记忆：用结构化状态换 token（DeepSeek 省 65%–96% 的核心）

### 14. RawHistory 不完整 —— 跨轮重复调用工具

`internal/harness/core/agent.go:215-219` 只写入 `userInput` + 最终 `output`，**图内所有 tool_call / tool_result 全部丢弃**。

直接后果：下一轮模型看不到上一轮查过什么，只能**重新调用同样的工具**。重复的工具调用是双重浪费——既付工具结果重新进上下文的 prompt 钱，又付额外的 ReAct 轮次。这比上下文里多留几 k token 贵得多。

同时 `session.Store` 注释（store.go:10-18）明确写了设计意图是「SessionStateV1 是工作记忆，RawHistory 是完整审计记录，两者通过游标关联」——现在 RawHistory 既不完整，游标也没在用。

修法：在 Graph 里挂 callback（`compose.WithGenLocalVariable` 或 eino 的 callbacks）捕获工具节点的输入输出，`Append` 到 RawHistory（`HistoryStore.Append` 接口已存在，history.go:44-49，无人调用）。

### 15. HistoryCursor 语义错误且只写不读

`internal/session/state.go:18`：

```go
s.HistoryCursor = len(messages)   // messages 是压缩后的装配产物
```

游标被设成**压缩后**的消息长度，而它应该指向**原始历史**中「已消费到哪条」。两者长度不同（压缩会改写、合并、丢弃），所以这个值没有意义。

且它**从未被读取**：`agent.go:180` 每轮都 `LoadHistory` 全量加载，不看游标。设计文档里「通过游标关联」的机制完全没接线。

修法：`UpdateFromTurn` 接收原始 history 长度而非压缩后 messages；`LoadHistory` 改为按游标增量加载（`HistoryStore` 增加 `LoadFrom(ctx, sessionID, cursor)`）。

### 16. Todo / Reminder / SkillRuntime 是死字段 —— 最划算的「结构化换 token」机会

`internal/session/state.go:8-10` 三个字段：
- `SkillRuntime`：只在 `LoadState`（store.go:47）和 `UpdateFromTurn`（state.go:14-16）里被初始化成空 map，**从不写入内容、从不注入上下文**
- `Todo` / `Reminder`：**全仓库除定义外零引用**

DeepSeek 的分层记忆之所以能省 65%–96%，关键是「**结构化查询不烧 token**」：模型不需要读回 20 轮对话才知道进展到哪，读一个 5 行的 todo 列表就够。

agentrix 这三个字段的类型已经定义好了，只差接线，而收益很直接：

| 字段 | 注入方式 | 替代掉的 token |
|---|---|---|
| `Todo` | 每轮装配时作为一条 system 消息注入（几十 token） | 模型不必从长历史里重建「做到哪一步」 |
| `SkillRuntime` | 记录已读过的 skill / reference 名，装配时注入一行「已加载：X, Y」 | **避免模型重复调用 `read_skill` 读同一份文档**——技能正文动辄数千 token，重复读一次就是实打实的浪费 |
| `Reminder` | 任务级约束（如「不得超过 20% 灰度」）固化在此，替代反复出现在历史里的自然语言叮嘱 | 减少历史膨胀 |

其中 `SkillRuntime` 去重是立竿见影的：现在每轮装配都是全新 Agent，模型完全不知道自己上一轮读过哪个 reference，很可能再读一遍。

修法：新增 `write_state` / `read_state` 内建工具让模型维护 Todo；`ReadSkillTool`（skill_tool.go:70-114）在返回前把 `name + reference` 写入 `SkillRuntime`，装配时把已读清单注入 system prompt 尾部。

### 17. 压缩成果不跨轮复用 —— 每轮重新付一遍 LLM 摘要的钱

因为 `CompressInPlace` 改的是图内 `input`、不回写 history，而 `Assemble` 每轮都从原始 history 重新装配，所以：

**每轮都要重新走一遍「加载全量历史 → 判断超窗 → 淘汰工具结果 → LLM 摘要」**。`MemorySummary` 虽然存在 `State` 里跨轮保留了，但因为第 1 条（middle 恒空）它根本没被写过；即使修好，每轮的 middle 边界也在变，摘要会反复重算。

DeepSeek 的做法是「历史该滚动就滚动，但提炼过的记忆按需注入，而不是每次全量携带」。

修法：压缩结果落盘。`State` 增加 `CompactedHistory []*schema.Message` + `CompactedUpTo int`（原始历史中的位置），下一轮 `Assemble` 直接从压缩产物起步，只追加新增部分。这样既省了重复摘要的 LLM 调用，也让前缀在多轮之间保持字节级稳定（配合 §7 的 watermark 冻结）。

### 18. 无主动压缩驱动器 —— 大窗口下压缩永不触发

`CompressThreshold` 默认 0.8。若接入方配 `TokenBudget: 1000000`（DeepSeek 长上下文场景），softLimit = 800k，一个正常会话根本到不了，压缩永不发生，历史无限膨胀。

DeepSeek 的 `compactionDriver` 在 agent idle 时按自定义压力比（默认 45%）主动驱动。

修法：`Agent.Run` 结束后（`agent.go:220-226` 保存 history 处）加一次 idle 压缩：若 `effectiveTokens(history) > TokenBudget × IdleCompactRatio`（默认 0.45），异步触发一次完整压缩并落盘（配合第 17 条）。

---

## 六、其他正确性问题（间接影响 token）

### 19. `MemoryStore.LoadHistory` 返回内部 slice，未拷贝

`internal/session/store.go:57-61` 直接 `return s.history[sessionID], nil`。对比 `MemoryHistoryStore.Load`（history.go:26-33）是做了 copy 的——两个 store 行为不一致。

`agent.go:218` 拿到后 `history = append(history, ...)`，若底层数组 cap 足够，会直接写穿到 store 内部状态。并发请求同一 session 时历史会互相污染。

被污染的历史进上下文 = 付钱买了错的东西。修法：`Load` 时 copy，`Save` 时 copy（`MemoryStore.SaveHistory` :63-68 同样直接赋值未拷贝）。

### 20. `fixToolCallPairs` 补的占位符不可回读

`internal/context/engine.go:570-576` 为悬空的 tool_call 补占位符，内容是「[该工具结果已被上下文压缩省略]」——**没有 tool_call_id、没有 spill ref**。

而 `buildStub`（engine.go:300-331）产出的 stub 是带 ref 和恢复路径的。两条路径产出的占位符能力不一致：走 `fixToolCallPairs` 补出来的那些，模型无法回读，等于永久丢失。

修法：`fixToolCallPairs` 复用 `buildStub` 的格式（此处 `tc.ID` 是已知的，可以生成同样的 ref）。

### 21. `PromptOverheadSnapshot.Multimodal` 是死字段

`internal/context/overhead.go:26` 定义了 `Multimodal int`，但 `computeOverhead`（agent.go:104-128）**只赋值 `SystemTokens` 和 `ToolsTokens`**，`Multimodal` 恒为 0。

消息级的多模态估算在 `estimateMultiContentTokens`（engine.go:612-630）里做了，所以功能上不缺失，但这个字段是未接线的残留。要么删掉，要么在装配期把已知的固定图片开销填进去。

### 22. `Compressor` 是死代码，且绕过 need-driven 纪律

`internal/context/compressor.go` 的 `NewCompressor` **全仓库无调用点**（grep 确认），逻辑与 `Engine.compress`（engine.go:184-221）重复。

风险在于：`Compressor.Compress` **没有 `shouldCompress` 判断**，是无条件压缩。一旦有人误用，就绕过了刚建立的 need-driven 纪律，每轮改写历史、每轮作废前缀——正是 `CompressInPlace` 注释（engine.go:137-143）里明确警告要避免的行为。

修法：删掉，或让它内部委托给 `Engine.Assemble`。留着比没有更危险。

### 23. `TokenBudget` 一字段两义

`SceneConfig.TokenBudget` 同时被用作：
- `internal/harness/core/agent.go:42` → `engine.TokenBudget`，即**模型上下文窗口**（容量）
- `internal/harness/core/agent.go:169` → `budget.NewBudget(...)`，即**单 Turn 累计消耗上限**（流量）

这两个是完全不同的量。配成 16384 意味着：窗口 16384，同时整个 Turn 累计只能花 16384。而 ReAct 每轮都要重付全量 prompt，8 轮下来累计轻松破 5 万——**预算会在正常执行中途耗尽**，返回 `ErrBudgetExhausted`，用户看到的是任务莫名失败。

修法：拆成 `ContextWindow int` 和 `TurnTokenBudget int` 两个字段，`TokenBudget` 保留为 deprecated 别名以兼容。

---

## 七、DeepSeek 设计 → agentrix 现状对照

| DeepSeek 机制 | agentrix 现状 | 缺口 | 优先级 |
|---|---|---|---|
| 极简模式（1万+ → 1000+ token） | 无档位概念，全量装配 | 缺 `Minimal` 档位 / 条件注册框架工具 | 中（§11） |
| `outputLadder` 出生点分流 | spill 只在超窗后触发 | **触发时机太晚**，巨型结果一路付费 | **高（§4）** |
| `toolTrim` / MCP 懒加载（省 ~9.4k/请求） | 仅场景级静态白名单 | 无按轮次动态可见性 | 中（§10） |
| `text2img` 长文本转图 | 无 | 视觉编码 token 效率，Go 侧成本高 | 低 |
| `ctx.compaction` 滑窗 + 45% 主动驱动 | 80% 被动触发，无 idle 驱动 | 间距窄、大窗口下永不触发 | 高（§7 §18） |
| `dsh-compaction-pro` 保留精确值 | 七字段摘要模板已有，但**从不执行** | TailTokenBudget 失配 | **最高（§1）** |
| 前缀稳定 / 缓存命中 97%–99.3% | system prompt 每轮乱序 | **map 遍历未排序** | **最高（§2）** |
| 缓存命中率观测 | `TokenUsage` 无 `CachedTokens` | 无法验证任何优化效果 | **高（§8）** |
| 分层记忆（省 65%–96%） | `State` 字段齐全但全是死的 | Todo / SkillRuntime 未接线 | 高（§16） |
| 记忆按需注入，不全量携带 | 每轮 `LoadHistory` 全量 | 游标语义错且只写不读 | 高（§15） |
| 历史滚动 + 压缩成果复用 | 压缩产物不落盘，每轮重算 | 重复付 LLM 摘要费 | 高（§17） |

---

## 八、建议推进顺序

**第一批（半天内可完成，收益立竿见影）**
1. §2 `Frontmatter()` 加排序 —— 一行代码，直接救回缓存命中率
2. §1 `TailTokenBudget` 与 `TokenBudget` 联动 —— 让 LLM 摘要路径真正跑起来
3. §8 `TokenUsage` 加 `CachedTokens` + 发出 `ContextCompacted` 信号 —— 没有这个，后面全是盲调
4. §9 token 估算按 CJK/ASCII 分别加权，合并两份重复实现
5. §22 删掉 `Compressor` 死代码；§21 清理 `Multimodal` 死字段

**第二批（1–2 天，结构性收益最大）**

6. §4 工具结果出生点分流 —— 复用已有 SpillStore + read_result，改动集中在一个包装器
7. §3 巨型单条结果的头尾采样，破掉 `keepRecent` 免死金牌
8. §16 接线 `SkillRuntime`（技能去重）+ `Todo` 注入 —— 用结构化状态换 token
9. §23 拆分 `ContextWindow` / `TurnTokenBudget`
10. §5 `read_result` 分页 + 动态页大小；§6 `SpillStore` 提升到进程级

**第三批（需要设计，治本）**

11. §14 §15 补全 RawHistory + 修正游标语义 + 增量加载
12. §17 压缩产物落盘，跨轮复用
13. §7 watermark 冻结 + lowWater 降到 0.45；§18 idle 主动压缩
14. §10 工具组懒加载；§11 `Minimal` 档位
15. §13 Stream 路径记账；§12 重试前减负

---

## 附：说明

- 本文所有代码位置基于当前工作区状态（`git status` 显示 `internal/context/`、`internal/harness/core/` 有未提交改动，已完成的三项优化在工作区中）。
- §1、§2、§3、§5、§23 的问题是通过读取代码 + 配置常量推导的确定性结论，可直接复现。
- §4、§7、§17 的 token 节省幅度是参照 DeepSeek 侧公开数据的**定性推断**，agentrix 侧的实际收益取决于接入方的工具结果规模分布，需要先落地 §8 的观测埋点再实测。
- §9 的 token/字符系数（CJK ≈ 0.7、ASCII ≈ 0.28）是主流 BPE 分词器的经验值，非精确常数；接入方若使用特定模型，应以该模型 tokenizer 实测为准。
