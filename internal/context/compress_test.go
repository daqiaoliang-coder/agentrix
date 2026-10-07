package context

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
)

// toolMsg 构造一条带工具名的工具结果消息。
func toolMsg(id, name, content string) *schema.Message {
	m := schema.ToolMessage(content, id)
	m.ToolName = name
	return m
}

// assistantCall 构造声明某次工具调用的助手消息，用于配对修复。
func assistantCall(id, name string) *schema.Message {
	return &schema.Message{
		Role: schema.Assistant,
		ToolCalls: []schema.ToolCall{{
			ID:       id,
			Function: schema.FunctionCall{Name: name, Arguments: "{}"},
		}},
	}
}

// buildToolHistory 构造 n 条「assistant 声明 + tool 结果」交替的消息序列，
// 每条结果含 chars 个字符。
func buildToolHistory(n, chars int) []*schema.Message {
	body := strings.Repeat("x", chars)
	msgs := make([]*schema.Message, 0, n*2+1)
	msgs = append(msgs, schema.SystemMessage("你是助手"))
	for i := 0; i < n; i++ {
		id := "call_" + string(rune('a'+i))
		msgs = append(msgs, assistantCall(id, "search"))
		msgs = append(msgs, toolMsg(id, "search", body))
	}
	return msgs
}

func mustCompressInPlace(tb testing.TB, e *Engine, messages []*schema.Message) []*schema.Message {
	tb.Helper()
	out, err := e.CompressInPlace(context.Background(), messages)
	if err != nil {
		tb.Fatalf("CompressInPlace: %v", err)
	}
	return out
}

func mustCompressInPlaceWithStats(tb testing.TB, e *Engine, messages []*schema.Message) ([]*schema.Message, CompressStats) {
	tb.Helper()
	out, stats, err := e.CompressInPlaceWithStats(context.Background(), messages)
	if err != nil {
		tb.Fatalf("CompressInPlaceWithStats: %v", err)
	}
	return out, stats
}

type failingSpillStore struct{}

func (failingSpillStore) Spill(context.Context, string, string) error {
	return errors.New("spill failed")
}

func (failingSpillStore) Read(context.Context, string) (string, error) {
	return "", errors.New("read failed")
}

// TestCompressInPlaceSkipsWhenUnderSoftLimit 验证 need-driven 纪律：
// 未越 softLimit 时消息序列必须原样返回，一条都不动。
// 这是修复「每轮无条件裁剪破坏 KV 缓存前缀」的核心断言。
func TestCompressInPlaceSkipsWhenUnderSoftLimit(t *testing.T) {
	e := NewEngine()
	e.ModelContextWindow = 100000 // softLimit = 80000，远超测试消息体量
	e.SetSpillStore(NewMemorySpillStore())

	msgs := buildToolHistory(10, 200)
	before := EstimateMessagesTokens(msgs)

	out := mustCompressInPlace(t, e, msgs)

	if EstimateMessagesTokens(out) != before {
		t.Errorf("未超阈值却改动了消息：token %d → %d", before, EstimateMessagesTokens(out))
	}
	// 指针级校验：应当原样返回，没有任何替换
	for i := range out {
		if isStub(out[i]) {
			t.Fatalf("未超阈值时第 %d 条被替换为 stub", i)
		}
	}
	if got := e.spillStore().(*MemorySpillStore).Len(); got != 0 {
		t.Errorf("未超阈值时不应产生 offload，实际 %d 条", got)
	}
}

