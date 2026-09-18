package context

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/daqiaoliang-coder/agentrix/internal/session"
)

const (
	defaultTokenBudget     = 128000
	defaultCompressRatio   = 0.8
	defaultLowWaterRatio   = 0.65
	defaultTailTokenBudget = 20000
	defaultSummaryRatio    = 0.2
	defaultMaxSummaryToken = 12000
	trimKeepRecentTools    = 4

	// oversizedResultRatio 判定「超尺寸工具结果」的阈值比例（相对 softLimit）。
	// 单条结果的估算 token 超过 softLimit × 该比例，即视为超尺寸，允许对它做
	// 头尾采样——即使它落在 keepRecent 保护区内。详见 sampleOversizedResults。
	oversizedResultRatio = 0.15

	// sampleHeadChars / sampleTailChars 是采样后保留的头尾字符数。
	// 保留头尾而非整体淘汰，是为了让模型仍能看到数据的形状（字段名、结构、
	// 开头与结尾的实际取值），中间段用一行 metadata 替代。
	sampleHeadChars = 1024
	sampleTailChars = 512

	// sampleMetaSlack 是中间 metadata 行的字符余量。三者之和构成「采样后最小体积」：
	// 原文不超过它时采样没有意义（可能反而变大），直接跳过。
	sampleMetaSlack = 160

	// maxTailRatio 是尾部保护区占 TokenBudget 的上限比例。
	//
	// 为什么必须有这个约束：splitByBoundary 从末尾向前累加，直到超过 TailTokenBudget
	// 才划出尾部边界。若 TailTokenBudget ≥ 触发压缩时的消息总量，tailStart 会一路
	// 退到 0，middle 恒为空，compress 直接提前返回——四阶段压缩里的「结构化摘要」
	// 阶段永远不执行，MemorySummary 永远是空串，跨轮记忆静默失效。
	//
	// 压缩在 effectiveTokens 越过 softLimit（TokenBudget × CompressThreshold）时触发，
	// 此刻消息总量必然小于 TokenBudget。因此只要 TailTokenBudget < TokenBudget，
	// 就至少存在可摘要的中间段。取 0.25 是留出余量：默认 CompressThreshold=0.8 时，
	// 中间段至少能占到消息总量的约 3/4。
	maxTailRatio = 0.25
)

// Engine 负责上下文装配与压缩
type Engine struct {
	TokenBudget       int     // 模型上下文窗口预算
	CompressThreshold float64 // 触发压缩的软阈值 softLimit（占 TokenBudget 比例）
	LowWaterRatio     float64 // 低水位：一次清理到此水位以下才停（占 TokenBudget 比例）
	TailTokenBudget   int     // 尾部保护区期望大小；实际生效值经 tailBudget() 收敛，不超过窗口的 maxTailRatio
	MaxSummaryRatio   float64 // 摘要占被压缩内容的上限比例
	MaxSummaryTokens  int     // 摘要 token 绝对上限

	// IsNonIdempotent 判定某工具是否为写/非幂等操作（如 edit/create/delete）。
	// 返回 true 的工具结果不参与淘汰：这类结果无法重放（重跑会重复执行副作用），
	// 也没有可回读的原文语义，只能保留在上下文中。nil 表示全部按可淘汰处理。
	// 由上层（harness/core）从 Catalog 的 NeedsApproval 元数据接线，
	// context 包不反向依赖工具注册表。
	IsNonIdempotent func(toolName string) bool

	summaryModel model.BaseChatModel // 用于结构化摘要，可为 nil（降级为规则摘要）

	// OnCompacted 是可选的压缩观测钩子：每次压缩事件结束后同步调用一次。
	//
	// 为什么必须有它：压缩是「收益容易测、代价难测」的典型——省下的 token 看得见，
	// 丢信息导致的失败率上升、摘要 LLM 自身的开销、缓存前缀被作废的次数，全都发生在
	// 这一瞬间。没有观测点，任何调参都是盲调，也无法区分「提升来自算法」与「提升来自
	// 多花了算力」。
	//
	// 钩子给出压缩前后的 token、采样与淘汰条数、摘要开销、不可恢复丢失条数与前缀
	// watermark。外层据此桥接为 projection.ContextCompacted 信号或指标系统。
	// context 包不反向依赖 projection，避免装配层与观测层形成环。
	//
	// 钩子必须非阻塞、不得长期持有 stats 之外的资源；落地为信号或指标由外层负责。
	OnCompacted func(stats CompressStats)

	// mu 保护 overhead：装配期写入、循环内读取，避免数据竞争。
	mu       sync.RWMutex
	overhead PromptOverheadSnapshot

	// spill 承接被淘汰的工具结果原文，使淘汰可恢复。nil 表示退化为不可恢复裁剪。
	spill SpillStore
}

func NewEngine() *Engine {
	return &Engine{
		TokenBudget:       defaultTokenBudget,
		CompressThreshold: defaultCompressRatio,
		LowWaterRatio:     defaultLowWaterRatio,
		TailTokenBudget:   defaultTailTokenBudget,
		MaxSummaryRatio:   defaultSummaryRatio,
		MaxSummaryTokens:  defaultMaxSummaryToken,
	}
}

// NewEngineWithModel 注入摘要模型，启用 LLM 结构化摘要
func NewEngineWithModel(m model.BaseChatModel) *Engine {
	e := NewEngine()
	e.summaryModel = m
	return e
}

// SetOverhead 注入固定开销快照（system prompt / 工具 schema / 多模态预留）。
// 由装配层在确定 active toolset 后调用一次；toolset 变更时应重新计算并注入。
func (e *Engine) SetOverhead(s PromptOverheadSnapshot) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.overhead = s
}

// Overhead 返回当前开销快照。
func (e *Engine) Overhead() PromptOverheadSnapshot {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.overhead
}

// SetSpillStore 注入溢出存储，启用「offload 而非 delete」的可恢复淘汰。
func (e *Engine) SetSpillStore(s SpillStore) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.spill = s
}

