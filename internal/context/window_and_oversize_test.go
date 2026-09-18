package context

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/daqiaoliang-coder/agentrix/internal/session"
)

// 本文件覆盖两项修复：
//   §1 尾部保护区收敛——保证小窗口下四阶段压缩的「结构化摘要」阶段不被静默跳过；
//   §3 超尺寸单条结果头尾采样——突破 evictToolResults 的 keepRecent 免死金牌。
// 复用 compress_test.go 中的 buildToolHistory / assistantCall / toolMsg / isStub 辅助函数。

// ---------------------------------------------------------------------------
// §1：尾部保护区收敛
// ---------------------------------------------------------------------------

// TestTailBudgetConvergesUnderWindow 验证 tailBudget() 把保护区收敛到窗口的
// maxTailRatio 以内，即使 TailTokenBudget 仍是按大窗口设定的默认值 20000。
func TestTailBudgetConvergesUnderWindow(t *testing.T) {
	e := NewEngine()
	e.TokenBudget = 16384 // 接入方常配的小窗口
	if e.TailTokenBudget != defaultTailTokenBudget {
		t.Fatalf("测试前提：TailTokenBudget 应为默认值 %d，实际 %d", defaultTailTokenBudget, e.TailTokenBudget)
	}
	got := e.tailBudget()
	if got >= e.TokenBudget {
		t.Errorf("尾部保护区 %d 未收敛，仍 >= 窗口 %d，middle 会恒空", got, e.TokenBudget)
	}
	if want := int(float64(e.TokenBudget) * maxTailRatio); got != want {
		t.Errorf("尾部保护区应收敛到 %d（窗口×maxTailRatio），实际 %d", want, got)
	}
}

// TestSplitByBoundaryKeepsNonEmptyMiddle 验证 splitByBoundary 的第二道收敛：
// 无论传入多大的期望尾部预算，只要消息足够多，中间段都非空——这是摘要阶段有活可干的前提。
func TestSplitByBoundaryKeepsNonEmptyMiddle(t *testing.T) {
	msgs := buildToolHistory(12, 300)
	// 故意传入一个远大于消息总量的尾部预算，复现「middle 被吞空」的触发条件
	head, middle, tail := splitByBoundary(msgs, 1<<20)
	if len(middle) == 0 {
		t.Fatalf("middle 为空，摘要阶段会被跳过；head=%d tail=%d", len(head), len(tail))
	}
	if len(head)+len(middle)+len(tail) != len(msgs) {
		t.Errorf("三段未覆盖全部消息：%d+%d+%d != %d", len(head), len(middle), len(tail), len(msgs))
	}
}

// TestSummaryPathExecutesWithSmallWindow 验证 §1 的端到端效果：配小窗口时，
// Assemble 触发压缩后会写入 state.Memory.Summary。summaryModel 为 nil，走规则摘要降级，
// 但只有 middle 非空才会调用 summarize 并写入——以此证明摘要路径确实执行了。
func TestSummaryPathExecutesWithSmallWindow(t *testing.T) {
	e := NewEngine()
	e.TokenBudget = 16384 // softLimit=13107
	e.SetSpillStore(NewMemorySpillStore())

	state := &session.State{}
	// 30 条 × 1800 字符（ASCII）≈ 30 × 508 token ≈ 15200，越过 softLimit=13107 触发压缩。
	// 字符规模按 EstimateTextTokens 的 ASCII 密度 0.28 换算（旧估算为 0.5，高估近 1.8 倍）。
	history := buildToolHistory(30, 1800)
	if e.effectiveTokens(history) <= e.softLimit() {
		t.Fatalf("测试前提不成立：历史 %d token 未越 softLimit %d",
			e.effectiveTokens(history), e.softLimit())
	}

	if _, err := e.Assemble(context.Background(), "sys", state, history, "继续"); err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if strings.TrimSpace(state.Memory.Summary) == "" {
		t.Error("MemorySummary 为空——结构化摘要阶段仍被跳过，§1 未修复")
	}
}

// ---------------------------------------------------------------------------
// §3：超尺寸单条结果头尾采样
// ---------------------------------------------------------------------------