func TestCompressionThresholdBoundaryIsConsistent(t *testing.T) {
	messages := buildToolHistory(10, 1200)
	probe := NewEngine()
	before := probe.effectiveTokens(messages)

	for _, tc := range []struct {
		name   string
		window int
		want   bool
	}{
		{name: "below", window: before + 1, want: false},
		{name: "equal", window: before, want: true},
		{name: "above", window: before - 1, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := NewEngine()
			e.CompressThreshold = 1
			e.ModelContextWindow = tc.window
			if got := e.shouldCompress(messages); got != tc.want {
				t.Fatalf("shouldCompress = %v，期望 %v", got, tc.want)
			}
		})
	}

	inPlace := NewEngine()
	inPlace.CompressThreshold = 1
	inPlace.ModelContextWindow = before
	_, inPlaceStats := mustCompressInPlaceWithStats(t, inPlace, messages)
	if !inPlaceStats.Triggered {
		t.Fatal("循环内压缩在 effectiveTokens == softLimit 时未触发")
	}

	assemble := NewEngine()
	systemPrompt := "你是测试助手"
	userInput := "继续"
	assembled := make([]*schema.Message, 0, len(messages)+2)
	assembled = append(assembled, schema.SystemMessage(systemPrompt))
	assembled = append(assembled, messages...)
	assembled = append(assembled, schema.UserMessage(userInput))
	assemble.CompressThreshold = 1
	assemble.ModelContextWindow = assemble.effectiveTokens(assembled)
	_, assembleStats, err := assemble.AssembleWithStats(context.Background(), systemPrompt, nil, messages, userInput)
	if err != nil {
		t.Fatalf("AssembleWithStats: %v", err)
	}
	if !assembleStats.Triggered {
		t.Fatal("初始装配在 effectiveTokens == softLimit 时未触发")
	}
}

func TestAssembleRejectsOversizedCurrentInput(t *testing.T) {
	const secret = "sensitive-user-payload"
	e := NewEngine()
	e.ModelContextWindow = 100

	_, err := e.Assemble(context.Background(), "sys", nil, nil, strings.Repeat(secret, 100))
	if !errors.Is(err, ErrContextWindowExceeded) {
		t.Fatalf("错误 = %v，期望 ErrContextWindowExceeded", err)
	}
	var limitErr *ContextWindowExceededError
	if !errors.As(err, &limitErr) {
		t.Fatalf("错误类型 = %T，期望 *ContextWindowExceededError", err)
	}
	if limitErr.Window != 100 || limitErr.Tokens <= limitErr.Window {
		t.Fatalf("错误容量信息不正确: %+v", limitErr)
	}
	if limitErr.Reason != ContextWindowReasonCurrentInput {
		t.Fatalf("错误原因 = %q，期望 %q", limitErr.Reason, ContextWindowReasonCurrentInput)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatal("超窗错误泄露了原始用户内容")
	}
}

func TestAssembleRejectsOversizedSystemPrompt(t *testing.T) {
	const secret = "sensitive-system-payload"
	e := NewEngine()
	e.ModelContextWindow = 100

	_, err := e.Assemble(context.Background(), strings.Repeat(secret, 100), nil, nil, "ok")
	var limitErr *ContextWindowExceededError
	if !errors.As(err, &limitErr) {
		t.Fatalf("错误 = %v，期望 *ContextWindowExceededError", err)
	}
	if limitErr.Reason != ContextWindowReasonProtectedContext {
		t.Fatalf("错误原因 = %q，期望 %q", limitErr.Reason, ContextWindowReasonProtectedContext)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatal("超窗错误泄露了原始 system prompt")
	}
}

func TestInPlaceRejectsNonIdempotentResultBeyondWindow(t *testing.T) {
	e := NewEngine()
	e.ModelContextWindow = 100
	e.IsNonIdempotent = func(name string) bool { return name == "write" }
	messages := []*schema.Message{
		schema.SystemMessage("sys"),
		assistantCall("call_write", "write"),
		toolMsg("call_write", "write", strings.Repeat("result", 200)),
	}

	_, err := e.CompressInPlace(context.Background(), messages)
	var limitErr *ContextWindowExceededError
	if !errors.As(err, &limitErr) {
		t.Fatalf("错误 = %v，期望 *ContextWindowExceededError", err)
	}
	if limitErr.Reason != ContextWindowReasonNonIdempotentTool {
		t.Fatalf("错误原因 = %q，期望 %q", limitErr.Reason, ContextWindowReasonNonIdempotentTool)
	}
}