// effectiveTokens 是压缩决策的唯一依据：消息本体 + 固定开销。
// 只看 estimateTokens(messages) 会漏掉工具 schema 等不在消息列表里的真实输入，
// 导致阈值失真、压缩触发过晚。
func (e *Engine) effectiveTokens(messages []*schema.Message) int {
	return EstimateMessagesTokens(messages) + e.Overhead().Total()
}

// softLimit 返回触发压缩的 token 线。
func (e *Engine) softLimit() int {
	return e.thresholdAt(e.CompressThreshold, defaultCompressRatio)
}

// lowWater 返回一次清理要达到的低水位 token 线。
// 低水位必须严格低于软阈值，否则清理后立即再次触发，等于每轮都破坏缓存前缀。
func (e *Engine) lowWater() int {
	low := e.thresholdAt(e.LowWaterRatio, defaultLowWaterRatio)
	if soft := e.softLimit(); low >= soft {
		low = soft * 3 / 4
	}
	if low <= 0 {
		low = 1
	}
	return low
}

// thresholdAt 把比例换算为 token 绝对值，对非法配置回落到默认值。
func (e *Engine) thresholdAt(ratio, fallback float64) int {
	budget := e.TokenBudget
	if budget <= 0 {
		budget = defaultTokenBudget
	}
	if ratio <= 0 || ratio > 1 {
		ratio = fallback
	}
	return int(float64(budget) * ratio)
}

// tailBudget 返回实际生效的尾部保护区大小：期望值与「窗口 × maxTailRatio」取较小者。
//
// 这一步收敛是「结构化摘要阶段能否执行」的开关。TailTokenBudget 的默认值 20000
// 是按 128k 窗口设定的，但接入方常把 TokenBudget 配成 16k 甚至 8k；此时 20000
// 大于整个窗口，middle 恒为空，LLM 摘要路径被静默跳过（详见 maxTailRatio 注释）。
// 在引擎内部收敛而不是要求每个接入方记得联动配置，是因为漏配不报错、只表现为
// 「压缩好像没效果」，排查成本远高于这里的一次 min。
func (e *Engine) tailBudget() int {
	budget := e.TokenBudget
	if budget <= 0 {
		budget = defaultTokenBudget
	}
	windowCap := int(float64(budget) * maxTailRatio)
	if windowCap <= 0 {
		windowCap = 1
	}
	want := e.TailTokenBudget
	if want <= 0 {
		want = defaultTailTokenBudget
	}
	if want > windowCap {
		return windowCap
	}
	return want
}

// CompressInPlace 对消息序列做规则压缩（工具结果淘汰 + 工具调用对修复），
// 不调用 LLM、不需要 session.State，适合在模型调用前对上下文做轻量维护。
// 这是 Agent.Run 级 Assemble（含 LLM 摘要）的补充。
//
// need-driven 纪律：仅当 effectiveTokens 越过 softLimit 才动手。
// 原先每轮无条件裁剪，会使裁剪前沿逐轮前移、每轮都改写同一段历史，
// 结果是可复用的 KV 缓存前缀被反复作废——省下的 token 还不够付缓存重建的成本。
// 改为按预算触发后，未超窗的轮次完全不动消息序列，缓存前缀保持稳定。
//
// 一旦触发，就一次清理到 lowWater 以下（clear_at_least），而不是清一条查一条：
// 一次性付费、随后多轮命中缓存。
//
// 未越 softLimit 时直接返回原序列且不发观测事件：这类轮次占绝大多数，
// 每次模型调用都发一条「未压缩」信号会淹掉真正需要看的事件。
// 反之，越过阈值却没改写序列（Triggered=false）会被如实上报——那正是
// §1「middle 恒空」和 §3「keepRecent 免死金牌」两类静默失效的特征。
func (e *Engine) CompressInPlace(ctx context.Context, messages []*schema.Message) []*schema.Message {
	out, _ := e.CompressInPlaceWithStats(ctx, messages)
	return out
}

// CompressInPlaceWithStats 是 CompressInPlace 的统计版本，额外返回本次压缩的账本。
//
// 之所以要「返回值」而不只依赖 OnCompacted 钩子：Engine 是跨 Turn 共享的单例，
// 钩子只能指向一个目标；而 Budget 是按 Turn 创建的。若靠钩子把账本塞进 Budget，
// 并发 Turn 之间会串号。返回值让调用方自己决定记到哪个 Budget 上，天然无竞态。
//
// 未越过 softLimit 时返回原序列且 stats.Triggered=false——这类轮次占绝大多数，
// 调用方据此可选择不记账，避免 CompressEvents 退化成「模型调用次数」。
func (e *Engine) CompressInPlaceWithStats(
	ctx context.Context,
	messages []*schema.Message,
) (out []*schema.Message, stats CompressStats) {

	before := e.effectiveTokens(messages)
	if before <= e.softLimit() {
		return messages, CompressStats{Path: "inplace", TokensBefore: before, TokensAfter: before}
	}

	stats = CompressStats{Path: "inplace", TokensBefore: before}
	defer func() {
		stats.TokensAfter = e.effectiveTokens(out)
		stats.Triggered = stats.Sampled > 0 || stats.Evicted > 0 || stats.Placeholders > 0
		e.notifyCompacted(stats)
	}()

	// 先采样再淘汰：采样不受 keepRecent 豁免，专治「一两条巨型结果撑爆窗口」——
	// 这正是 evictToolResults 因总数不足而直接放弃的那类场景。
	sampled, nSampled, nSampledOffloaded := e.sampleOversizedResultsCounted(ctx, messages)
	stats.Sampled = nSampled
	stats.Offloaded += nSampledOffloaded

	// 采样已把体量压到低水位以下时，不再淘汰任何整条结果：
	// 每淘汰一条就改写一段历史、作废一次缓存前缀，能不付就不付。
	if e.effectiveTokens(sampled) <= e.lowWater() {
		res, ph := e.fixToolCallPairsCounted(ctx, sampled)
		stats.Placeholders = ph
		stats.UnrecoverableLost = (stats.Sampled - nSampledOffloaded) + ph
		stats.StablePrefixTokens = stablePrefixTokens(messages, res)
		return res, stats
	}

	evicted, nEvicted, nEvictedOffloaded := e.evictToolResultsCounted(ctx, sampled, trimKeepRecentTools, e.lowWater())
	stats.Evicted = nEvicted
	stats.Offloaded += nEvictedOffloaded

	res, ph := e.fixToolCallPairsCounted(ctx, evicted)
	stats.Placeholders = ph
	// 不可恢复丢失 = 采样/淘汰中未成功 offload 的部分 + 配对补洞。
	// 这些内容既没有回读路径、也不体现在任何 token 节省里，是代价侧唯一
	// 无法用 token 衡量的量，直接对应下游任务失败与幻觉风险。
	stats.UnrecoverableLost = (stats.Sampled - nSampledOffloaded) + (stats.Evicted - nEvictedOffloaded) + ph
	stats.StablePrefixTokens = stablePrefixTokens(messages, res)
	return res, stats
}

