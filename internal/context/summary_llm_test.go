package context

import (
	"context"
	"fmt"
	"strings"
	"testing"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/daqiaoliang-coder/agentrix/internal/session"
)

// 本文件覆盖此前的最大盲区：LLM 结构化摘要路径（四阶段压缩的阶段③）。
//
// 为什么它必须单独测：window_and_oversize_test.go 里的摘要测试用的 summaryModel=nil，
// 走的是规则降级（只产出「已省略 N 条消息」一句）。也就是说七字段结构化摘要
// ——整个压缩设计里唯一做语义压缩的环节——从未被任何测试执行过。
// 摘要质量问题（字段缺失、增量漂移、截断砍断结构）只会在这条路径上暴露。

// ── 摘要模型替身 ──

// fakeSummaryModel 按脚本返回摘要，并记录每次被喂入的 prompt，用于验证增量更新。
type fakeSummaryModel struct {
	st *fakeSummaryState
}

type fakeSummaryState struct {
	replies []*schema.Message
	prompts []string // 每次 Generate 收到的 user 消息内容
	call    int
	usage   *schema.TokenUsage // 非 nil 时挂到 ResponseMeta，模拟 provider 返回真实用量
	err     error              // 非 nil 时 Generate 直接失败
}

func newFakeSummaryModel(replies ...*schema.Message) *fakeSummaryModel {
	return &fakeSummaryModel{st: &fakeSummaryState{replies: replies}}
}

func (m *fakeSummaryModel) Generate(_ context.Context, in []*schema.Message, _ ...einomodel.Option) (*schema.Message, error) {
	if m.st.err != nil {
		return nil, m.st.err
	}
	// 记录喂入的正文（user 消息），用于断言增量更新是否带上了 prevSummary
	for _, msg := range in {
		if msg != nil && msg.Role == schema.User {
			m.st.prompts = append(m.st.prompts, msg.Content)
		}
	}
	if m.st.call >= len(m.st.replies) {
		return schema.AssistantMessage("目标: 兜底", nil), nil
	}
	out := m.st.replies[m.st.call]
	m.st.call++
	if m.st.usage != nil {
		if out.ResponseMeta == nil {
			out.ResponseMeta = &schema.ResponseMeta{}
		}
		out.ResponseMeta.Usage = m.st.usage
	}
	return out, nil
}

func (m *fakeSummaryModel) Stream(_ context.Context, _ []*schema.Message, _ ...einomodel.Option) (*schema.StreamReader[*schema.Message], error) {
	return nil, context.DeadlineExceeded
}

// summaryFields 是摘要必须包含的七个字段名，与 summarySchemaPrompt 严格对应。
var summaryFields = []string{"目标:", "约束:", "进度:", "关键决策:", "相关文件:", "下一步:", "关键上下文:"}

func wellFormedSummary() string {
	return strings.Join([]string{
		"目标: 修复订单服务的超时问题",
		"约束: 不得改动数据库 schema",
		"进度: 已定位到连接池配置",
		"关键决策: 采用扩容而非重试",
		"相关文件: order/pool.go",
		"下一步: 调整 maxConns 并压测",
		"关键上下文: 故障发生在灰度环境",
	}, "\n")
}

// ── 阶段③：LLM 结构化摘要路径 ──

// TestSummarizeViaLLMProducesStructuredFields 验证摘要路径真的走了 LLM，
// 且账本如实记录 viaLLM=true——这是区分「语义压缩」与「规则降级」的关键标记。
func TestSummarizeViaLLMProducesStructuredFields(t *testing.T) {
	fake := newFakeSummaryModel(schema.AssistantMessage(wellFormedSummary(), nil))
	e := NewEngineWithModel(fake)
	e.TokenBudget = 16384
	e.SetSpillStore(NewMemorySpillStore())

	state := &session.State{}
	history := buildToolHistory(30, 1800)
	if e.effectiveTokens(history) <= e.softLimit() {
		t.Fatalf("测试前提不成立：历史 %d token 未越 softLimit %d",
			e.effectiveTokens(history), e.softLimit())
	}

	out, stats, err := e.AssembleWithStats(context.Background(), "sys", state, history, "继续")
	if err != nil {
		t.Fatalf("AssembleWithStats: %v", err)
	}

	if !stats.SummaryViaLLM {
		t.Error("SummaryViaLLM=false：未走 LLM 结构化摘要路径，仍是规则降级")
	}
	if fake.st.call != 1 {
		t.Errorf("摘要模型应被调用 1 次（设计约定 LLM 仅调用一次），实际 %d 次", fake.st.call)
	}
	if len(fake.st.prompts) != 1 {
		t.Fatalf("未捕获到摘要 prompt，无法验证喂入内容")
	}

	// 七字段必须完整落到 state，否则模型下一轮读到的是残缺上下文
	for _, f := range summaryFields {
		if !strings.Contains(state.Memory.Summary, f) {
			t.Errorf("摘要缺少字段 %q，实际:\n%s", f, state.Memory.Summary)
		}
	}
	// 摘要必须作为 system 消息出现在产物里，否则写了 state 也白写
	found := false
	for _, m := range out {
		if m.Role == schema.System && strings.Contains(m.Content, "目标:") {
			found = true
		}
	}
	if !found {
		t.Error("压缩产物中找不到摘要 system 消息，跨轮记忆未真正注入上下文")
	}
}