// TestOversizedResultSampledWithinKeepRecent 验证 §3 的核心：单条巨型工具结果
// 即使落在 keepRecent 保护区内（工具结果总数 ≤ keepRecent，evictToolResults 会直接放弃），
// 也会被头尾采样，突破「免死金牌」。原文完整 offload、可回读，头尾保留、中间折叠。
func TestOversizedResultSampledWithinKeepRecent(t *testing.T) {
	e := NewEngine()
	e.TokenBudget = 10000 // softLimit=8000
	spill := NewMemorySpillStore()
	e.SetSpillStore(spill)

	big := strings.Repeat("A", 40000) // ≈20000 token，单条即撑爆窗口
	msgs := []*schema.Message{
		schema.SystemMessage("sys"),
		assistantCall("call_big", "search"),
		toolMsg("call_big", "search", big),
	}
	if e.effectiveTokens(msgs) <= e.softLimit() {
		t.Fatalf("测试前提不成立：%d token 未越 softLimit %d", e.effectiveTokens(msgs), e.softLimit())
	}
	// 只有 1 条工具结果 ≤ keepRecent，evictToolResults 必然放弃，采样是唯一减负路径

	out := e.CompressInPlace(context.Background(), msgs)

	var sampled *schema.Message
	for _, m := range out {
		if m.Role == schema.Tool && m.ToolCallID == "call_big" {
			sampled = m
		}
	}
	if sampled == nil {
		t.Fatal("巨型工具结果丢失（应被采样保留，而非删除）")
	}
	if got := len([]rune(sampled.Content)); got >= len([]rune(big)) {
		t.Errorf("巨型结果未被采样，体积未下降：%d >= %d", got, len([]rune(big)))
	}
	if !strings.Contains(sampled.Content, "中间已省略") {
		t.Errorf("采样体缺少折叠标记：%q", sampled.Content)
	}
	if !strings.Contains(sampled.Content, "read_result") {
		t.Error("采样体应带 read_result 回读路径")
	}
	if !strings.HasPrefix(sampled.Content, "AAAA") {
		t.Error("采样体未保留头部")
	}
	if !strings.HasSuffix(sampled.Content, "AAAA") {
		t.Error("采样体未保留尾部")
	}
	// 协议配对字段保留，否则下次模型调用报错
	if sampled.ToolCallID == "" || sampled.ToolName == "" {
		t.Errorf("采样体丢失配对字段：toolCallID=%q toolName=%q", sampled.ToolCallID, sampled.ToolName)
	}
	// 原文完整可回读
	got, err := spill.Read(context.Background(), SpillRef("call_big"))
	if err != nil {
		t.Fatalf("回读失败: %v", err)
	}
	if got != big {
		t.Errorf("原文未完整 offload：长度 %d != %d", len(got), len(big))
	}
}

// TestOversizedSamplingIsIdempotent 验证采样幂等：采样后已低于软阈值，
// 二次 CompressInPlace 不再改写序列、不重复 offload（避免反复作废缓存前缀）。
func TestOversizedSamplingIsIdempotent(t *testing.T) {
	e := NewEngine()
	e.TokenBudget = 10000
	spill := NewMemorySpillStore()
	e.SetSpillStore(spill)

	big := strings.Repeat("B", 40000)
	msgs := []*schema.Message{
		schema.SystemMessage("sys"),
		assistantCall("call_big", "search"),
		toolMsg("call_big", "search", big),
	}
	once := e.CompressInPlace(context.Background(), msgs)
	if e.effectiveTokens(once) > e.softLimit() {
		t.Skip("一次采样后仍超阈，需配合淘汰，跳过纯幂等断言")
	}
	if spill.Len() != 1 {
		t.Fatalf("首次采样应 offload 1 条，实际 %d", spill.Len())
	}

	twice := e.CompressInPlace(context.Background(), once)
	if EstimateMessagesTokens(twice) != EstimateMessagesTokens(once) {
		t.Errorf("二次压缩改动了已采样序列：%d → %d", EstimateMessagesTokens(once), EstimateMessagesTokens(twice))
	}
	if spill.Len() != 1 {
		t.Errorf("二次压缩重复 offload：%d 条", spill.Len())
	}
}

// TestSmallResultsNotSampled 验证低于阈值的小结果不被采样（不误伤）。
func TestSmallResultsNotSampled(t *testing.T) {
	e := NewEngine()
	e.TokenBudget = 10000
	e.SetSpillStore(NewMemorySpillStore())

	small := strings.Repeat("c", 400) // ≈200 token，远低于 threshold=1200
	msgs := []*schema.Message{
		schema.SystemMessage("sys"),
		assistantCall("call_s", "search"),
		toolMsg("call_s", "search", small),
	}
	out := e.sampleOversizedResults(context.Background(), msgs)
	for _, m := range out {
		if m.Role == schema.Tool && m.ToolCallID == "call_s" {
			if m.Content != small {
				t.Error("小结果被误采样")
			}
			if isStub(m) {
				t.Error("小结果被误标记为已处理")
			}
		}
	}
}