// Assemble 装配模型可见上下文，必要时触发压缩
func (e *Engine) Assemble(
	ctx context.Context,
	systemPrompt string,
	state *session.State,
	history []*schema.Message,
	userInput string,
) ([]*schema.Message, error) {
	out, _, err := e.AssembleWithStats(ctx, systemPrompt, state, history, userInput)
	return out, err
}

// AssembleWithStats 是 Assemble 的统计版本，额外返回本次装配的压缩账本。
//
// 未触发压缩时返回的账本 Triggered=false。与 CompressInPlaceWithStats 同理，
// 用返回值而非钩子传递账本，是因为 Engine 跨 Turn 共享、Budget 按 Turn 创建，
// 靠钩子转发会在并发 Turn 间串号。
func (e *Engine) AssembleWithStats(
	ctx context.Context,
	systemPrompt string,
	state *session.State,
	history []*schema.Message,
	userInput string,
) (out []*schema.Message, stats CompressStats, err error) {

	messages := make([]*schema.Message, 0, len(history)+3)
	messages = append(messages, schema.SystemMessage(systemPrompt))
	if state != nil && state.Memory.Summary != "" {
		messages = append(messages, schema.SystemMessage(state.Memory.Summary))
	}
	messages = append(messages, history...)
	messages = append(messages, schema.UserMessage(userInput))

	before := e.effectiveTokens(messages)
	if !e.shouldCompress(messages) {
		return messages, CompressStats{
			Path: "assemble", TokensBefore: before, TokensAfter: before,
		}, nil
	}

	out, stats, err = e.compress(ctx, messages, state)
	if err != nil {
		return nil, stats, err
	}
	return out, stats, nil
}

// ----------------------------------------------------------------------------
// 四阶段压缩主流程（规则优先，LLM 仅调用一次）
// 1. 优先裁剪工具输出（最占空间、最易过期）
// 2. 保护头和尾（头定义任务起点，尾包含最新状态）
// 3. 中间段做结构化摘要（七个字段：目标、约束、进度、关键决策、相关文件、下一步、关键上下文）
// 4. 最后修复被切断的 tool_call/tool_result 配对
// ----------------------------------------------------------------------------

func (e *Engine) compress(
	ctx context.Context,
	messages []*schema.Message,
	state *session.State,
) (out []*schema.Message, stats CompressStats, err error) {

	before := e.effectiveTokens(messages)
	stats = CompressStats{Path: "assemble", TokensBefore: before}
	defer func() {
		if err != nil {
			return // 摘要失败时未产出任何压缩结果，不记账，避免污染统计
		}
		stats.TokensAfter = e.effectiveTokens(out)
		stats.Triggered = stats.Sampled > 0 || stats.Evicted > 0 ||
			stats.Placeholders > 0 || stats.SummaryTokens > 0
		stats.StablePrefixTokens = stablePrefixTokens(messages, out)
		e.notifyCompacted(stats)
	}()

	// 阶段①：工具结果减负（规则，不调 LLM，可 offload 回读）
	// 先采样巨型单条结果（不受 keepRecent 豁免），再按 oldest-first 淘汰整条。
	sampled, nSampled, nSampledOffloaded := e.sampleOversizedResultsCounted(ctx, messages)
	trimmed, nEvicted, nEvictedOffloaded := e.evictToolResultsCounted(ctx, sampled, trimKeepRecentTools, e.lowWater())
	stats.Sampled = nSampled
	stats.Evicted = nEvicted
	stats.Offloaded = nSampledOffloaded + nEvictedOffloaded

	// 阶段②：边界确定，保护头尾
	head, middle, tail := splitByBoundary(trimmed, e.tailBudget())
	if len(middle) == 0 {
		// middle 为空 = 结构化摘要阶段无事可做。这正是 §1「尾部保护区吞空 middle」
		// 的失效特征：SummaryTokens 为 0 会被如实上报，据此可发现阈值配错。
		fixed, ph := e.fixToolCallPairsCounted(ctx, trimmed)
		stats.Placeholders = ph
		stats.UnrecoverableLost = (nSampled - nSampledOffloaded) + (nEvicted - nEvictedOffloaded) + ph
		return fixed, stats, nil
	}

	// 阶段③：结构化摘要（仅调用一次 LLM，增量更新）
	var prevSummary string
	if state != nil {
		prevSummary = state.Memory.Summary
	}
	summary, modelTokens, viaLLM, sumErr := e.summarize(ctx, middle, prevSummary)
	if sumErr != nil {
		return nil, stats, sumErr
	}
	summary = e.truncateSummary(summary, middle)

	stats.SummaryTokens = EstimateTextTokens(summary)
	// 摘要 LLM 调用自身的开销记入账本。它是压缩的成本而非收益：
	// NetTokensSaved 会把它从毛节省里扣掉，否则净收益被系统性高估。
	stats.SummaryModelTokens = modelTokens
	stats.SummaryViaLLM = viaLLM

	if state != nil {
		state.Memory.Summary = summary
	}

	// 阶段④：工具调用对修复
	merged := make([]*schema.Message, 0, len(head)+1+len(tail))
	merged = append(merged, head...)
	merged = append(merged, schema.SystemMessage(summary))
	merged = append(merged, tail...)

	fixed, ph := e.fixToolCallPairsCounted(ctx, merged)
	stats.Placeholders = ph
	stats.UnrecoverableLost = (nSampled - nSampledOffloaded) + (nEvicted - nEvictedOffloaded) + ph

	return fixed, stats, nil
}