// TestSummaryIncrementalUpdateFeedsPrevSummary 验证增量更新：第二次摘要的 prompt
// 必须带上第一次的摘要。漏带会让每次摘要都从头重建，语义漂移不可控。
func TestSummaryIncrementalUpdateFeedsPrevSummary(t *testing.T) {
	first := "目标: 第一次摘要的目标\n约束: 第一次的约束"
	second := "目标: 第二次摘要的目标\n约束: 第二次的约束"
	fake := newFakeSummaryModel(
		schema.AssistantMessage(first, nil),
		schema.AssistantMessage(second, nil),
	)
	e := NewEngineWithModel(fake)
	e.TokenBudget = 16384
	e.SetSpillStore(NewMemorySpillStore())

	state := &session.State{}
	history := buildToolHistory(30, 1800)

	if _, _, err := e.AssembleWithStats(context.Background(), "sys", state, history, "第一轮"); err != nil {
		t.Fatalf("第一轮: %v", err)
	}
	if state.Memory.Summary != first {
		t.Fatalf("第一轮摘要未写入 state: %q", state.Memory.Summary)
	}

	// 第二轮复用同一 state，prevSummary 应被喂给摘要模型
	if _, _, err := e.AssembleWithStats(context.Background(), "sys", state, buildToolHistory(30, 1800), "第二轮"); err != nil {
		t.Fatalf("第二轮: %v", err)
	}
	if len(fake.st.prompts) < 2 {
		t.Fatalf("第二轮未调用摘要模型，prompts=%d", len(fake.st.prompts))
	}
	if !strings.Contains(fake.st.prompts[1], first) {
		t.Errorf("增量更新失效：第二轮 prompt 未带上第一轮摘要\n第二轮 prompt:\n%s", fake.st.prompts[1])
	}
	if !strings.Contains(fake.st.prompts[1], "增量更新") {
		t.Error("第二轮 prompt 缺少增量更新指令，模型可能从头重写")
	}
}