func TestInPlaceRejectsAllNonIdempotentResultsBeyondWindow(t *testing.T) {
	e := NewEngine()
	e.ModelContextWindow = 1000
	e.IsNonIdempotent = func(string) bool { return true }
	messages := buildToolHistory(8, 1200)

	out, stats, err := e.CompressInPlaceWithStats(context.Background(), messages)
	var limitErr *ContextWindowExceededError
	if !errors.As(err, &limitErr) {
		t.Fatalf("错误 = %v，期望 *ContextWindowExceededError", err)
	}
	if limitErr.Reason != ContextWindowReasonNonIdempotentTool {
		t.Fatalf("错误原因 = %q，期望 %q", limitErr.Reason, ContextWindowReasonNonIdempotentTool)
	}
	if stats.Sampled != 0 || stats.Evicted != 0 || stats.Triggered {
		t.Fatalf("非幂等结果不应被改写: %+v", stats)
	}
	for i := range messages {
		if out[i].Content != messages[i].Content {
			t.Fatalf("第 %d 条非幂等消息被改写", i)
		}
	}
}

func TestInPlacePreservesResultsWhenSpillFails(t *testing.T) {
	e := NewEngine()
	e.ModelContextWindow = 1000
	e.SetSpillStore(failingSpillStore{})
	messages := []*schema.Message{
		schema.SystemMessage("sys"),
		assistantCall("call_big", "search"),
		toolMsg("call_big", "search", strings.Repeat("result", 2000)),
	}

	out, stats, err := e.CompressInPlaceWithStats(context.Background(), messages)
	var limitErr *ContextWindowExceededError
	if !errors.As(err, &limitErr) {
		t.Fatalf("错误 = %v，期望 *ContextWindowExceededError", err)
	}
	if stats.Sampled != 0 || stats.Evicted != 0 || stats.Offloaded != 0 || stats.Triggered {
		t.Fatalf("Spill 失败后不应报告压缩成功: %+v", stats)
	}
	if out[2].Content != messages[2].Content || isStub(out[2]) {
		t.Fatal("Spill 失败后工具结果未保留原文")
	}
}

func TestEvictionPreservesResultsWhenSpillFails(t *testing.T) {
	e := NewEngine()
	e.SetSpillStore(failingSpillStore{})
	messages := buildToolHistory(8, 400)

	out, evicted, offloaded := e.evictToolResultsCounted(context.Background(), messages, trimKeepRecentTools, 1)
	if evicted != 0 || offloaded != 0 {
		t.Fatalf("Spill 失败后不应淘汰结果: evicted=%d offloaded=%d", evicted, offloaded)
	}
	for i := range messages {
		if out[i].Content != messages[i].Content || isStub(out[i]) {
			t.Fatalf("Spill 失败后第 %d 条消息被改写", i)
		}
	}
}

// TestCompressInPlaceEvictsToLowWater 验证越阈值后 oldest-first + clear_at_least：
// 一次清理到低水位以下即停，且保留最近 keepRecent 条原文。
func TestCompressInPlaceEvictsToLowWater(t *testing.T) {
	e := NewEngine()
	e.ModelContextWindow = 10000 // softLimit=8000, lowWater=6500
	spill := NewMemorySpillStore()
	e.SetSpillStore(spill)

	// 20 条 × 1600 字符（ASCII）≈ 20 × 452 token ≈ 9000，越过 softLimit=8000。
	// 字符规模按 EstimateTextTokens 的 ASCII 密度 0.28 换算，不是旧的 0.5——
	// 旧估算把这类内容高估近 1.8 倍，所以此前 800 字符就够触发，现在需要约两倍。
	msgs := buildToolHistory(20, 1600)
	if e.effectiveTokens(msgs) <= e.softLimit() {
		t.Fatalf("测试前提不成立：初始 token %d 未越 softLimit %d",
			e.effectiveTokens(msgs), e.softLimit())
	}

	out := mustCompressInPlace(t, e, msgs)

	if got := e.effectiveTokens(out); got > e.lowWater() {
		t.Errorf("清理后 token %d 仍高于低水位 %d", got, e.lowWater())
	}

	stubs := 0
	for _, m := range out {
		if isStub(m) {
			stubs++
		}
	}
	if stubs == 0 {
		t.Fatal("越阈值后未淘汰任何工具结果")
	}
	// clear_at_least 的要点是「够了就停」，不应把候选全部清空
	if stubs >= 20-trimKeepRecentTools {
		t.Errorf("淘汰了全部候选（%d 条），未体现一次清到低水位即停", stubs)
	}
	if spill.Len() != stubs {
		t.Errorf("offload 条数 %d 与 stub 数 %d 不一致", spill.Len(), stubs)
	}
}