// ----------------------------------------------------------------------------
// 阶段判断
// ----------------------------------------------------------------------------

// shouldCompress 判断当前消息序列是否接近模型窗口上限。
// 依据是 effectiveTokens（消息 + 固定开销），而非仅消息本体——
// 工具 schema 由 eino 绑定给模型、不在 messages 里，漏算会让压缩触发得过晚。
func (e *Engine) shouldCompress(messages []*schema.Message) bool {
	return e.effectiveTokens(messages) >= e.softLimit()
}

// ----------------------------------------------------------------------------
// 阶段①：工具结果淘汰（offload 而非 delete）
// ----------------------------------------------------------------------------

// evictToolResults 把较早的工具结果替换为可操作的 stub，直到降到低水位以下。
//
// 四条纪律：
//  1. oldest-first：从最老的工具结果开始淘汰，最新的直接决定下一步动作，不能动；
//  2. keepRecent 保护：最近 keepRecent 条工具结果完整保留；
//  3. 跳过写/非幂等工具：这类结果无法重放（重跑会重复执行副作用），淘汰等于永久丢失；
//  4. clear_at_least：一次清到 lowWater 以下才停，避免逐条清理反复作废缓存前缀。
//
// 淘汰是 offload 而非 delete：配置了 SpillStore 时原文落入存储，stub 里带上
// 回读路径，模型可凭 tool_call_id 调 read_result 取回；未配置时退化为不可恢复裁剪，
// 但 stub 仍保留工具身份与原文规模，避免模型对着空占位符幻觉续编。
func (e *Engine) evictToolResults(
	ctx context.Context,
	messages []*schema.Message,
	keepRecent int,
	lowWater int,
) []*schema.Message {
	out, _, _ := e.evictToolResultsCounted(ctx, messages, keepRecent, lowWater)
	return out
}

// evictToolResultsCounted 是 evictToolResults 的计数版本，额外返回
// 「被替换为 stub 的条数」与「其中原文成功 offload、因而可回读的条数」。
//
// 两者之差就是不可恢复丢失：未配置 SpillStore，或 buildStub 内 offload 写入失败。
// 保留原签名的 evictToolResults 作委托，是为了不牵动既有调用点与测试。
func (e *Engine) evictToolResultsCounted(
	ctx context.Context,
	messages []*schema.Message,
	keepRecent int,
	lowWater int,
) (out []*schema.Message, evicted, offloaded int) {

	toolIdxs := make([]int, 0)
	for i, m := range messages {
		if m != nil && m.Role == schema.Tool {
			toolIdxs = append(toolIdxs, i)
		}
	}
	if len(toolIdxs) <= keepRecent {
		return messages, 0, 0
	}

	spill := e.spillStore()

	out = make([]*schema.Message, len(messages))
	copy(out, messages)

	// oldest-first：候选区间是除最近 keepRecent 条之外的全部工具结果
	for _, idx := range toolIdxs[:len(toolIdxs)-keepRecent] {
		orig := out[idx]

		// 已经是 stub 的不再处理（幂等，避免重复压缩同一条）
		if isStub(orig) {
			continue
		}
		// 写/非幂等工具结果不淘汰
		if e.nonIdempotent(orig.ToolName) {
			continue
		}

		stub := e.buildStub(ctx, orig, spill)
		if stub == nil {
			// offload 写入失败：宁可保留原文也不丢数据，故不计入任何淘汰数
			continue
		}
		out[idx] = stub
		evicted++
		// 只有原文确实落盘且有 tool_call_id 可寻址时，这条淘汰才是可恢复的。
		// 与 buildStub 内选择「恢复方式 / 不可回读」文案的条件严格一致。
		if spill != nil && orig.ToolCallID != "" {
			offloaded++
		}

		// clear_at_least：达到低水位就停，不追求清得更多
		if e.effectiveTokens(out) <= lowWater {
			break
		}
	}
	return out, evicted, offloaded
}

// buildStub 构造可操作的占位符。offload 成功时附带回读路径，失败/未配置时降级。
// 返回 nil 表示这条结果不应被淘汰（例如 offload 写入失败，宁可保留原文也不丢数据）。
func (e *Engine) buildStub(ctx context.Context, orig *schema.Message, spill SpillStore) *schema.Message {
	chars := len([]rune(orig.Content))
	var sb strings.Builder

	fmt.Fprintf(&sb, "[工具 %s 的结果已移出上下文·原始 %d 字符]", orig.ToolName, chars)

	if orig.ToolCallID != "" {
		fmt.Fprintf(&sb, "\n调用: %s", orig.ToolCallID)
	}

	if spill != nil && orig.ToolCallID != "" {
		ref := SpillRef(orig.ToolCallID)
		// offload 失败时不淘汰：可恢复性是这条路径的前提，写不进去就保留原文
		if err := spill.Spill(ctx, ref, orig.Content); err != nil {
			return nil
		}
		sb.WriteString("\n恢复方式: 调用 read_result 工具（传 tool_call_id=")
		sb.WriteString(orig.ToolCallID)
		sb.WriteString("）回读完整结果")
	} else {
		sb.WriteString("\n注意: 原始结果不可回读，如需该数据请重新调用工具")
	}

	return &schema.Message{
		Role:       orig.Role,
		ToolCallID: orig.ToolCallID,
		ToolName:   orig.ToolName,
		Name:       orig.Name,
		Content:    sb.String(),
		Extra:      map[string]any{extraKeyStub: true},
	}
}

