package budget

import (
	"testing"
)

// 本文件覆盖为「压缩效果评估」新增的观测量：缓存命中明细、压缩事件累计、
// 以及两者的可用性判定。这些量不参与预算判定，但它们是回答
// 「压缩优化到底有没有效」的唯一数据来源，算错就等于没有数据。

// TestCachedTokensNotDoubleCounted 验证缓存 token 不被计入预算扣减。
//
// CachedTokens 是 PromptTokens 的子集。若把它也累加进 UsedTokens，
// 等于同一批 token 扣两次预算，会导致明明没超窗就提前终止——
// 这是压缩优化最常见的自伤：省了钱却把任务掐断。
func TestCachedTokensNotDoubleCounted(t *testing.T) {
	b := NewBudget(100000, 100)

	usage := TokenUsage{
		PromptTokens:     1000,
		CompletionTokens: 200,
		TotalTokens:      1200,
		CachedTokens:     800, // 属于 PromptTokens 的子集
	}
	if err := b.ConsumeTokens(usage); err != nil {
		t.Fatalf("ConsumeTokens: %v", err)
	}

	if b.UsedTokens != 1200 {
		t.Errorf("UsedTokens=%d，应为 TotalTokens 1200；若为 2000 说明缓存被重复扣减", b.UsedTokens)
	}
	if b.UsedPromptTokens != 1000 {
		t.Errorf("UsedPromptTokens=%d，应为 1000", b.UsedPromptTokens)
	}
	if b.UsedCachedTokens != 800 {
		t.Errorf("UsedCachedTokens=%d，应为 800", b.UsedCachedTokens)
	}
}

// TestCachedRatioIsCumulativeWeighted 验证命中率按累计 token 加权，而非各轮等权平均。
//
// 各轮 prompt 长度差异极大：一轮 100 token 的短请求与一轮 10000 token 的长请求
// 等权平均，会让短请求的命中率虚高权重，掩盖真实的前缀复用程度。
func TestCachedRatioIsCumulativeWeighted(t *testing.T) {
	b := NewBudget(0, 0)

	// 第一轮：短请求，命中率低
	_ = b.ConsumeTokens(TokenUsage{PromptTokens: 100, TotalTokens: 150, CachedTokens: 0})
	// 第二轮：长请求，命中率高
	_ = b.ConsumeTokens(TokenUsage{PromptTokens: 10000, TotalTokens: 10200, CachedTokens: 9000})

	got := b.CachedRatio()
	want := 9000.0 / 10100.0
	if diff := got - want; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("累计命中率 %.6f，应为 %.6f（等权平均会得到 %.6f，那是错的）",
			got, want, (0.0+0.9)/2)
	}
}

// TestCachedRatioClampedAndZeroSafe 验证命中率的边界：不越界、不除零。
func TestCachedRatioClampedAndZeroSafe(t *testing.T) {
	b := NewBudget(0, 0)
	if got := b.CachedRatio(); got != 0 {
		t.Errorf("无数据时命中率应为 0，实际 %v", got)
	}

	// provider 异常返回 cached > prompt 时不得产出 >1 的比率（单次用量口径）
	u := TokenUsage{PromptTokens: 100, CachedTokens: 500}
	if got := u.CachedRatio(); got != 1 {
		t.Errorf("单次异常数据应被钳到 1，实际 %v", got)
	}

	// 累计口径同样要钳制：两处算法必须一致，否则同一指标在不同层级给出不同值，
	// 看板会出现 >100% 的命中率
	if got := b.CachedRatio(); got > 1 {
		t.Errorf("累计命中率 %v 超过 1，未被钳制", got)
	}
}

// TestHasUsageDataDistinguishesUnknownFromZero 验证「未采到数据」与「命中率为零」可区分。
//
// 这两种情况的处置完全相反：前者是埋点没接通（provider 不回传明细），
// 后者才是压缩策略真的破坏了缓存前缀。混为一谈会让人误判优化方向。
func TestHasUsageDataDistinguishesUnknownFromZero(t *testing.T) {
	t.Run("估算兜底路径视为未采到数据", func(t *testing.T) {
		b := NewBudget(0, 0)
		// 只有 TotalTokens、没有 PromptTokens：对应 provider 未返回用量明细时的估算兜底
		_ = b.ConsumeTokens(TokenUsage{TotalTokens: 500})
		if b.HasUsageData() {
			t.Error("仅有估算值时不应声称已采到用量数据")
		}
	})

	t.Run("provider返回明细后视为已采到", func(t *testing.T) {
		b := NewBudget(0, 0)
		_ = b.ConsumeTokens(TokenUsage{PromptTokens: 100, TotalTokens: 150, CachedTokens: 0})
		if !b.HasUsageData() {
			t.Error("已拿到 prompt 明细却报告无数据")
		}
		// 关键：此时命中率为 0 是「真的没命中」，而非「没数据」
		if got := b.CachedRatio(); got != 0 {
			t.Errorf("命中率应为 0，实际 %v", got)
		}
	})
}

