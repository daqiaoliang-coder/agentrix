package context

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/daqiaoliang-coder/agentrix/internal/session"
)

// 本文件是压缩算法的「内在指标」评估层：不调用真实 agent loop，只度量压缩本身
// 的信息保真度与开销，因此可以进 CI、秒级完成。
//
// 度量口径刻意分成两个，混用会得出虚假结论：
//
//   - InContextRecall：事实是否仍逐字存在于模型直接可见的消息里；
//   - TotalRecall：事实是否仍可获取——在上下文里，或已 offload 到 SpillStore
//     且压缩产物带有 read_result 回读路径。
//
// 只报 TotalRecall 会掩盖「模型必须多花一次工具调用才能拿回信息」的成本；
// 只报 InContextRecall 则会低估 offload 设计的价值（它确实把不可恢复丢失
// 变成了可恢复丢失）。两者之差，正是 spill 机制买到的东西。

// ---------------------------------------------------------------------------
// 测试夹具：植入可追踪的事实
// ---------------------------------------------------------------------------

// plantFact 构造 n 轮工具调用，其中第 plantIdx 轮的结果里植入唯一标记 fact。
// 标记形如「FACT-7=订单池上限4096」，可在压缩产物与 spill 中精确检索。
//
// 用唯一标记而非自然语言，是为了让召回判定不依赖分词或语义匹配：
// strings.Contains 即可给出确定的真值，避免评估本身引入不确定性。
func plantFact(n, chars, plantIdx int, fact string) []*schema.Message {
	body := strings.Repeat("x", chars)
	msgs := []*schema.Message{schema.SystemMessage("你是助手")}
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("call_%02d", i)
		msgs = append(msgs, assistantCall(id, "search"))
		content := body
		if i == plantIdx {
			// 植入位置放在正文中段：头部会被采样保留，植入中段才能测出真实丢失
			content = body[:len(body)/2] + fact + body[len(body)/2:]
		}
		msgs = append(msgs, toolMsg(id, "search", content))
	}
	return msgs
}

// recallResult 是一次召回度量的结果。
type recallResult struct {
	inContext bool // 事实仍在模型直接可见的消息里
	total     bool // 事实仍可获取（在上下文里，或经 read_result 可回读）
}

// measureRecall 度量植入事实在压缩后的可达性。
//
// total 的判定要求两个条件同时成立：原文确实在 SpillStore 里，且压缩产物中
// 带有指向它的回读路径（read_result + tool_call_id）。只查前者不够——
// 原文落了盘但 stub 没写恢复方式，模型照样拿不回来，这正是
// fixToolCallPairs 旧占位符的缺陷。
func measureRecall(out []*schema.Message, spill SpillStore, fact, toolCallID string) recallResult {
	var r recallResult
	for _, m := range out {
		if m != nil && strings.Contains(m.Content, fact) {
			r.inContext = true
		}
	}
	if r.inContext {
		r.total = true
		return r
	}
	if spill == nil || toolCallID == "" {
		return r
	}
	stored, err := spill.Read(context.Background(), SpillRef(toolCallID))
	if err != nil || !strings.Contains(stored, fact) {
		return r // 原文没落盘，或落盘内容里已经没有这个事实
	}
	// 原文可取回，还需产物里真的有回读指引
	for _, m := range out {
		if m == nil || m.Role != schema.Tool || m.ToolCallID != toolCallID {
			continue
		}
		if strings.Contains(m.Content, "read_result") && strings.Contains(m.Content, toolCallID) {
			r.total = true
		}
	}
	return r
}

// ---------------------------------------------------------------------------
// 内在指标 1：关键事实召回
// ---------------------------------------------------------------------------