// extraKeyStub 标记该消息已是 stub，用于幂等判断。
const extraKeyStub = "agentrix.context.stub"

// isStub 报告消息是否已经被淘汰过。
func isStub(m *schema.Message) bool {
	if m == nil || m.Extra == nil {
		return false
	}
	v, ok := m.Extra[extraKeyStub]
	if !ok {
		return false
	}
	b, _ := v.(bool)
	return b
}

// ----------------------------------------------------------------------------
// 阶段①补：超尺寸单条结果的头尾采样（outputLadder 式出生点减负的循环内兜底）
// ----------------------------------------------------------------------------

// sampleOversizedResults 对「单条就撑爆窗口」的巨型工具结果做头尾采样：
// 保留头 sampleHeadChars + 尾 sampleTailChars 字符，中间段折叠为一行 metadata，
// 原文整体 offload 到 SpillStore，凭 tool_call_id 可回读。
//
// 为什么需要它、且必须独立于 evictToolResults：
// evictToolResults 有两条硬约束——工具结果总数 ≤ keepRecent 时直接放弃，
// 且最近 keepRecent 条永不触碰。于是最常见的爆窗成因「一两条巨型结果」恰好
// 落在它的盲区里：要么总数不足触发不了，要么那条巨型结果正在保护区内。
// 采样专治这个盲区——它按「单条体积」而非「条数/新旧」判定，keepRecent
// 保护区内的巨型结果同样会被采样。
//
// 与淘汰一样遵循可恢复纪律：配置了 SpillStore 时原文落盘、采样体带恢复路径；
// 未配置时只对可重放（幂等）工具采样——因为采样会永久丢弃中段，而幂等工具
// 的结果模型可重新调用取回，非幂等工具则宁可保留原文也不冒险。
func (e *Engine) sampleOversizedResults(ctx context.Context, messages []*schema.Message) []*schema.Message {
	out, _, _ := e.sampleOversizedResultsCounted(ctx, messages)
	return out
}

// sampleOversizedResultsCounted 是 sampleOversizedResults 的计数版本，
// 额外返回「被采样的条数」与「其中原文成功 offload、因而可回读的条数」。
func (e *Engine) sampleOversizedResultsCounted(
	ctx context.Context,
	messages []*schema.Message,
) (out []*schema.Message, sampled, offloaded int) {

	threshold := int(float64(e.softLimit()) * oversizedResultRatio)
	if threshold <= 0 {
		return messages, 0, 0
	}
	spill := e.spillStore()

	// 采样体积随阈值自适应：头占阈值的一半、尾占四分之一（按 token 计），
	// 各自不超过 sampleHeadChars / sampleTailChars 的绝对上限。
	//
	// 为什么必须自适应而不是固定头尾长度：固定 1024+512 字符在大窗口下很合适，
	// 但小窗口场景下会高于阈值本身——采样完仍超阈，等于白做。
	// 换算字符数时按每条内容的实测密度进行，而不是假定某一种语言：
	// JSON / 代码类结果能保留约 3.6 倍于中文的字符量，两者都不会超预算。
	out = make([]*schema.Message, len(messages))
	copy(out, messages)

	changed := false
	for i, m := range out {
		if m == nil || m.Role != schema.Tool {
			continue
		}
		// 幂等：已处理过的（stub 或已采样）不再动，避免反复改写同一条、作废缓存前缀
		if isStub(m) {
			continue
		}
		if EstimateMessageTokens(m) <= threshold {
			continue
		}
		headChars, tailChars := sampleWindow(threshold, TextDensity(m.Content))
		// 原文尚未超过「头+尾+metadata」的体积时，采样没有意义（可能反而更大），跳过
		if len([]rune(m.Content)) <= headChars+tailChars+sampleMetaSlack {
			continue
		}
		// 未配置 spill 时，采样会永久丢失中段：非幂等工具结果不可重放，跳过不采
		if spill == nil && e.nonIdempotent(m.ToolName) {
			continue
		}

		res := e.buildSampled(ctx, m, spill, headChars, tailChars)
		if res == nil {
			// offload 写入失败：保留原文，不计入任何采样数
			continue
		}
		out[i] = res
		sampled++
		// 与 buildSampled 内选择「回读 / 不可回读」文案的条件严格一致
		if spill != nil && m.ToolCallID != "" {
			offloaded++
		}
		changed = true
	}
	if !changed {
		return messages, 0, 0
	}
	return out, sampled, offloaded
}

// sampleWindow 按超尺寸阈值与内容密度换算头尾各自的保留字符数。
//
// density 是该内容「每字符的 token 数」（见 TextDensity），用来把 token 预算反推为
// 字符预算。此前实现假定固定「2 字符 = 1 token」，对两类主流内容都会失真：
// 中文（密度约 0.7）会被砍得过多、丢掉有效信息，而 JSON / 代码（密度约 0.28）
// 会保留远超必要的字符、采样完仍超阈。按实测密度换算后两者都恰好落在预算内。
//
// 预算分配：头占阈值的一半 token、尾占四分之一 token，各自不超过
// sampleHeadChars / sampleTailChars 的绝对上限。density 非法时回落到最保守值，
// 宁可少留也不冒超阈的风险。
func sampleWindow(thresholdTokens int, density float64) (headChars, tailChars int) {
	if density <= 0 || density > 1 {
		density = tokensPerCJKChar
	}
	headChars = int(float64(thresholdTokens) / 2 / density)
	if headChars > sampleHeadChars {
		headChars = sampleHeadChars
	}
	tailChars = int(float64(thresholdTokens) / 4 / density)
	if tailChars > sampleTailChars {
		tailChars = sampleTailChars
	}
	if headChars < 1 {
		headChars = 1
	}
	if tailChars < 1 {
		tailChars = 1
	}
	return headChars, tailChars
}