// TestSummaryModelTokensRecordedAsCost 验证摘要 LLM 的自身开销被记为成本。
//
// 这是净收益核算的关键：provider 返回用量时取真实值，未返回时按估算兜底。
// 若这一项恒为 0，NetTokensSaved 会系统性高估压缩收益——「摘要越做越贵」
// 这种退化将完全不可见。
func TestSummaryModelTokensRecordedAsCost(t *testing.T) {
	t.Run("provider返回真实用量时取真实值", func(t *testing.T) {
		fake := newFakeSummaryModel(schema.AssistantMessage(wellFormedSummary(), nil))
		fake.st.usage = &schema.TokenUsage{
			PromptTokens: 5000, CompletionTokens: 300, TotalTokens: 5300,
		}
		e := NewEngineWithModel(fake)
		e.TokenBudget = 16384
		e.SetSpillStore(NewMemorySpillStore())

		_, stats, err := e.AssembleWithStats(context.Background(), "sys", &session.State{},
			buildToolHistory(30, 1800), "继续")
		if err != nil {
			t.Fatalf("AssembleWithStats: %v", err)
		}
		if stats.SummaryModelTokens != 5300 {
			t.Errorf("摘要成本应取 provider 真实用量 5300，实际 %d", stats.SummaryModelTokens)
		}
	})

	t.Run("provider未返回用量时按估算兜底而非记0", func(t *testing.T) {
		fake := newFakeSummaryModel(schema.AssistantMessage(wellFormedSummary(), nil))
		// 不设 usage：模拟 provider 不回传用量明细
		e := NewEngineWithModel(fake)
		e.TokenBudget = 16384
		e.SetSpillStore(NewMemorySpillStore())

		_, stats, err := e.AssembleWithStats(context.Background(), "sys", &session.State{},
			buildToolHistory(30, 1800), "继续")
		if err != nil {
			t.Fatalf("AssembleWithStats: %v", err)
		}
		if stats.SummaryModelTokens <= 0 {
			t.Errorf("摘要成本为 %d：provider 未返回用量时必须按估算兜底，记 0 会虚增净收益",
				stats.SummaryModelTokens)
		}
	})

	t.Run("净收益必须扣除摘要成本", func(t *testing.T) {
		fake := newFakeSummaryModel(schema.AssistantMessage(wellFormedSummary(), nil))
		fake.st.usage = &schema.TokenUsage{PromptTokens: 5000, CompletionTokens: 300, TotalTokens: 5300}
		e := NewEngineWithModel(fake)
		e.TokenBudget = 16384
		e.SetSpillStore(NewMemorySpillStore())

		_, stats, err := e.AssembleWithStats(context.Background(), "sys", &session.State{},
			buildToolHistory(30, 1800), "继续")
		if err != nil {
			t.Fatalf("AssembleWithStats: %v", err)
		}
		if want := stats.TokensSaved() - stats.SummaryModelTokens; stats.NetTokensSaved() != max(0, want) {
			t.Errorf("净节省 %d != 毛节省 %d − 摘要成本 %d",
				stats.NetTokensSaved(), stats.TokensSaved(), stats.SummaryModelTokens)
		}
		if stats.SummaryModelTokens > stats.TokensSaved() {
			t.Logf("注意：本次摘要成本 %d 超过毛节省 %d，净收益为 0——这正是该指标要暴露的情况",
				stats.SummaryModelTokens, stats.TokensSaved())
		}
	})
}

// TestSummarizeFailureDoesNotRecordStats 验证摘要失败时不记账：
// 失败轮次没有产出任何压缩结果，计入统计会污染节省量与事件数。
func TestSummarizeFailureDoesNotRecordStats(t *testing.T) {
	fake := newFakeSummaryModel()
	fake.st.err = fmt.Errorf("summary model unavailable")
	e := NewEngineWithModel(fake)
	e.TokenBudget = 16384
	e.SetSpillStore(NewMemorySpillStore())

	var notified int
	e.OnCompacted = func(CompressStats) { notified++ }

	state := &session.State{}
	state.Memory.Summary = "原有摘要"
	_, stats, err := e.AssembleWithStats(context.Background(), "sys", state, buildToolHistory(30, 1800), "继续")
	if err == nil {
		t.Fatal("摘要模型失败时应返回错误，而不是静默降级")
	}
	if notified != 0 {
		t.Errorf("摘要失败仍发出了 %d 次压缩事件，会污染统计", notified)
	}
	if state.Memory.Summary != "原有摘要" {
		t.Errorf("摘要失败时不应改写 state，实际: %q", state.Memory.Summary)
	}
	if stats.Triggered {
		t.Error("失败路径不应标记 Triggered")
	}
}

// ── 摘要截断：按字段边界，不砍断结构 ──