// TestEvictKeepsRecentToolResults 验证 Survivor 语义：最近 keepRecent 条工具结果不被淘汰。
func TestEvictKeepsRecentToolResults(t *testing.T) {
	e := NewEngine()
	e.ModelContextWindow = 1000 // 阈值压得极低，强制淘汰尽可能多的候选
	spill := NewMemorySpillStore()
	e.SetSpillStore(spill)

	msgs := buildToolHistory(8, 400)
	out := e.evictToolResults(context.Background(), msgs, trimKeepRecentTools, 1)

	// 收集仍是原文的工具结果，按出现顺序应为最后 keepRecent 条
	kept := make([]string, 0)
	for _, m := range out {
		if m.Role == schema.Tool && !isStub(m) {
			kept = append(kept, m.ToolCallID)
		}
	}
	if len(kept) != trimKeepRecentTools {
		t.Fatalf("应保留最近 %d 条工具结果，实际 %d 条: %v", trimKeepRecentTools, len(kept), kept)
	}
	// 最后一条必定是最新的工具调用
	wantLast := "call_" + string(rune('a'+7))
	if kept[len(kept)-1] != wantLast {
		t.Errorf("最新工具结果 %s 被淘汰，保留的是 %v", wantLast, kept)
	}
}

// TestEvictSkipsNonIdempotentTool 验证写/非幂等工具结果不被淘汰：
// 这类结果无法重放，淘汰即永久丢失。
func TestEvictSkipsNonIdempotentTool(t *testing.T) {
	e := NewEngine()
	e.ModelContextWindow = 1000
	e.IsNonIdempotent = func(name string) bool { return name == "edit_draft" }
	spill := NewMemorySpillStore()
	e.SetSpillStore(spill)

	body := strings.Repeat("y", 400)
	msgs := []*schema.Message{schema.SystemMessage("sys")}
	// 最老的一条是写操作，其余是只读检索
	msgs = append(msgs, assistantCall("call_w", "edit_draft"), toolMsg("call_w", "edit_draft", body))
	for i := 0; i < 8; i++ {
		id := "call_r" + string(rune('a'+i))
		msgs = append(msgs, assistantCall(id, "search"), toolMsg(id, "search", body))
	}

	out := e.evictToolResults(context.Background(), msgs, trimKeepRecentTools, 1)

	for _, m := range out {
		if m.Role != schema.Tool {
			continue
		}
		if m.ToolName == "edit_draft" && isStub(m) {
			t.Error("写操作 edit_draft 的结果被淘汰，违反非幂等保护")
		}
	}
	// 只读结果应确有淘汰发生，否则本测试无法证明「跳过」是有效的
	stubs := 0
	for _, m := range out {
		if isStub(m) {
			stubs++
		}
	}
	if stubs == 0 {
		t.Error("只读工具结果也未被淘汰，测试未能验证跳过逻辑")
	}
}