// buildSampled 构造头尾采样后的工具结果消息。原文 offload 失败时返回 nil（保留原文）。
func (e *Engine) buildSampled(
	ctx context.Context,
	orig *schema.Message,
	spill SpillStore,
	headChars, tailChars int,
) *schema.Message {
	runes := []rune(orig.Content)
	total := len(runes)
	head := string(runes[:headChars])
	tail := string(runes[total-tailChars:])
	omitted := total - headChars - tailChars

	// 原文先落盘，再折叠：可恢复性是采样路径的前提，写不进去就保留原文。
	if spill != nil && orig.ToolCallID != "" {
		if err := spill.Spill(ctx, SpillRef(orig.ToolCallID), orig.Content); err != nil {
			return nil
		}
	}

	var sb strings.Builder
	sb.WriteString(head)
	fmt.Fprintf(&sb, "\n\n[…中间已省略 %d 字符（原文共 %d 字符）", omitted, total)
	if spill != nil && orig.ToolCallID != "" {
		fmt.Fprintf(&sb, "；如需完整内容，调用 read_result（传 tool_call_id=%s）回读]", orig.ToolCallID)
	} else {
		sb.WriteString("；此为采样结果，中段不可回读，如需请重新调用该工具]")
	}
	sb.WriteString("\n\n")
	sb.WriteString(tail)

	return &schema.Message{
		Role:       orig.Role,
		ToolCallID: orig.ToolCallID,
		ToolName:   orig.ToolName,
		Name:       orig.Name,
		Content:    sb.String(),
		// 标记为已处理：与 stub 共用 extraKeyStub，使采样/淘汰两条路径都跳过它，
		// 保证重复压缩幂等，不反复改写同一段历史。
		Extra: map[string]any{extraKeyStub: true},
	}
}

// nonIdempotent 判定工具是否为写/非幂等。未接线判定函数时全部视为可淘汰。
func (e *Engine) nonIdempotent(toolName string) bool {
	if e.IsNonIdempotent == nil || toolName == "" {
		return false
	}
	return e.IsNonIdempotent(toolName)
}

// spillStore 读取当前溢出存储（加锁，避免与装配期注入竞争）。
func (e *Engine) spillStore() SpillStore {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.spill
}

// ----------------------------------------------------------------------------
// 阶段②：边界确定
// ----------------------------------------------------------------------------

// maxTailShareOfMessages 是尾部保护区占「实际消息总量」的上限比例。
//
// 只按 TokenBudget 收敛尾部还不够。压缩是在 effectiveTokens（消息 + 固定开销）
// 越过 softLimit 时触发的，当 overhead 很大（庞大的系统提示 + 几十个工具 schema）
// 时，触发那一刻的消息本体可能只占窗口的一小部分。若尾部保护区仍按窗口比例算，
// 就会大于消息总量，middle 再次退化为空。
//
// 这里按「消息实际总量」再收一层：无论 overhead 多大，尾部最多占 60%，
// 剩下至少 40% 是可摘要的中间段。两道约束（窗口比例 + 消息比例）同时生效，
// 才能真正保证结构化摘要阶段有活可干。
const maxTailShareOfMessages = 0.6

// splitByBoundary 保护头部（System Prompt + 首次交互）和尾部（最近 tailBudget token），
// 中间段才是允许被改写的区域。
//
// tailBudget 传入的是期望值，函数内部会再按「消息实际总量 × maxTailShareOfMessages」
// 收敛一次，避免 middle 退化为空导致摘要阶段被静默跳过。
func splitByBoundary(
	messages []*schema.Message,
	tailBudget int,
) (head, middle, tail []*schema.Message) {

	if len(messages) == 0 {
		return nil, nil, nil
	}
	if tailBudget <= 0 {
		tailBudget = defaultTailTokenBudget
	}

	// 头部：起始连续的 System 消息 + 第一条非 System 消息（首次交互）
	headEnd := 0
	for headEnd < len(messages) && messages[headEnd].Role == schema.System {
		headEnd++
	}
	if headEnd < len(messages) {
		headEnd++
	}
	head = messages[:headEnd]

	if headEnd >= len(messages) {
		return head, nil, nil
	}

	// 尾部：从末尾向前累加，直到超过 tailBudget
	rest := messages[headEnd:]

	// 第二道收敛：按消息实际总量限制尾部占比，确保中间段非空。
	// 只在 rest 有 2 条以上时收——只有 1 条时无论怎么分都没有中间段可言。
	if len(rest) > 1 {
		if shareCap := int(float64(EstimateMessagesTokens(rest)) * maxTailShareOfMessages); shareCap > 0 && shareCap < tailBudget {
			tailBudget = shareCap
		}
	}
	tailStart := len(rest)
	used := 0
	for i := len(rest) - 1; i >= 0; i-- {
		t := EstimateMessageTokens(rest[i])
		if used+t > tailBudget && tailStart < len(rest) {
			break
		}
		used += t
		tailStart = i
	}

	middle = rest[:tailStart]
	tail = rest[tailStart:]
	return head, middle, tail
}

// ----------------------------------------------------------------------------
// 阶段③：结构化摘要
// ----------------------------------------------------------------------------

const summarySchemaPrompt = `你是对话上下文压缩器。请把给定的历史对话压缩为结构化摘要，
严格输出以下七个字段，每个字段一行，不要输出任何额外内容：

目标: <用户最终想达成什么>
约束: <必须遵守的规则、限制>
进度: <已经完成了什么>
关键决策: <做过哪些重要决定>
相关文件: <涉及的文件、表、资源>
下一步: <接下来应该做什么>
关键上下文: <其他必须保留的信息>`