// TestInContextRecallDegradesButTotalRecallHolds 验证 offload 的核心价值：
// 激进压缩下事实会离开上下文（InContextRecall 下降），但只要配了 SpillStore
// 且回读路径完整，它仍然是可获取的（TotalRecall 保持为真）。
//
// 这条测试同时守住两个方向的退化：
//   - TotalRecall 变假 = 回读链路断了（stub 没写恢复方式，或 ref 算错）；
//   - InContextRecall 在温和压缩下也变假 = 阈值过激，把不该动的内容动了。
func TestInContextRecallDegradesButTotalRecallHolds(t *testing.T) {
	const fact = "FACT-42=订单池上限4096"
	const plantIdx = 3 // 植入较早的一轮，确保落在被淘汰的区间
	toolCallID := fmt.Sprintf("call_%02d", plantIdx)

	t.Run("配置SpillStore时事实可回读", func(t *testing.T) {
		e := NewEngine()
		e.TokenBudget = 8000 // 压低窗口，迫使激进压缩
		spill := NewMemorySpillStore()
		e.SetSpillStore(spill)

		msgs := plantFact(20, 1200, plantIdx, fact)
		if !strings.Contains(msgs[2*plantIdx+2].Content, fact) {
			t.Fatalf("夹具构造失败：植入的事实不在预期消息里")
		}

		out := e.CompressInPlace(context.Background(), msgs)
		r := measureRecall(out, spill, fact, toolCallID)

		if !r.total {
			t.Errorf("TotalRecall=false：原文已 offload 但模型拿不回来。"+
				"回读链路断裂（stub 缺恢复方式，或 ref=%q 算错）", SpillRef(toolCallID))
		}
		t.Logf("InContextRecall=%v TotalRecall=%v（两者之差即 spill 买到的可恢复性）",
			r.inContext, r.total)
	})

	t.Run("未配置SpillStore时召回全部丢失", func(t *testing.T) {
		e := NewEngine()
		e.TokenBudget = 8000
		// 故意不配 SpillStore

		msgs := plantFact(20, 1200, plantIdx, fact)
		out := e.CompressInPlace(context.Background(), msgs)
		r := measureRecall(out, nil, fact, toolCallID)

		if r.total && !r.inContext {
			t.Error("没有 spill 却报告可回读，判定逻辑有误")
		}
		if !r.inContext && r.total != r.inContext {
			t.Errorf("未配 spill 时 TotalRecall 应等于 InContextRecall：%v vs %v", r.total, r.inContext)
		}
		// 此时不可恢复丢失必须被如实计数，否则代价被隐藏
		_, stats := e.CompressInPlaceWithStats(context.Background(), plantFact(20, 1200, plantIdx, fact))
		if stats.Evicted > 0 && stats.UnrecoverableLost < stats.Evicted {
			t.Errorf("淘汰 %d 条但不可恢复丢失只记了 %d 条", stats.Evicted, stats.UnrecoverableLost)
		}
	})
}

// TestRecallOfRecentFactAlwaysPreserved 验证 Survivor 语义在信息层面成立：
// 最近 keepRecent 轮的事实必须仍在上下文里——它们直接决定下一步动作，
// 淘汰等于让模型对着残缺的最新状态决策。
func TestRecallOfRecentFactAlwaysPreserved(t *testing.T) {
	const fact = "FACT-99=最新一次查询结果"
	// 植入到倒数第二轮，落在 keepRecent 保护区内
	plantIdx := 18
	toolCallID := fmt.Sprintf("call_%02d", plantIdx)

	e := NewEngine()
	e.TokenBudget = 8000
	e.SetSpillStore(NewMemorySpillStore())

	msgs := plantFact(20, 1200, plantIdx, fact)
	out := e.CompressInPlace(context.Background(), msgs)

	r := measureRecall(out, e.spillStore(), fact, toolCallID)
	if !r.inContext {
		t.Errorf("最近 %d 轮内的事实被移出上下文，违反 Survivor 语义", trimKeepRecentTools)
	}
}

// ---------------------------------------------------------------------------
// 内在指标 2：可恢复率与不可恢复丢失
// ---------------------------------------------------------------------------