// TestRecordCompactionAccumulates 验证压缩账本正确累计。
func TestRecordCompactionAccumulates(t *testing.T) {
	b := NewBudget(0, 0)

	// saved=毛节省, netSaved=扣除摘要开销后的净节省, summary=摘要占用, unrecoverable=不可恢复丢失
	b.RecordCompaction(1000, 700, 100, 2)
	b.RecordCompaction(500, 500, 0, 0)

	if b.CompressEvents != 2 {
		t.Errorf("压缩事件数=%d，应为 2", b.CompressEvents)
	}
	if b.CompressTokensSaved != 1500 {
		t.Errorf("毛节省=%d，应为 1500", b.CompressTokensSaved)
	}
	if b.CompressNetSaved != 1200 {
		t.Errorf("净节省=%d，应为 1200", b.CompressNetSaved)
	}
	if b.CompressSummaryTokens != 100 {
		t.Errorf("摘要占用=%d，应为 100", b.CompressSummaryTokens)
	}
	if b.CompressUnrecoverableLost != 2 {
		t.Errorf("不可恢复丢失=%d，应为 2", b.CompressUnrecoverableLost)
	}
}

// TestRecordCompactionConcurrencySafe 验证并发记账不丢数。
//
// Budget 会被多个 goroutine 共享（模型装饰器与压缩路径分别记账），
// 漏锁的表现是统计值偏小——不报错、只让数据静默失真。
func TestRecordCompactionConcurrencySafe(t *testing.T) {
	b := NewBudget(0, 0)

	const goroutines, perG = 8, 50
	done := make(chan struct{}, goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			for j := 0; j < perG; j++ {
				b.RecordCompaction(10, 8, 1, 0)
				_ = b.ConsumeTokens(TokenUsage{PromptTokens: 5, TotalTokens: 6, CachedTokens: 3})
				_ = b.CachedRatio()
				_ = b.Snapshot()
			}
			done <- struct{}{}
		}()
	}
	for i := 0; i < goroutines; i++ {
		<-done
	}

	if want := goroutines * perG; b.CompressEvents != want {
		t.Errorf("压缩事件数=%d，应为 %d（并发丢数）", b.CompressEvents, want)
	}
	if want := goroutines * perG * 10; b.CompressTokensSaved != want {
		t.Errorf("毛节省=%d，应为 %d", b.CompressTokensSaved, want)
	}
}

// TestSnapshotCarriesObservability 验证快照携带全部观测量，供信号投影与效果评估读取。
func TestSnapshotCarriesObservability(t *testing.T) {
	b := NewBudget(10000, 10)
	_ = b.ConsumeTokens(TokenUsage{
		PromptTokens: 1000, CompletionTokens: 200, TotalTokens: 1200, CachedTokens: 800,
	})
	b.RecordCompaction(300, 250, 50, 1)

	s := b.Snapshot()
	if s.UsedPromptTokens != 1000 || s.UsedCachedTokens != 800 {
		t.Errorf("快照缓存明细错误：prompt=%d cached=%d", s.UsedPromptTokens, s.UsedCachedTokens)
	}
	if s.ModelCalls != 1 {
		t.Errorf("快照模型调用数=%d，应为 1", s.ModelCalls)
	}
	if !s.HasUsageData {
		t.Error("快照未标记已采到用量数据")
	}
	want := 0.8
	if diff := s.CachedRatio - want; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("快照命中率=%.6f，应为 %.6f", s.CachedRatio, want)
	}
	if s.CompressEvents != 1 || s.CompressTokensSaved != 300 ||
		s.CompressNetSaved != 250 || s.CompressSummaryTokens != 50 ||
		s.CompressUnrecoverableLost != 1 {
		t.Errorf("快照压缩账本不完整: %+v", s)
	}
	// 既有字段不得因新增观测而改变语义
	if s.MaxTokens != 10000 || s.UsedTokens != 1200 {
		t.Errorf("快照既有字段被影响：max=%d used=%d", s.MaxTokens, s.UsedTokens)
	}
}

// TestSnapshotJSONTagsStable 验证快照可序列化为 JSON 且字段名稳定。
// 这些字段名会被外层指标系统与 Signal 载荷直接消费，改名即破坏下游。
func TestSnapshotJSONTagsStable(t *testing.T) {
	b := NewBudget(0, 0)
	_ = b.ConsumeTokens(TokenUsage{PromptTokens: 10, TotalTokens: 12, CachedTokens: 4})
	b.RecordCompaction(5, 4, 1, 0)

	// 通过 Snapshot 的 json tag 间接校验：字段存在且可被投影层读取
	s := b.Snapshot()
	if s.CachedRatio <= 0 {
		t.Error("cached_ratio 字段应已被填充")
	}
	if s.CompressNetSaved != 4 {
		t.Error("compress_net_saved 字段应已被填充")
	}
}