// summarize 对中间段做结构化摘要。
// 第二次及以后在前一次摘要基础上增量更新，避免语义漂移。
//
// 除摘要文本外还返回两项观测信息：
//   - modelTokens：摘要 LLM 这次调用自身烧掉的 token。它是压缩的成本而非收益，
//     NetTokensSaved 会把它从毛节省里扣掉。不记这一项，压缩效果会被系统性高估，
//     且「摘要越做越贵」这种退化完全不可见。
//   - viaLLM：是否真的走了 LLM 结构化摘要路径。为 false 表示降级成了规则摘要
//     （只有「已省略 N 条消息」一句，跨轮记忆实际上是空的）——这是 §1 那类
//     静默失效的直接特征，必须能从数据里看出来。
//
// provider 未返回用量时按估算兜底，避免成本项恒为 0 而虚增净收益。
func (e *Engine) summarize(
	ctx context.Context,
	middle []*schema.Message,
	prevSummary string,
) (summary string, modelTokens int, viaLLM bool, err error) {

	if len(middle) == 0 {
		return prevSummary, 0, false, nil
	}

	// 无摘要模型时降级为规则摘要，保证链路可用
	if e.summaryModel == nil {
		return fmt.Sprintf(
			"[历史压缩] 已省略 %d 条较早消息，最早一条为 %s 角色。",
			len(middle), middle[0].Role,
		), 0, false, nil
	}

	var sb strings.Builder
	if prevSummary != "" {
		sb.WriteString("已有摘要（请在此基础上增量更新，不要从头重写）：\n")
		sb.WriteString(prevSummary)
		sb.WriteString("\n\n新增对话：\n")
	}
	for _, m := range middle {
		if m == nil {
			continue
		}
		content := m.Content
		if len(m.ToolCalls) > 0 {
			names := make([]string, 0, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				names = append(names, tc.Function.Name)
			}
			content += fmt.Sprintf(" [tool_calls: %s]", strings.Join(names, ","))
		}
		sb.WriteString(fmt.Sprintf("[%s] %s\n", m.Role, content))
	}

	req := []*schema.Message{
		schema.SystemMessage(summarySchemaPrompt),
		schema.UserMessage(sb.String()),
	}

	out, genErr := e.summaryModel.Generate(ctx, req)
	if genErr != nil {
		return "", 0, false, fmt.Errorf("summarize: %w", genErr)
	}
	if out == nil {
		return prevSummary, 0, true, nil
	}

	// 摘要成本 = 这次调用的 prompt + completion。provider 给了用量就用真实值，
	// 否则按本地估算兜底——宁可略微不准，也不能让成本项静默为 0。
	if out.ResponseMeta != nil && out.ResponseMeta.Usage != nil && out.ResponseMeta.Usage.TotalTokens > 0 {
		modelTokens = out.ResponseMeta.Usage.TotalTokens
	} else {
		modelTokens = EstimateMessagesTokens(req) + EstimateTextTokens(out.Content)
	}

	return out.Content, modelTokens, true, nil
}

// truncateSummary 执行摘要预算：
// 摘要长度 ≤ 被压缩内容的 MaxSummaryRatio，且 ≤ MaxSummaryTokens，取最严约束。
//
// 超预算时按「整行字段」截断，而不是按 rune 硬切。原因有两层：
//
//  1. 七字段摘要是按行输出的，硬切会把某个字段砍成半句（如「下一步: 应当先修」），
//     模型读到的是残缺指令；
//  2. 更要命的是增量更新——本次摘要会被写回 state.Memory.Summary，作为下一轮的
//     prevSummary 喂给摘要模型。残片进入下一轮，模型会在残句基础上继续改写，
//     错误逐轮累积，最终摘要可能完全偏离原始任务。
//
// 因此宁可整字段丢弃、并在末尾留一行显式标记，也不留半句。标记同时给出恢复路径，
// 让模型知道信息是被裁剪而非不存在。
func (e *Engine) truncateSummary(summary string, source []*schema.Message) string {
	if summary == "" {
		return summary
	}

	budget := e.MaxSummaryTokens
	if budget <= 0 {
		budget = defaultMaxSummaryToken
	}
	if e.MaxSummaryRatio > 0 {
		srcTokens := EstimateMessagesTokens(source)
		ratioBudget := int(float64(srcTokens) * e.MaxSummaryRatio)
		if ratioBudget > 0 && ratioBudget < budget {
			budget = ratioBudget
		}
	}

	if EstimateTextTokens(summary) <= budget {
		return summary
	}

	return truncateSummaryByField(summary, budget)
}

// summaryTruncatedNote 是摘要被裁剪时追加的标记行。
const summaryTruncatedNote = "[摘要超预算，已按字段截断；更早细节可凭 tool_call_id 调 read_result 回读]"

// truncateSummaryByField 按行（字段）边界把摘要裁剪到预算内。
//
// 从前往后保留完整的行，直到再加一行就会超预算；随后追加截断标记行，标记本身也计入预算。
// 单行就超预算时（极端情况），退化为按 rune 截断该行——此时已无字段结构可言，
// 保证不超预算优先。
func truncateSummaryByField(summary string, budget int) string {
	noteTokens := EstimateTextTokens(summaryTruncatedNote)

	var sb strings.Builder
	used := 0
	// 给标记行预留空间；预算过小连标记都放不下时，退化为纯截断
	limit := budget - noteTokens
	if limit <= 0 {
		limit = budget
		noteTokens = 0
	}

	lines := strings.Split(summary, "\n")
	kept := 0
	for _, line := range lines {
		lt := EstimateTextTokens(line)
		if used+lt > limit {
			// 一行都放不下时按 rune 截断该行，避免整体丢失
			if kept == 0 {
				sb.WriteString(truncateToTokens(line, limit))
				kept = 1
			}
			break
		}
		if kept > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString(line)
		used += lt
		kept++
	}

	if noteTokens > 0 && kept > 0 {
		if kept < len(lines) {
			sb.WriteString("\n")
			sb.WriteString(summaryTruncatedNote)
		}
	}
	return sb.String()
}

// truncateToTokens 把单行文本按 rune 截到不超过 budgetTokens。
// 使用内容自身的实测密度换算字符数，而不是假定固定的字符/token 比。
func truncateToTokens(s string, budgetTokens int) string {
	if budgetTokens <= 0 {
		return ""
	}
	density := TextDensity(s)
	if density <= 0 {
		return ""
	}
	maxChars := int(float64(budgetTokens) / density)
	runes := []rune(s)
	if len(runes) <= maxChars {
		return s
	}
	return string(runes[:maxChars])
}