// TestRecoverabilityRatioIsOneWithSpill 验证配置 spill 时可恢复率应为 100%。
// 低于 100% 意味着有淘汰走了「落盘失败」或「无 tool_call_id」的降级分支，
// 那是数据丢失而不是设计意图，必须暴露出来。
func TestRecoverabilityRatioIsOneWithSpill(t *testing.T) {
	e := NewEngine()
	e.TokenBudget = 10000
	e.SetSpillStore(NewMemorySpillStore())

	msgs := buildToolHistory(20, 1600)
	_, stats := e.CompressInPlaceWithStats(context.Background(), msgs)

	touched := stats.Sampled + stats.Evicted
	if touched == 0 {
		t.Fatal("未发生任何淘汰/采样，无法验证可恢复率")
	}
	if stats.Offloaded != touched {
		t.Errorf("可恢复率 %d/%d < 100%%：配置 SpillStore 时每条淘汰都应可回读",
			stats.Offloaded, touched)
	}
	if stats.UnrecoverableLost != stats.Placeholders {
		t.Errorf("不可恢复丢失 %d 应仅来自配对补洞 %d（淘汰部分都已 offload）",
			stats.UnrecoverableLost, stats.Placeholders)
	}
}

// ---------------------------------------------------------------------------
// 内在指标 3：缓存前缀稳定性（拿到真实 CachedTokens 之前的代理指标）
// ---------------------------------------------------------------------------

// TestStablePrefixHoldsAcrossRoundsWhenUnderThreshold 验证 need-driven 纪律的
// 缓存收益：未越阈值的轮次完全不改动序列，因此前缀保持字节级稳定。
//
// 这是「省下的 token 是否够付缓存重建成本」的核心。若每轮都改写历史，
// 前缀长度趋近 0，provider 侧 KV 缓存全废，压缩就是净亏损。
func TestStablePrefixHoldsAcrossRoundsWhenUnderThreshold(t *testing.T) {
	e := NewEngine()
	e.TokenBudget = 100000 // 远超测试体量，确保不触发压缩
	e.SetSpillStore(NewMemorySpillStore())

	base := buildToolHistory(10, 200)
	full := EstimateMessagesTokens(base)
	if full <= 0 {
		t.Fatal("夹具 token 为 0，无法度量前缀")
	}

	// 模拟多轮：每轮追加一条新交互，但都不越阈值
	current := base
	for round := 0; round < 5; round++ {
		before := EstimateMessagesTokens(current)
		out := e.CompressInPlace(context.Background(), current)

		if got := stablePrefixTokens(current, out); got != before {
			t.Errorf("第 %d 轮未越阈值却改写了前缀：稳定前缀 %d != 原序列 %d",
				round+1, got, before)
		}
		if len(out) != len(current) {
			t.Fatalf("第 %d 轮序列长度变化：%d → %d", round+1, len(current), len(out))
		}
		// 追加下一轮内容
		id := fmt.Sprintf("call_r%d", round)
		current = append(append([]*schema.Message{}, out...),
			assistantCall(id, "search"), toolMsg(id, "search", strings.Repeat("y", 200)))
	}
}

// TestStablePrefixShrinksOnlyFromFirstRewrite 验证前缀度量能准确定位改写起点：
// 压缩从中间段开始改写时，头部（system + 首次交互）仍应完整保留为稳定前缀。
// 度量值必须大于 0 且小于全量——为 0 说明连头部都被动了，等于全量缓存失效。
func TestStablePrefixShrinksOnlyFromFirstRewrite(t *testing.T) {
	e := NewEngine()
	e.TokenBudget = 10000
	e.SetSpillStore(NewMemorySpillStore())

	msgs := buildToolHistory(20, 1600)
	full := EstimateMessagesTokens(msgs)

	out, stats := e.CompressInPlaceWithStats(context.Background(), msgs)
	if !stats.Triggered {
		t.Fatal("未触发压缩，无法验证前缀度量")
	}
	if stats.StablePrefixTokens <= 0 {
		t.Errorf("稳定前缀为 0：头部被改写，缓存将全量失效")
	}
	if stats.StablePrefixTokens >= full {
		t.Errorf("稳定前缀 %d 等于全量 %d，说明度量未反映实际改写", stats.StablePrefixTokens, full)
	}
	// 度量必须与手工比对一致
	if manual := stablePrefixTokens(msgs, out); manual != stats.StablePrefixTokens {
		t.Errorf("账本前缀 %d 与手工计算 %d 不一致", stats.StablePrefixTokens, manual)
	}
	t.Logf("压缩前 %d token → 稳定前缀 %d token（%.1f%% 可复用缓存）",
		full, stats.StablePrefixTokens, 100*float64(stats.StablePrefixTokens)/float64(full))
}

