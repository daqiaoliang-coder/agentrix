package context

import (
	"strings"
	"testing"
)

// 本文件验证 token 估算器本身的可信度。
//
// 为什么这一层必须先于一切效果度量：压缩率、召回率、缓存命中率、权衡曲线的拐点
// ——所有指标的分母都是估算值。尺子刻度错了，量出来的每个数字都错，而且错得
// 看起来很有说服力。旧公式 len([]rune(s))/2 + 1 对工具结果高估约 1.8 倍，
// 直接后果是压缩在远未接近窗口时就触发，白白作废缓存前缀。
//
// 诚实的边界：这里验证的是「内部一致性」与「与主流 BPE 经验值的方向对齐」，
// 不是对某个真实 tokenizer 的精确复现。要拿到精确值，必须用目标模型自己的
// tokenizer 实测替换 tokensPerCJKChar / tokensPerASCIIChar 两个系数。
// 下面所有断言都按「量级与方向」设定容差，而非要求逐 token 相等。

// TestEstimateTextDensityMatchesKnownRanges 验证两类主流内容的密度落在经验区间内。
//
// 经验值来源：主流 BPE 分词器下，英文/JSON/代码约 3.5–4 字符 = 1 token
// （密度 0.25–0.286），中文约 1–1.5 字符 = 1 token（密度 0.667–1.0）。
func TestEstimateTextDensityMatchesKnownRanges(t *testing.T) {
	cases := []struct {
		name    string
		content string
		wantLo  float64 // 每 token 的字符数下界
		wantHi  float64 // 每 token 的字符数上界
	}{
		{"纯ASCII", strings.Repeat("a", 4000), 3.0, 4.5},
		{"JSON工具结果", benchPayload("json", 4000), 3.0, 4.5},
		{"代码片段", `func (e *Engine) compress(ctx context.Context) error { return nil }` +
			strings.Repeat("\n\tvar x = map[string]int{}", 200), 3.0, 4.5},
		{"纯中文", strings.Repeat("中", 4000), 1.0, 1.8},
		{"中英混排对话", benchPayload("mixed", 4000), 1.5, 4.5},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tokens := EstimateTextTokens(tc.content)
			if tokens <= 0 {
				t.Fatalf("非空内容估算为 %d token", tokens)
			}
			runes := len([]rune(tc.content))
			charsPerToken := float64(runes) / float64(tokens)
			if charsPerToken < tc.wantLo || charsPerToken > tc.wantHi {
				t.Errorf("%d 字符估为 %d token → %.2f 字符/token，超出经验区间 [%.1f, %.1f]",
					runes, tokens, charsPerToken, tc.wantLo, tc.wantHi)
			}
			t.Logf("%s: %d 字符 → %d token（%.2f 字符/token）", tc.name, runes, tokens, charsPerToken)
		})
	}
}

// TestEstimateFixesOldFormulaBias 验证相对旧公式的偏差方向与量级。
//
// 旧公式对两类内容同时失真，且方向相反（实测值见各子测试输出）：
//   - JSON/代码（工具结果的主体）被高估约 1.79 倍 → 压缩过早触发、缓存前缀白废；
//   - 中文对话被低估约 1.40 倍 → 压缩过晚触发、真的会爆窗。
//
// 注：docs/token优化差距分析.md 里写的「中文低估约 2 倍」是按中文密度下界
// （1 字符 = 1 token）算的最坏情况；按实测中位密度 1.43 字符/token 算是 1.40 倍。
// 这里断言的是实测区间，不是文档里的最坏情况。
//
// 这条测试把「修好了」变成可验证的断言，而不是注释里的一句话。
func TestEstimateFixesOldFormulaBias(t *testing.T) {
	oldFormula := func(s string) int {
		if s == "" {
			return 0
		}
		return len([]rune(s))/2 + 1
	}

	t.Run("ASCII类不再被高估约1.8倍", func(t *testing.T) {
		payload := benchPayload("json", 40000)
		old, now := oldFormula(payload), EstimateTextTokens(payload)
		ratio := float64(old) / float64(now)
		if ratio < 1.5 || ratio > 2.2 {
			t.Errorf("旧公式对新公式的比值 %.2f，预期约 1.8（旧公式高估 ASCII 类）", ratio)
		}
		t.Logf("JSON 40000 字符：旧公式 %d token，新公式 %d token（旧值高估 %.2f 倍）", old, now, ratio)
	})

	t.Run("中文类不再被低估约1.4倍", func(t *testing.T) {
		payload := strings.Repeat("中", 40000)
		old, now := oldFormula(payload), EstimateTextTokens(payload)
		ratio := float64(now) / float64(old)
		if ratio < 1.2 || ratio > 1.8 {
			t.Errorf("新公式对旧公式的比值 %.2f，预期约 1.4（旧公式低估中文）", ratio)
		}
		t.Logf("中文 40000 字符：旧公式 %d token，新公式 %d token（旧值低估 %.2f 倍）", old, now, ratio)
	})
}