// ----------------------------------------------------------------------------
// 阶段④：工具调用对修复
// ----------------------------------------------------------------------------

// fixToolCallPairs 修复被压缩切断的 tool_call / tool_result 配对。
// 保证压缩后的消息序列在协议层自洽，否则下次模型调用会直接报错。
//
// 这是不感知 SpillStore 的版本（包级函数，无引擎状态）。需要占位符带回读路径、
// 或需要统计补洞条数时，用 fixToolCallPairsCounted。
func fixToolCallPairs(messages []*schema.Message) []*schema.Message {
	out, _ := (&Engine{}).fixToolCallPairsCounted(nil, messages)
	return out
}

// fixToolCallPairsCounted 是 fixToolCallPairs 的引擎感知版本，额外返回补入的占位符条数。
//
// 与包级版本的两点差异，都是为了让「被动补洞」不再静默丢信息：
//
//  1. 占位符带回读路径。此前占位符内容是固定的一句「[该工具结果已被上下文压缩省略]」，
//     而 buildStub 产出的 stub 是带 tool_call_id 与 read_result 恢复方式的——同一个
//     「结果不在上下文里」的处境，两条路径给模型的能力不一致。凡是原文确实已 offload
//     到 SpillStore 的，这里补上同样的回读指引；没有的则明确告知不可回读、需重新调用工具。
//     没有这层区分，模型会对着一句「已省略」凭空编造结果——这是压缩引发幻觉的主要来源。
//
//  2. 统计补洞条数并计入不可恢复丢失。这类占位符不参与任何 token 节省的核算，
//     却代表模型确实少看到了一次工具结果，必须单独计数才能和任务失败率对上。
//
// 传入 nil ctx 时跳过 spill 查询（等价于未配置 spill 的行为）。
func (e *Engine) fixToolCallPairsCounted(
	ctx context.Context,
	messages []*schema.Message,
) (out []*schema.Message, placeholders int) {

	declared := make(map[string]struct{})
	for _, m := range messages {
		if m == nil || m.Role != schema.Assistant {
			continue
		}
		for _, tc := range m.ToolCalls {
			if tc.ID != "" {
				declared[tc.ID] = struct{}{}
			}
		}
	}

	answered := make(map[string]struct{})
	for _, m := range messages {
		if m == nil || m.Role != schema.Tool || m.ToolCallID == "" {
			continue
		}
		answered[m.ToolCallID] = struct{}{}
	}

	out = make([]*schema.Message, 0, len(messages))
	for _, m := range messages {
		if m == nil {
			continue
		}

		// 丢弃孤立的 tool 结果：找不到对应的 tool_call 声明
		if m.Role == schema.Tool {
			if _, ok := declared[m.ToolCallID]; !ok {
				continue
			}
			out = append(out, m)
			continue
		}

		out = append(out, m)

		// 为缺失的 tool 结果补占位，避免 assistant.tool_calls 悬空
		if m.Role == schema.Assistant {
			for _, tc := range m.ToolCalls {
				if tc.ID == "" {
					continue
				}
				if _, ok := answered[tc.ID]; ok {
					continue
				}
				out = append(out, &schema.Message{
					Role:       schema.Tool,
					ToolCallID: tc.ID,
					ToolName:   tc.Function.Name,
					Content:    e.placeholderContent(ctx, tc.ID, tc.Function.Name),
					// 标记为已处理，使后续压缩轮次跳过它、不反复改写
					Extra: map[string]any{extraKeyStub: true},
				})
				answered[tc.ID] = struct{}{}
				placeholders++
			}
		}
	}
	return out, placeholders
}

// placeholderContent 生成配对修复时补入的占位符文案。
//
// 分三种情况，核心是不给模型留「凭空编造」的空间：
//
//  1. 原文已 offload 到 SpillStore → 给出 read_result 回读路径，与 buildStub 的
//     stub 能力对齐。此前占位符只有一句「已被省略」，而 stub 却带回读方式，
//     同样「结果不在上下文」的处境给模型的能力不一致，模型对着占位符只能幻觉。
//  2. 未配置 SpillStore / 原文不在其中 → 明确告知不可回读、需要时重新调用工具。
//     「不可回读」这四个字必须写出来，否则模型无法区分「信息被裁剪」与「信息不存在」。
//  3. 无 tool_call_id → 无从寻址，只能降级为最简文案。
//
// 探测存在性用 Read 而非新增接口方法：SpillStore 只有 Spill/Read 两个方法，
// 为一次探测扩接口会让所有外部实现者被迫改动；这里读出即弃，代价是一次内存查询。
func (e *Engine) placeholderContent(ctx context.Context, toolCallID, toolName string) string {
	name := toolName
	if name == "" {
		name = "未知工具"
	}

	if toolCallID != "" && e != nil {
		if spill := e.spillStore(); spill != nil {
			if ctx == nil {
				ctx = context.Background()
			}
			// 读出即弃：只用错误与否判断原文是否可寻址，内容本身不需要
			if _, err := spill.Read(ctx, SpillRef(toolCallID)); err == nil {
				return fmt.Sprintf(
					"[工具 %s 的结果已移出上下文]\n调用: %s\n恢复方式: 调用 read_result 工具（传 tool_call_id=%s）回读完整结果",
					name, toolCallID, toolCallID,
				)
			}
		}
	}

	if toolCallID != "" {
		return fmt.Sprintf(
			"[工具 %s 的结果已被上下文压缩省略，且原文不可回读]\n调用: %s\n注意: 如需该数据请重新调用工具，不要基于本占位符推断结果",
			name, toolCallID,
		)
	}
	return fmt.Sprintf(
		"[工具 %s 的结果已被上下文压缩省略，且原文不可回读]\n注意: 如需该数据请重新调用工具，不要基于本占位符推断结果",
		name,
	)
}