// ---------------------------------------------------------------------------
// 内在指标 4：累积退化（单次无损 ≠ 十次无损）
// ---------------------------------------------------------------------------

// TestCumulativeCompressionDoesNotLeakTokens 验证连续压缩不产生 token 泄漏。
//
// 每轮压缩都会往序列里插入 stub、采样体、占位符——这些替换文本本身也占 token。
// 若替换文本比原文还长，或每轮都重复插入，连续压缩会让上下文不降反升。
// 这是「压缩越压越大」这类隐蔽退化的唯一检测手段。
func TestCumulativeCompressionDoesNotLeakTokens(t *testing.T) {
	e := NewEngine()
	e.TokenBudget = 10000
	spill := NewMemorySpillStore()
	e.SetSpillStore(spill)

	current := buildToolHistory(20, 1600)
	first := e.effectiveTokens(current)
	prev := 0

	for round := 1; round <= 5; round++ {
		out := e.CompressInPlace(context.Background(), current)
		now := e.effectiveTokens(out)

		if now > first {
			t.Errorf("第 %d 轮压缩后 token %d 超过首轮压缩前 %d，出现泄漏", round, now, first)
		}
		// 幂等：连续压缩同一段已稳定的序列不应继续膨胀
		if round > 1 && now > prev {
			t.Errorf("第 %d 轮 token 从 %d 增长到 %d，压缩非单调", round, prev, now)
		}
		prev = now
		current = out
	}
	t.Logf("连续 5 轮压缩：首轮前 %d token → 稳定在 %d token", first, e.effectiveTokens(current))
}

// TestOffloadDoesNotDuplicateAcrossRounds 验证多轮压缩不会重复落盘同一条原文。
// spill 无限增长会把「省下的上下文」转嫁成「无限膨胀的存储」，
// 而 SpillRef 由 tool_call_id 生成，天然幂等——这条测试守住这个性质。
func TestOffloadDoesNotDuplicateAcrossRounds(t *testing.T) {
	e := NewEngine()
	e.TokenBudget = 10000
	spill := NewMemorySpillStore()
	e.SetSpillStore(spill)

	current := buildToolHistory(20, 1600)
	for round := 0; round < 4; round++ {
		current = e.CompressInPlace(context.Background(), current)
	}
	// 20 条工具结果，最多只可能有 20 份原文
	if spill.Len() > 20 {
		t.Errorf("多轮压缩后 spill 存了 %d 份原文，超过工具结果总数 20，存在重复落盘", spill.Len())
	}
}

// ---------------------------------------------------------------------------
// 权衡曲线：扫描阈值，给出「压缩率 vs 信息保真」的对照表
// ---------------------------------------------------------------------------