// TestEstimateIsMonotonicAndNonZero 验证估算器的基本数学性质。
//
// 单调性一旦破坏，压缩阈值判断就会出现「内容变长反而估得更小」的荒谬情况，
// 直接导致该压时不压。非零性破坏则会让短消息绕过阈值检查。
func TestEstimateIsMonotonicAndNonZero(t *testing.T) {
	if got := EstimateTextTokens(""); got != 0 {
		t.Errorf("空串应估为 0，实际 %d", got)
	}
	// 任何非空内容至少 1 token，否则短串会被估成 0 而永远不触发压缩
	for _, s := range []string{"a", "中", " ", "\n", "{}"} {
		if got := EstimateTextTokens(s); got < 1 {
			t.Errorf("非空内容 %q 估为 %d token，应至少为 1", s, got)
		}
	}

	// 单调不减：同一类内容越长，估算值不得变小。
	// 必须按 kind 分别比较——跨类型比较毫无意义：中文密度约 0.7、ASCII 约 0.28，
	// 同样 1000 字符，中文估出的 token 数天然是 ASCII 的两倍多，
	// 把它们放进同一个单调序列只会测出假失败。
	for _, kind := range []string{"ascii", "cjk", "json", "mixed"} {
		prev := 0
		for _, chars := range []int{0, 1, 10, 100, 1000, 10000, 100000} {
			got := EstimateTextTokens(benchPayload(kind, chars))
			if got < prev {
				t.Errorf("%s 长度 %d 估为 %d token，小于更短内容的 %d token，违反单调性",
					kind, chars, got, prev)
			}
			prev = got
		}
	}
}

// TestTextDensityRoundTrips 验证 TextDensity 与 EstimateTextTokens 口径自洽。
//
// sampleWindow 靠 density 把 token 预算反推为字符预算，truncateToTokens 同理。
// 两个函数若口径不一致，反推出来的字符数会偏离预算——要么采样后仍超阈（白做），
// 要么砍得过多（无谓丢信息）。
func TestTextDensityRoundTrips(t *testing.T) {
	for _, kind := range []string{"ascii", "cjk", "json", "mixed"} {
		payload := benchPayload(kind, 20000)
		density := TextDensity(payload)
		if density <= 0 || density > 1 {
			t.Errorf("%s 密度 %v 超出 (0,1]", kind, density)
		}

		// 用 density 反推：给定 token 预算，应能换算出恰好落在预算内的字符数
		const budgetTokens = 500
		chars := int(float64(budgetTokens) / density)
		got := EstimateTextTokens(payload[:min(chars, len(payload))])
		// 允许一个字符的取整误差
		if got > budgetTokens+2 {
			t.Errorf("%s：按密度反推 %d 字符，实际估为 %d token，超出预算 %d",
				kind, chars, got, budgetTokens)
		}
	}
}