// TestTruncateSummaryKeepsFieldIntegrity 验证超预算时按整行字段截断。
//
// 为什么不能按 rune 硬切：七字段按行输出，硬切会把字段砍成半句（如「下一步: 应当先修」）。
// 更要命的是摘要会写回 state 作为下一轮的 prevSummary，残句进入下一轮后被继续改写，
// 错误逐轮累积，最终摘要可能完全偏离原始任务。
func TestTruncateSummaryKeepsFieldIntegrity(t *testing.T) {
	e := NewEngine()
	// 收紧预算，强制触发截断
	e.MaxSummaryTokens = 60
	e.MaxSummaryRatio = 0

	summary := wellFormedSummary()
	if EstimateTextTokens(summary) <= e.MaxSummaryTokens {
		t.Fatalf("测试前提不成立：摘要 %d token 未超预算 %d",
			EstimateTextTokens(summary), e.MaxSummaryTokens)
	}

	got := e.truncateSummary(summary, nil)

	// 每一行都必须是完整的字段行，不允许出现半句
	for _, line := range strings.Split(got, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if strings.HasPrefix(line, "[摘要超预算") {
			continue // 截断标记行
		}
		// 完整字段行必须含 ": "，且冒号后有非空内容
		idx := strings.Index(line, ": ")
		if idx <= 0 {
			t.Errorf("出现非字段结构的残片行: %q", line)
			continue
		}
		field := line[:idx+1]
		value := line[idx+2:]
		if strings.TrimSpace(value) == "" {
			t.Errorf("字段 %q 被砍空", field)
		}
		// 该字段在原文里必须完整存在，证明是整行保留而非截断拼接
		if !strings.Contains(summary, line) {
			t.Errorf("保留的行不是原文的完整行: %q", line)
		}
	}

	// 截断后不得超预算
	if EstimateTextTokens(got) > e.MaxSummaryTokens {
		t.Errorf("截断后仍超预算：%d > %d", EstimateTextTokens(got), e.MaxSummaryTokens)
	}
	// 必须有显式标记，让模型知道信息是被裁剪而非不存在
	if !strings.Contains(got, "摘要超预算") {
		t.Errorf("缺少截断标记，模型无法区分「被裁剪」与「不存在」:\n%s", got)
	}
	// 至少保留了第一个字段（目标），否则等于摘要全丢
	if !strings.Contains(got, "目标:") {
		t.Errorf("连「目标」字段都未保留，摘要失去意义:\n%s", got)
	}
}

// TestTruncateSummaryNoOpWithinBudget 验证未超预算时原样返回，不做任何改写。
// 每次压缩都动摘要会破坏缓存前缀，也会让增量摘要无谓漂移。
func TestTruncateSummaryNoOpWithinBudget(t *testing.T) {
	e := NewEngine()
	e.MaxSummaryTokens = 5000
	e.MaxSummaryRatio = 0

	summary := wellFormedSummary()
	if got := e.truncateSummary(summary, nil); got != summary {
		t.Errorf("未超预算却改写了摘要:\n原=%q\n改=%q", summary, got)
	}
}

// TestTruncateSummaryRatioBoundBySource 验证 MaxSummaryRatio 生效：
// 摘要不得超过被压缩内容的一定比例，取「比例」与「绝对值」两者更严的一个。
func TestTruncateSummaryRatioBoundBySource(t *testing.T) {
	e := NewEngine()
	e.MaxSummaryTokens = 100000 // 绝对上限放到极大，确保由比例约束生效
	e.MaxSummaryRatio = 0.2

	// 构造一个很小的 source，使比例预算远小于摘要长度
	source := []*schema.Message{schema.ToolMessage(strings.Repeat("q", 200), "call_src")}
	ratioBudget := int(float64(EstimateMessagesTokens(source)) * e.MaxSummaryRatio)

	summary := wellFormedSummary()
	if EstimateTextTokens(summary) <= ratioBudget {
		t.Fatalf("测试前提不成立：摘要 %d 未超比例预算 %d", EstimateTextTokens(summary), ratioBudget)
	}

	got := e.truncateSummary(summary, source)
	if EstimateTextTokens(got) > ratioBudget {
		t.Errorf("摘要超出比例预算：%d > %d（source=%d token, ratio=%v）",
			EstimateTextTokens(got), ratioBudget, EstimateMessagesTokens(source), e.MaxSummaryRatio)
	}
}

// ── 累积退化：连续多轮压缩 ──