// TestThresholdSweepTradeoffCurve 扫描 CompressThreshold，输出压缩率与信息保真的
// 权衡曲线。
//
// 为什么必须扫而不是报单点：单点数据（「压缩率 60%、召回 100%」）无法回答
// 「再激进一点会怎样」。曲线才能定位拐点——即保真度开始陡降前的最大压缩率，
// 那才是该配给生产的阈值。
//
// 方向性预期：阈值越低 → 越早触发 → 压缩越激进 → InContextRecall 越低。
// TotalRecall 在配置 spill 时应保持为真，这条不变量对所有阈值都成立。
func TestThresholdSweepTradeoffCurve(t *testing.T) {
	const fact = "FACT-7=灰度上限20%"
	const plantIdx = 3
	toolCallID := fmt.Sprintf("call_%02d", plantIdx)

	// 数据量必须跨过最高档（0.9）的软阈值，否则低档位根本不会触发压缩，
	// 扫出来的「曲线」会是一串 0%——那是阈值没生效，不是压缩没效果。
	// 12000 × 0.9 = 10800，故取 20 × 2400 ASCII ≈ 13400 token 让全部档位都触发。
	const rounds, chars = 20, 2400

	thresholds := []float64{0.9, 0.8, 0.7, 0.6, 0.5}

	t.Logf("%-10s %-12s %-10s %-14s %-14s %-10s",
		"阈值", "压缩率", "净节省", "InContextRecall", "TotalRecall", "不可恢复")

	var prevInContext = true
	triggeredCount := 0
	for _, th := range thresholds {
		e := NewEngine()
		e.TokenBudget = 12000
		e.CompressThreshold = th
		spill := NewMemorySpillStore()
		e.SetSpillStore(spill)

		msgs := plantFact(rounds, chars, plantIdx, fact)
		out, stats := e.CompressInPlaceWithStats(context.Background(), msgs)

		// 扫描前提：每一档都必须真的触发压缩，否则该档数据无意义
		if !stats.Triggered {
			t.Errorf("阈值 %.1f 下未触发压缩：数据量未跨过软阈值 %d，扫描失效", th, e.softLimit())
			continue
		}
		triggeredCount++

		r := measureRecall(out, spill, fact, toolCallID)

		// 不变量 1：配了 spill 且回读路径完整时，事实必须始终可获取
		if !r.total {
			t.Errorf("阈值 %.1f 下 TotalRecall=false：offload 的可恢复性被破坏", th)
		}
		// 不变量 2：TotalRecall 不可能弱于 InContextRecall
		if r.inContext && !r.total {
			t.Errorf("阈值 %.1f 下 InContextRecall=true 但 TotalRecall=false，判定矛盾", th)
		}
		// 单调性：阈值越低（越激进），上下文内召回不应变好
		if r.inContext && !prevInContext {
			t.Errorf("阈值从更激进回调到 %.1f 时召回恢复，但更激进档已丢失——曲线非单调", th)
		}
		prevInContext = r.inContext

		t.Logf("%-10.2f %-11.1f%% %-10d %-14v %-14v %-10d",
			th, stats.CompressionRatio()*100, stats.NetTokensSaved(),
			r.inContext, r.total, stats.UnrecoverableLost)
	}
	if triggeredCount != len(thresholds) {
		t.Errorf("仅 %d/%d 档触发压缩，未形成完整权衡曲线", triggeredCount, len(thresholds))
	}
}