// TestEvictedStubCarriesRecoveryPath 验证占位符可操作：
// stub 必须保留工具身份与恢复路径，而不是无意义的 [truncated]。
func TestEvictedStubCarriesRecoveryPath(t *testing.T) {
	e := NewEngine()
	e.ModelContextWindow = 1000
	spill := NewMemorySpillStore()
	e.SetSpillStore(spill)

	msgs := buildToolHistory(8, 400)
	out := e.evictToolResults(context.Background(), msgs, trimKeepRecentTools, 1)

	var stub *schema.Message
	for _, m := range out {
		if isStub(m) {
			stub = m
			break
		}
	}
	if stub == nil {
		t.Fatal("未产生 stub，无法校验占位符内容")
	}
	for _, want := range []string{"search", "call_a", "read_result", stub.ToolCallID} {
		if !strings.Contains(stub.Content, want) {
			t.Errorf("stub 缺少 %q，实际内容: %s", want, stub.Content)
		}
	}
	// 协议配对字段必须保留，否则下次模型调用会报错
	if stub.ToolCallID == "" || stub.ToolName == "" {
		t.Errorf("stub 丢失配对字段: toolCallID=%q toolName=%q", stub.ToolCallID, stub.ToolName)
	}
}

// TestOffloadedContentIsReadable 验证 offload 可恢复：被淘汰的原文能从 SpillStore 完整取回。
func TestOffloadedContentIsReadable(t *testing.T) {
	e := NewEngine()
	e.ModelContextWindow = 1000
	spill := NewMemorySpillStore()
	e.SetSpillStore(spill)

	const original = "ORIGINAL_PAYLOAD_需要完整取回的结果"
	msgs := []*schema.Message{schema.SystemMessage("sys")}
	msgs = append(msgs, assistantCall("call_x", "search"), toolMsg("call_x", "search", original))
	for i := 0; i < 8; i++ {
		id := "call_p" + string(rune('a'+i))
		msgs = append(msgs, assistantCall(id, "search"), toolMsg(id, "search", strings.Repeat("z", 400)))
	}

	out := e.evictToolResults(context.Background(), msgs, trimKeepRecentTools, 1)

	// 最老的 call_x 应已被淘汰
	var evicted bool
	for _, m := range out {
		if m.ToolCallID == "call_x" && isStub(m) {
			evicted = true
		}
	}
	if !evicted {
		t.Fatal("call_x 未被淘汰，无法验证回读")
	}

	got, err := spill.Read(context.Background(), SpillRef("call_x"))
	if err != nil {
		t.Fatalf("回读失败: %v", err)
	}
	if got != original {
		t.Errorf("回读内容与原文不一致:\n 原文=%q\n 取回=%q", original, got)
	}
}

// TestWithoutSpillStoreStubSaysUnrecoverable 验证未配置 SpillStore 时降级正确：
// stub 必须明确告知不可回读，避免模型误以为能取回而幻觉等待。
func TestWithoutSpillStoreStubSaysUnrecoverable(t *testing.T) {
	e := NewEngine()
	e.ModelContextWindow = 1000
	// 故意不设置 SpillStore

	msgs := buildToolHistory(8, 400)
	out := e.evictToolResults(context.Background(), msgs, trimKeepRecentTools, 1)

	var stub *schema.Message
	for _, m := range out {
		if isStub(m) {
			stub = m
			break
		}
	}
	if stub == nil {
		t.Fatal("未产生 stub")
	}
	if strings.Contains(stub.Content, "read_result") {
		t.Error("未配置 SpillStore 时不应提示 read_result 回读路径")
	}
	if !strings.Contains(stub.Content, "不可回读") {
		t.Errorf("stub 未告知不可回读，实际: %s", stub.Content)
	}
}