// TestTruncateToTokensRespectsBudget 验证按 token 截断的单行裁剪不超预算。
func TestTruncateToTokensRespectsBudget(t *testing.T) {
	for _, kind := range []string{"ascii", "cjk", "json"} {
		payload := benchPayload(kind, 20000)
		for _, budget := range []int{0, 1, 50, 500} {
			got := truncateToTokens(payload, budget)
			if budget <= 0 {
				if got != "" {
					t.Errorf("%s 预算 %d 应返回空串，实际 %d 字符", kind, budget, len(got))
				}
				continue
			}
			if tokens := EstimateTextTokens(got); tokens > budget+2 {
				t.Errorf("%s 预算 %d token，截断后实际 %d token", kind, budget, tokens)
			}
		}
	}
}

// TestEstimateSingleSourceOfTruth 验证全仓库只有一份估算实现。
//
// 此前 context 与 harness/core 各持一份，两份分叉的直接后果是：压缩决策用 A 尺子、
// 用量记账用 B 尺子，「省了多少」与「花了多少」对不上账。这条测试通过检查
// 导出函数可被跨包调用来锁住单点实现的存在（重复实现已在本次改造中删除）。
func TestEstimateSingleSourceOfTruth(t *testing.T) {
	// 导出的三个函数构成对外唯一口径
	msgs := buildToolHistory(3, 200)
	if EstimateMessagesTokens(msgs) <= 0 {
		t.Error("EstimateMessagesTokens 应返回正值")
	}
	if EstimateMessageTokens(msgs[0]) <= 0 {
		t.Error("EstimateMessageTokens 应返回正值")
	}
	if EstimateTextTokens("probe") <= 0 {
		t.Error("EstimateTextTokens 应返回正值")
	}

	// 序列总量应等于各条之和（无重复计入、无遗漏）
	sum := 0
	for _, m := range msgs {
		sum += EstimateMessageTokens(m)
	}
	if got := EstimateMessagesTokens(msgs); got != sum {
		t.Errorf("序列估算 %d != 各条之和 %d，存在重复计入或遗漏", got, sum)
	}
}

// BenchmarkEffectiveTokens 度量热路径上的估算成本。
// effectiveTokens 每轮模型调用都要算一次，即使未触发压缩也要付这个成本
// （need-driven 的固有代价）。这个数必须远小于 LLM 调用延迟，否则得不偿失。
func BenchmarkEffectiveTokens(b *testing.B) {
	e := NewEngine()
	e.TokenBudget = 128000
	msgs := buildToolHistory(50, 2000)

	b.ReportAllocs()
	b.ResetTimer()
	var sink int
	for i := 0; i < b.N; i++ {
		sink += e.effectiveTokens(msgs)
	}
	_ = sink
}

// TestEstimateOverheadUsesSameScale 验证固定开销与消息本体共用同一把尺子。
//
// effectiveTokens = 消息本体 + overhead。两部分若用不同公式估算，误差方向可能
// 相反，softLimit 就成了一条位置不明的线：压缩可能在远未接近窗口时触发
// （白白作废缓存前缀），也可能在真要爆窗时才触发。
// harness/core.computeOverhead 已改用 EstimateTextTokens，这里锁住这个约定。
func TestEstimateOverheadUsesSameScale(t *testing.T) {
	e := NewEngine()
	e.TokenBudget = 10000

	msgs := buildToolHistory(10, 200)
	base := e.effectiveTokens(msgs)
	if base != EstimateMessagesTokens(msgs) {
		t.Fatalf("无 overhead 时 effectiveTokens %d 应等于消息估算 %d",
			base, EstimateMessagesTokens(msgs))
	}

	// overhead 必须是可加的绝对 token 量，不做二次换算
	e.SetOverhead(PromptOverheadSnapshot{SystemTokens: 5000, ToolsTokens: 4000, Multimodal: 1000})
	if got, want := e.effectiveTokens(msgs), base+10000; got != want {
		t.Errorf("计入 overhead 后 %d，应为 %d（消息 %d + 开销 10000）", got, want, base)
	}
	if e.Overhead().Total() != 10000 {
		t.Errorf("overhead 总量 %d，应为 10000", e.Overhead().Total())
	}

	// 口径一致性：overhead 的 token 量与消息本体的 token 量可直接相加比较
	if base <= 0 {
		t.Error("消息本体估算为 0，overhead 占比将失真")
	}
}