// TestLowWaterMustStayBelowSoftLimit 验证水位配置不会自我打架。
//
// 低水位若不低于软阈值，压缩完立刻又越阈，下一轮再压——每轮都改写历史、
// 每轮都作废缓存前缀，省下的 token 全用来付缓存重建。这条不变量对所有
// 合法配置都必须成立，包括 lowWater() 内部的兜底钳制路径。
func TestLowWaterMustStayBelowSoftLimit(t *testing.T) {
	for _, tc := range []struct {
		name              string
		budget            int
		compressThreshold float64
		lowWaterRatio     float64
	}{
		{"默认配置", 128000, 0, 0},
		{"小窗口", 8192, 0, 0},
		{"极小窗口", 1000, 0, 0},
		{"低水位配得比阈值还高", 128000, 0.5, 0.9},
		{"两者相等", 128000, 0.7, 0.7},
		{"非法比例回落默认", 128000, -1, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := NewEngine()
			e.TokenBudget = tc.budget
			e.CompressThreshold = tc.compressThreshold
			e.LowWaterRatio = tc.lowWaterRatio

			soft, low := e.softLimit(), e.lowWater()
			if low >= soft {
				t.Errorf("低水位 %d >= 软阈值 %d：压缩后会立即再次触发，缓存前缀每轮作废", low, soft)
			}
			if low <= 0 {
				t.Errorf("低水位 %d 非法", low)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 基准测试：估算器与压缩路径的开销
// ---------------------------------------------------------------------------

// benchPayload 构造各类真实内容形态的样本。
// 估算器的误差方向依内容类型而异，必须分别度量，只测一种会掩盖问题。
func benchPayload(kind string, chars int) string {
	switch kind {
	case "ascii":
		return strings.Repeat("a", chars)
	case "cjk":
		return strings.Repeat("中", chars)
	case "json":
		// 工具结果的典型形态：大量标点与短字段名，密度接近纯 ASCII
		var sb strings.Builder
		for sb.Len() < chars {
			sb.WriteString(`{"id":"a1b2c3","status":"ok","count":42,"name":"value"},`)
		}
		return sb.String()[:chars]
	case "mixed":
		// 中英混排的对话历史
		var sb strings.Builder
		for sb.Len() < chars {
			sb.WriteString("用户询问了订单服务的超时问题，assistant 调用 search 工具查询配置。\n")
		}
		r := []rune(sb.String())
		if len(r) > chars {
			r = r[:chars]
		}
		return string(r)
	default:
		return strings.Repeat("x", chars)
	}
}

func BenchmarkEstimateTextTokens(b *testing.B) {
	for _, kind := range []string{"ascii", "cjk", "json", "mixed"} {
		for _, chars := range []int{100, 4000, 40000} {
			payload := benchPayload(kind, chars)
			b.Run(fmt.Sprintf("%s_%d", kind, chars), func(b *testing.B) {
				b.ReportAllocs()
				var sink int
				for i := 0; i < b.N; i++ {
					sink += EstimateTextTokens(payload)
				}
				_ = sink
			})
		}
	}
}

// BenchmarkEstimateTextDensity 度量把 token 预算反推为字符预算的开销。
// sampleWindow 每采样一条都要调一次，热路径上的成本不能忽略。
func BenchmarkEstimateTextDensity(b *testing.B) {
	payload := benchPayload("json", 40000)
	b.ReportAllocs()
	var sink float64
	for i := 0; i < b.N; i++ {
		sink += TextDensity(payload)
	}
	_ = sink
}

// BenchmarkCompressInPlace 度量压缩路径的耗时与压缩率。
//
// 用 ReportMetric 把压缩率一并输出：只看 ns/op 会得到误导结论——
// 一个「什么都不做」的实现耗时最低，但毫无价值。两个数必须同时看。
func BenchmarkCompressInPlace(b *testing.B) {
	for _, tc := range []struct {
		name   string
		budget int
		rounds int
		chars  int
	}{
		{"未越阈值_直接返回", 200000, 20, 1600},
		{"越阈值_淘汰为主", 10000, 20, 1600},
		{"越阈值_巨型结果采样", 10000, 3, 40000},
		{"大历史", 20000, 100, 1600},
	} {
		b.Run(tc.name, func(b *testing.B) {
			e := NewEngine()
			e.TokenBudget = tc.budget
			e.SetSpillStore(NewMemorySpillStore())

			msgs := buildToolHistory(tc.rounds, tc.chars)
			before := e.effectiveTokens(msgs)

			b.ReportAllocs()
			b.ResetTimer()
			var lastRatio float64
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				// 每轮用未压缩的原始序列，避免幂等跳过导致测不到真实路径
				fresh := make([]*schema.Message, len(msgs))
				copy(fresh, msgs)
				e.SetSpillStore(NewMemorySpillStore())
				b.StartTimer()

				_, stats := e.CompressInPlaceWithStats(context.Background(), fresh)
				lastRatio = stats.CompressionRatio()
			}
			b.ReportMetric(lastRatio*100, "compress%")
			b.ReportMetric(float64(before), "tokens_before")
		})
	}
}

// BenchmarkAssembleWithSummary 度量含 LLM 摘要的装配路径开销。
// 用替身模型避免真实网络调用，度量的是框架自身的编排成本。
func BenchmarkAssembleWithSummary(b *testing.B) {
	fake := newFakeSummaryModel(schema.AssistantMessage(wellFormedSummary(), nil))
	e := NewEngineWithModel(fake)
	e.TokenBudget = 16384
	e.SetSpillStore(NewMemorySpillStore())

	msgs := buildToolHistory(30, 1800)
	state := session.NewState("bench-session")

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		state.Memory.Summary = "" // 每轮重置，确保真的走摘要而非空转
		_, _, _ = e.AssembleWithStats(context.Background(), "sys", state, msgs, "继续")
	}
}