// TestEffectiveTokensCountsOverhead 验证不在 messages 中的工具 schema 固定开销
// 必须计入压缩判断，否则阈值失真、压缩触发过晚。
func TestEffectiveTokensCountsOverhead(t *testing.T) {
	e := NewEngine()
	e.ModelContextWindow = 10000 // softLimit = 8000

	// 消息本体很小，远不到 softLimit
	msgs := buildToolHistory(10, 200)
	if msgTokens := EstimateMessagesTokens(msgs); msgTokens >= e.softLimit() {
		t.Fatalf("测试前提不成立：消息本体 %d token 已达 softLimit", msgTokens)
	}
	if e.shouldCompress(msgs) {
		t.Fatal("无 overhead 时不应触发压缩")
	}

	// 注入大额 overhead（模拟庞大的工具 schema）
	e.SetOverhead(PromptOverheadSnapshot{ToolsTokens: 9000})

	if got := e.effectiveTokens(msgs); got != EstimateMessagesTokens(msgs)+9000 {
		t.Errorf("effectiveTokens 未计入 overhead：期望 %d，实际 %d", EstimateMessagesTokens(msgs)+9000, got)
	}
	if !e.shouldCompress(msgs) {
		t.Error("计入 overhead 后应触发压缩，实际未触发——阈值失真问题未修复")
	}
}

// TestCompressInPlaceIsIdempotent 验证冻结语义：已是 stub 的消息不被二次处理，
// 重复压缩不会反复改写同一段历史（避免 creep 失真与无谓的缓存作废）。
func TestCompressInPlaceIsIdempotent(t *testing.T) {
	e := NewEngine()
	e.ModelContextWindow = 10000
	spill := NewMemorySpillStore()
	e.SetSpillStore(spill)

	msgs := buildToolHistory(20, 1600)
	once := mustCompressInPlace(t, e, msgs)
	if !e.shouldCompress(msgs) {
		t.Fatalf("测试前提不成立：%d token 未越 softLimit %d，幂等断言将空转",
			e.effectiveTokens(msgs), e.softLimit())
	}
	firstLen := spill.Len()
	if firstLen == 0 {
		t.Fatal("首次压缩未产生 offload，幂等断言将空转")
	}

	twice := mustCompressInPlace(t, e, once)
	if spill.Len() != firstLen {
		t.Errorf("二次压缩重复 offload：%d → %d 条", firstLen, spill.Len())
	}
	if EstimateMessagesTokens(twice) != EstimateMessagesTokens(once) {
		t.Errorf("二次压缩改动了已稳定的序列：%d → %d token",
			EstimateMessagesTokens(once), EstimateMessagesTokens(twice))
	}
}

// TestEvictPreservesToolCallPairing 验证淘汰后协议自洽：
// 每个 tool 结果都有对应的 tool_call 声明，每个声明都有结果，否则模型调用直接报错。
func TestEvictPreservesToolCallPairing(t *testing.T) {
	e := NewEngine()
	e.ModelContextWindow = 1000
	e.SetSpillStore(NewMemorySpillStore())

	msgs := buildToolHistory(10, 400)
	out := mustCompressInPlace(t, e, msgs)

	declared := map[string]bool{}
	for _, m := range out {
		if m.Role == schema.Assistant {
			for _, tc := range m.ToolCalls {
				declared[tc.ID] = false
			}
		}
	}
	for _, m := range out {
		if m.Role != schema.Tool {
			continue
		}
		if _, ok := declared[m.ToolCallID]; !ok {
			t.Errorf("孤立的 tool 结果 %s（无对应 tool_call 声明）", m.ToolCallID)
			continue
		}
		declared[m.ToolCallID] = true
	}
	for id, answered := range declared {
		if !answered {
			t.Errorf("tool_call %s 缺少对应结果，会导致模型调用报错", id)
		}
	}
}

// TestMultimodalTokensAreCounted 验证多模态分片不再被估成 0 token。
func TestMultimodalTokensAreCounted(t *testing.T) {
	m := &schema.Message{
		Role: schema.User,
		UserInputMultiContent: []schema.MessageInputPart{
			{Type: schema.ChatMessagePartTypeImageURL},
			{Type: schema.ChatMessagePartTypeImageURL},
			{Type: schema.ChatMessagePartTypeText, Text: "看看这两张图"},
		},
	}
	got := EstimateMessageTokens(m)
	if got < 2*multimodalTokensPerPart {
		t.Errorf("两个图片分片至少应计 %d token，实际 %d", 2*multimodalTokensPerPart, got)
	}
}