// TestOversizedNonIdempotentNotSampledWithoutSpill 验证降级安全：未配置 SpillStore 时，
// 非幂等（写类）工具结果不被采样——采样会永久丢失中段，而写结果不可重放。
func TestOversizedNonIdempotentNotSampledWithoutSpill(t *testing.T) {
	e := NewEngine()
	e.TokenBudget = 10000
	e.IsNonIdempotent = func(name string) bool { return name == "edit" }
	// 故意不设 SpillStore

	big := strings.Repeat("C", 40000)
	msgs := []*schema.Message{
		schema.SystemMessage("sys"),
		assistantCall("call_w", "edit"),
		toolMsg("call_w", "edit", big),
	}
	out := e.sampleOversizedResults(context.Background(), msgs)
	for _, m := range out {
		if m.Role == schema.Tool && m.ToolCallID == "call_w" {
			if len([]rune(m.Content)) < len([]rune(big)) {
				t.Error("未配置 spill 时非幂等工具结果被采样，会永久丢失中段")
			}
		}
	}
}

// TestSampleWindowScalesWithThreshold 验证采样窗口随阈值自适应，且不超过绝对上限。
//
// 断言用「换算出的字符数再估回 token 是否仍在预算内」，而不是假定固定的
// 字符/token 比——后者正是旧实现失真的根源。两类密度都要成立：
// ASCII/JSON 类（密度低，字符数多）与中文类（密度高，字符数少）。
func TestSampleWindowScalesWithThreshold(t *testing.T) {
	// 大阈值：受绝对上限约束（任何密度下都会被 sampleHeadChars/sampleTailChars 截住）
	head, tail := sampleWindow(100000, tokensPerASCIIChar)
	if head != sampleHeadChars || tail != sampleTailChars {
		t.Errorf("大阈值下应取绝对上限：head=%d tail=%d", head, tail)
	}

	for _, tc := range []struct {
		name    string
		density float64
		char    rune // 用于构造该密度的样本内容
	}{
		{"ASCII/JSON", tokensPerASCIIChar, 'a'},
		{"中文", tokensPerCJKChar, '中'},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// 取小阈值，确保按比例缩放真正生效而非触顶。
			// 触顶条件是 threshold ≥ sampleHeadChars×2×density，这里刻意远低于它。
			const threshold = 200
			h, tl := sampleWindow(threshold, tc.density)

			if h >= sampleHeadChars || tl >= sampleTailChars {
				t.Errorf("小阈值下应缩小采样窗口：head=%d tail=%d", h, tl)
			}
			if h < 1 || tl < 1 {
				t.Fatalf("采样窗口非法：head=%d tail=%d", h, tl)
			}

			// 核心不变量：头部保留的 token ≈ 阈值的一半，尾部 ≈ 四分之一，均不超预算。
			// 超预算意味着「采样完仍超阈」，等于白做一次压缩。
			headTokens := EstimateTextTokens(strings.Repeat(string(tc.char), h))
			tailTokens := EstimateTextTokens(strings.Repeat(string(tc.char), tl))
			if headTokens > threshold/2 {
				t.Errorf("头部超出预算：保留 %d 字符 ≈ %d token > 阈值一半 %d",
					h, headTokens, threshold/2)
			}
			if tailTokens > threshold/4 {
				t.Errorf("尾部超出预算：保留 %d 字符 ≈ %d token > 阈值四分之一 %d",
					tl, tailTokens, threshold/4)
			}
			if headTokens+tailTokens >= threshold {
				t.Errorf("采样后仍可能超阈：head=%d tail=%d", headTokens, tailTokens)
			}

			// 密度非法时回落到最保守值，不 panic、不返回 0
			for _, bad := range []float64{0, -1, 1.5, 100} {
				bh, bt := sampleWindow(threshold, bad)
				if bh < 1 || bt < 1 {
					t.Errorf("密度 %v 下采样窗口非法：head=%d tail=%d", bad, bh, bt)
				}
			}
		})
	}
}
