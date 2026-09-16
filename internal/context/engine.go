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

	// multimodalTokensPerPart 是单个非文本消息分片（图片/音频/视频/文件）的预留 token 数。
	// 这类分片的实际开销由 provider 侧按分辨率等计算，框架层用固定预留量兜底，
	// 避免含图消息被估成 0 token 而绕过压缩阈值。
	multimodalTokensPerPart = 1024
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
	return estimateTokens(messages) + e.Overhead().Total()
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
func (e *Engine) CompressInPlace(ctx context.Context, messages []*schema.Message) []*schema.Message {
	if e.effectiveTokens(messages) <= e.softLimit() {
		return messages
	}
	// 先采样再淘汰：采样不受 keepRecent 豁免，专治「一两条巨型结果撑爆窗口」——
	// 这正是 evictToolResults 因总数不足而直接放弃的那类场景。
	sampled := e.sampleOversizedResults(ctx, messages)
	// 采样已把体量压到低水位以下时，不再淘汰任何整条结果：
	// 每淘汰一条就改写一段历史、作废一次缓存前缀，能不付就不付。
	if e.effectiveTokens(sampled) <= e.lowWater() {
		return fixToolCallPairs(sampled)
	}
	evicted := e.evictToolResults(ctx, sampled, trimKeepRecentTools, e.lowWater())
	return fixToolCallPairs(evicted)
}

// Assemble 装配模型可见上下文，必要时触发压缩
func (e *Engine) Assemble(
	ctx context.Context,
	systemPrompt string,
	state *session.State,
	history []*schema.Message,
	userInput string,
) ([]*schema.Message, error) {

	messages := make([]*schema.Message, 0, len(history)+3)
	messages = append(messages, schema.SystemMessage(systemPrompt))
	if state != nil && state.MemorySummary != "" {
		messages = append(messages, schema.SystemMessage(state.MemorySummary))
	}
	messages = append(messages, history...)
	messages = append(messages, schema.UserMessage(userInput))

	if !e.shouldCompress(messages) {
		return messages, nil
	}

	return e.compress(ctx, messages, state)
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
) ([]*schema.Message, error) {

	// 阶段①：工具结果减负（规则，不调 LLM，可 offload 回读）
	// 先采样巨型单条结果（不受 keepRecent 豁免），再按 oldest-first 淘汰整条。
	sampled := e.sampleOversizedResults(ctx, messages)
	trimmed := e.evictToolResults(ctx, sampled, trimKeepRecentTools, e.lowWater())

	// 阶段②：边界确定，保护头尾
	head, middle, tail := splitByBoundary(trimmed, e.tailBudget())
	if len(middle) == 0 {
		return fixToolCallPairs(trimmed), nil
	}

	// 阶段③：结构化摘要（仅调用一次 LLM，增量更新）
	var prevSummary string
	if state != nil {
		prevSummary = state.MemorySummary
	}
	summary, err := e.summarize(ctx, middle, prevSummary)
	if err != nil {
		return nil, err
	}
	summary = e.truncateSummary(summary, middle)

	if state != nil {
		state.MemorySummary = summary
	}

	// 阶段④：工具调用对修复
	merged := make([]*schema.Message, 0, len(head)+1+len(tail))
	merged = append(merged, head...)
	merged = append(merged, schema.SystemMessage(summary))
	merged = append(merged, tail...)

	return fixToolCallPairs(merged), nil
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

	toolIdxs := make([]int, 0)
	for i, m := range messages {
		if m != nil && m.Role == schema.Tool {
			toolIdxs = append(toolIdxs, i)
		}
	}
	if len(toolIdxs) <= keepRecent {
		return messages
	}

	spill := e.spillStore()

	out := make([]*schema.Message, len(messages))
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
			continue
		}
		out[idx] = stub

		// clear_at_least：达到低水位就停，不追求清得更多
		if e.effectiveTokens(out) <= lowWater {
			break
		}
	}
	return out
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
	threshold := int(float64(e.softLimit()) * oversizedResultRatio)
	if threshold <= 0 {
		return messages
	}
	spill := e.spillStore()

	// 采样体积随阈值自适应：头占阈值的一半、尾占四分之一（按 token 计），
	// 各自不超过 sampleHeadChars / sampleTailChars 的绝对上限。
	//
	// 为什么必须自适应而不是固定头尾长度：固定 1024+512 字符在大窗口下很合适，
	// 但换算成 token 约 768，会高于小窗口场景的阈值本身——采样完仍超阈，等于白做。
	// 按阈值比例缩放后，采样结果必定落在阈值以下，两种窗口尺寸都成立。
	headChars, tailChars := sampleWindow(threshold)
	minOriginal := headChars + tailChars + sampleMetaSlack

	out := make([]*schema.Message, len(messages))
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
		if estimateMessageTokens(m) <= threshold {
			continue
		}
		runes := []rune(m.Content)
		// 原文尚未超过「头+尾+metadata」的体积时，采样没有意义（可能反而更大），跳过
		if len(runes) <= minOriginal {
			continue
		}
		// 未配置 spill 时，采样会永久丢失中段：非幂等工具结果不可重放，跳过不采
		if spill == nil && e.nonIdempotent(m.ToolName) {
			continue
		}

		sampled := e.buildSampled(ctx, m, spill, headChars, tailChars)
		if sampled == nil {
			continue
		}
		out[i] = sampled
		changed = true
	}
	if !changed {
		return messages
	}
	return out
}