// TestRepeatedCompressionDoesNotDegradeMonotonically 验证累积压缩不单调退化。
//
// summarize 是增量的（吃 prevSummary），单次无损 ≠ 十次无损：语义漂移只在这条
// 路径上暴露。这里连续压缩多轮，断言每轮都仍产出七字段结构、且 token 不失控增长。
func TestRepeatedCompressionDoesNotDegradeMonotonically(t *testing.T) {
	const rounds = 10

	fake := newFakeSummaryModel()
	// 脚本耗尽后走兜底分支，这里让每轮都返回结构完整的摘要，模拟模型稳定发挥
	replies := make([]*schema.Message, 0, rounds)
	for i := 0; i < rounds; i++ {
		replies = append(replies, schema.AssistantMessage(
			fmt.Sprintf("目标: 第 %d 轮的任务目标\n约束: 不得改动 schema\n进度: 已完成 %d 步\n"+
				"关键决策: 第 %d 次决策\n相关文件: svc/handler.go\n下一步: 继续第 %d 步\n"+
				"关键上下文: 环境为灰度", i+1, i, i, i+2), nil))
	}
	fake.st.replies = replies

	e := NewEngineWithModel(fake)
	e.TokenBudget = 16384
	e.SetSpillStore(NewMemorySpillStore())

	state := &session.State{}
	prevSummaryTokens := 0

	for i := 0; i < rounds; i++ {
		// 每轮喂入新的历史，模拟会话推进
		history := buildToolHistory(30, 1800)
		_, stats, err := e.AssembleWithStats(context.Background(), "sys", state, history,
			fmt.Sprintf("第 %d 轮指令", i+1))
		if err != nil {
			t.Fatalf("第 %d 轮: %v", i+1, err)
		}
		if !stats.SummaryViaLLM {
			t.Fatalf("第 %d 轮未走 LLM 摘要路径", i+1)
		}

		summary := state.Memory.Summary
		if strings.TrimSpace(summary) == "" {
			t.Fatalf("第 %d 轮摘要为空，跨轮记忆断裂", i+1)
		}
		// 每轮都必须保留完整七字段结构
		for _, f := range summaryFields {
			if !strings.Contains(summary, f) {
				t.Fatalf("第 %d 轮摘要丢失字段 %q:\n%s", i+1, f, summary)
			}
		}

		cur := EstimateTextTokens(summary)
		if prevSummaryTokens > 0 && cur > prevSummaryTokens*2 {
			t.Errorf("第 %d 轮摘要 token 从 %d 暴涨到 %d，增量更新可能在累积堆叠",
				i+1, prevSummaryTokens, cur)
		}
		prevSummaryTokens = cur
	}

	if fake.st.call != rounds {
		t.Errorf("应调用摘要模型 %d 次，实际 %d 次", rounds, fake.st.call)
	}
	// 每轮 prompt 都应带上上一轮摘要（增量而非重建）
	for i := 1; i < len(fake.st.prompts); i++ {
		if !strings.Contains(fake.st.prompts[i], "已有摘要") {
			t.Errorf("第 %d 轮 prompt 未走增量更新分支", i+1)
		}
	}
}

// ── 账本与前缀观测 ──

// TestCompressStatsAccounting 验证账本的算术自洽与语义正确。
func TestCompressStatsAccounting(t *testing.T) {
	e := NewEngine()
	e.TokenBudget = 10000
	spill := NewMemorySpillStore()
	e.SetSpillStore(spill)

	msgs := buildToolHistory(20, 1600)
	before := e.effectiveTokens(msgs)

	out, stats := e.CompressInPlaceWithStats(context.Background(), msgs)

	if !stats.Triggered {
		t.Fatal("越过 softLimit 却未触发压缩")
	}
	if stats.Path != "inplace" {
		t.Errorf("Path=%q，应为 inplace", stats.Path)
	}
	if stats.TokensBefore != before {
		t.Errorf("TokensBefore=%d，应为 %d", stats.TokensBefore, before)
	}
	if stats.TokensAfter != e.effectiveTokens(out) {
		t.Errorf("TokensAfter=%d，与产物实际 %d 不一致", stats.TokensAfter, e.effectiveTokens(out))
	}
	if stats.TokensAfter >= stats.TokensBefore {
		t.Errorf("压缩后未减负：%d → %d", stats.TokensBefore, stats.TokensAfter)
	}
	if stats.TokensSaved() != stats.TokensBefore-stats.TokensAfter {
		t.Errorf("TokensSaved=%d，应为 %d", stats.TokensSaved(), stats.TokensBefore-stats.TokensAfter)
	}
	if ratio := stats.CompressionRatio(); ratio <= 0 || ratio >= 1 {
		t.Errorf("压缩率 %v 超出合理范围 (0,1)", ratio)
	}
	// 循环内压缩不调 LLM，摘要相关字段必须为 0
	if stats.SummaryModelTokens != 0 || stats.SummaryViaLLM {
		t.Errorf("inplace 路径不应产生摘要开销：modelTokens=%d viaLLM=%v",
			stats.SummaryModelTokens, stats.SummaryViaLLM)
	}
	if stats.NetTokensSaved() != stats.TokensSaved() {
		t.Errorf("无摘要开销时净节省 %d 应等于毛节省 %d", stats.NetTokensSaved(), stats.TokensSaved())
	}
	// 配了 spill，淘汰应当全部可恢复
	if stats.Evicted > 0 && stats.Offloaded != stats.Evicted {
		t.Errorf("配置 SpillStore 时淘汰 %d 条但仅 offload %d 条", stats.Evicted, stats.Offloaded)
	}
	// 稳定前缀不可能超过压缩前的总量
	if stats.StablePrefixTokens > stats.TokensBefore {
		t.Errorf("稳定前缀 %d 超过压缩前总量 %d", stats.StablePrefixTokens, stats.TokensBefore)
	}
}

// TestUnrecoverableLostCountedWithoutSpill 验证未配置 SpillStore 时，
// 不可恢复丢失被如实计数。这是代价侧唯一无法用 token 衡量的量，
// 漏计会让人以为「零成本压缩」，而实际模型已经少看到了数据。
func TestUnrecoverableLostCountedWithoutSpill(t *testing.T) {
	e := NewEngine()
	e.TokenBudget = 10000
	// 故意不配 SpillStore

	msgs := buildToolHistory(20, 1600)
	_, stats := e.CompressInPlaceWithStats(context.Background(), msgs)

	if !stats.Triggered {
		t.Fatal("未触发压缩，无法验证计数")
	}
	if stats.Evicted == 0 {
		t.Fatal("未淘汰任何结果，无法验证不可恢复计数")
	}
	if stats.Offloaded != 0 {
		t.Errorf("未配置 SpillStore 却报告 offload %d 条", stats.Offloaded)
	}
	if stats.UnrecoverableLost < stats.Evicted {
		t.Errorf("不可恢复丢失 %d 少于淘汰数 %d：未配 spill 时淘汰即永久丢失",
			stats.UnrecoverableLost, stats.Evicted)
	}
}

// TestNoCompactionSignalWhenUnderThreshold 验证未越阈值时不发事件。
// 这类轮次占绝大多数，全发出去会淹掉真正需要关注的压缩事件。
func TestNoCompactionSignalWhenUnderThreshold(t *testing.T) {
	e := NewEngine()
	e.TokenBudget = 100000 // softLimit 远超测试消息体量
	e.SetSpillStore(NewMemorySpillStore())

	var got []CompressStats
	e.OnCompacted = func(s CompressStats) { got = append(got, s) }

	msgs := buildToolHistory(10, 200)
	out := e.CompressInPlace(context.Background(), msgs)

	if len(got) != 0 {
		t.Errorf("未越阈值却发出 %d 次压缩事件", len(got))
	}
	if len(out) != len(msgs) {
		t.Errorf("未越阈值却改动了序列长度：%d → %d", len(msgs), len(out))
	}
}

// TestCompactionHookFiresWhenTriggered 验证钩子在真正压缩时被调用，
// 且账本内容完整可序列化（外层要把它桥接成 projection.ContextCompacted 信号）。
func TestCompactionHookFiresWhenTriggered(t *testing.T) {
	e := NewEngine()
	e.TokenBudget = 10000
	e.SetSpillStore(NewMemorySpillStore())

	var got []CompressStats
	e.OnCompacted = func(s CompressStats) { got = append(got, s) }

	msgs := buildToolHistory(20, 1600)
	e.CompressInPlace(context.Background(), msgs)

	if len(got) != 1 {
		t.Fatalf("应发出 1 次压缩事件，实际 %d 次", len(got))
	}
	s := got[0]
	if !s.Triggered || s.TokensSaved() <= 0 {
		t.Errorf("账本内容异常：triggered=%v saved=%d", s.Triggered, s.TokensSaved())
	}
}

// TestStablePrefixShrinksWhenHeadRewritten 验证前缀统计能反映改写位置。
// 头部（system + 首次交互）被保护时前缀应较长；这是缓存复用的直接代理指标。
func TestStablePrefixShrinksWhenHeadRewritten(t *testing.T) {
	head := []*schema.Message{schema.SystemMessage("长系统提示"), schema.UserMessage("首个用户请求")}
	changed := []*schema.Message{schema.SystemMessage("完全不同的系统提示"), schema.UserMessage("首个用户请求")}

	full := stablePrefixTokens(head, head)
	if full <= 0 {
		t.Fatalf("完全相同的序列前缀应大于 0，实际 %d", full)
	}
	if got := stablePrefixTokens(head, changed); got >= full {
		t.Errorf("首条被改写后前缀应收缩：%d >= %d", got, full)
	}
	if got := stablePrefixTokens(head, nil); got != 0 {
		t.Errorf("空序列前缀应为 0，实际 %d", got)
	}
}