// sampleWindow 按超尺寸阈值换算头尾各自的保留字符数。
// 估算口径与 estimateTextTokens 一致（约 2 字符 = 1 token）。
func sampleWindow(thresholdTokens int) (headChars, tailChars int) {
	headChars = thresholdTokens // 阈值一半的 token ≈ 阈值大小的字符数
	if headChars > sampleHeadChars {
		headChars = sampleHeadChars
	}
	tailChars = thresholdTokens / 2 // 阈值四分之一的 token
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
		if shareCap := int(float64(estimateTokens(rest)) * maxTailShareOfMessages); shareCap > 0 && shareCap < tailBudget {
			tailBudget = shareCap
		}
	}
	tailStart := len(rest)
	used := 0
	for i := len(rest) - 1; i >= 0; i-- {
		t := estimateMessageTokens(rest[i])
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
func (e *Engine) summarize(
	ctx context.Context,
	middle []*schema.Message,
	prevSummary string,
) (string, error) {

	if len(middle) == 0 {
		return prevSummary, nil
	}

	// 无摘要模型时降级为规则摘要，保证链路可用
	if e.summaryModel == nil {
		return fmt.Sprintf(
			"[历史压缩] 已省略 %d 条较早消息，最早一条为 %s 角色。",
			len(middle), middle[0].Role,
		), nil
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

	out, err := e.summaryModel.Generate(ctx, req)
	if err != nil {
		return "", fmt.Errorf("summarize: %w", err)
	}
	if out == nil {
		return prevSummary, nil
	}
	return out.Content, nil
}

// truncateSummary 执行摘要预算：
// 摘要长度 ≤ 被压缩内容的 MaxSummaryRatio，且 ≤ MaxSummaryTokens，取最严约束。
func (e *Engine) truncateSummary(summary string, source []*schema.Message) string {
	if summary == "" {
		return summary
	}

	budget := e.MaxSummaryTokens
	if budget <= 0 {
		budget = defaultMaxSummaryToken
	}
	if e.MaxSummaryRatio > 0 {
		srcTokens := estimateTokens(source)
		ratioBudget := int(float64(srcTokens) * e.MaxSummaryRatio)
		if ratioBudget > 0 && ratioBudget < budget {
			budget = ratioBudget
		}
	}

	if estimateTextTokens(summary) <= budget {
		return summary
	}

	// 按 rune 保守截断（粗略按 1 token ≈ 2 字符）
	maxChars := budget * 2
	runes := []rune(summary)
	if len(runes) > maxChars {
		runes = runes[:maxChars]
	}
	return string(runes)
}

// ----------------------------------------------------------------------------
// 阶段④：工具调用对修复
// ----------------------------------------------------------------------------

// fixToolCallPairs 修复被压缩切断的 tool_call / tool_result 配对。
// 保证压缩后的消息序列在协议层自洽，否则下次模型调用会直接报错。
func fixToolCallPairs(messages []*schema.Message) []*schema.Message {
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

	out := make([]*schema.Message, 0, len(messages))
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
					Content:    "[该工具结果已被上下文压缩省略]",
				})
				answered[tc.ID] = struct{}{}
			}
		}
	}
	return out
}

// ----------------------------------------------------------------------------
// Token 估算（粗略，生产环境建议替换为 tiktoken）
// ----------------------------------------------------------------------------

func estimateTokens(messages []*schema.Message) int {
	total := 0
	for _, m := range messages {
		total += estimateMessageTokens(m)
	}
	return total
}

func estimateMessageTokens(m *schema.Message) int {
	if m == nil {
		return 0
	}
	n := estimateTextTokens(m.Content)
	n += estimateTextTokens(m.ReasoningContent)
	for _, tc := range m.ToolCalls {
		n += estimateTextTokens(tc.Function.Name)
		n += estimateTextTokens(tc.Function.Arguments)
	}
	n += estimateMultiContentTokens(m)
	return n + 4 // 消息结构固定开销
}

// estimateMultiContentTokens 估算多模态分片的开销。
// 非文本分片（图片/音频/视频/文件）按固定预留量计，文本分片按字符估算。
// 漏算会让含图消息被估成近乎 0 token，从而永远不触发压缩。
func estimateMultiContentTokens(m *schema.Message) int {
	n := 0
	// MultiContent 已废弃但仍被部分 provider 使用，一并计入
	for _, part := range m.MultiContent {
		if part.Type == schema.ChatMessagePartTypeText {
			n += estimateTextTokens(part.Text)
			continue
		}
		n += multimodalTokensPerPart
	}
	for _, part := range m.UserInputMultiContent {
		if part.Type == schema.ChatMessagePartTypeText {
			n += estimateTextTokens(part.Text)
			continue
		}
		n += multimodalTokensPerPart
	}
	return n
}

// estimateTextTokens 中英混排的粗略估算：约 2 字符 = 1 token
func estimateTextTokens(s string) int {
	if s == "" {
		return 0
	}
	return len([]rune(s))/2 + 1
}
