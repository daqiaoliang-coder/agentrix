package telemetry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cloudwego/eino/callbacks"
	"github.com/cloudwego/eino/components"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func newTestProvider(t *testing.T) (*Provider, *tracetest.SpanRecorder, *sdkmetric.ManualReader) {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	mr := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(mr))
	p, err := New(tp, mp)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p, rec, mr
}

func findSpan(spans []sdktrace.ReadOnlySpan, name string) sdktrace.ReadOnlySpan {
	for _, s := range spans {
		if s.Name() == name {
			return s
		}
	}
	return nil
}

func attrOf(span sdktrace.ReadOnlySpan, key string) attribute.Value {
	for _, kv := range span.Attributes() {
		if string(kv.Key) == key {
			return kv.Value
		}
	}
	return attribute.Value{}
}

// sumMetric 汇总名为 name 的 Int64 Sum 指标中，属性命中 kv 的数据点总值。
func sumMetric(rm metricdata.ResourceMetrics, name string, kv ...attribute.KeyValue) int64 {
	var total int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				continue
			}
			for _, dp := range sum.DataPoints {
				if attrsMatch(dp.Attributes, kv) {
					total += dp.Value
				}
			}
		}
	}
	return total
}

func attrsMatch(set attribute.Set, kv []attribute.KeyValue) bool {
	for _, want := range kv {
		v, ok := set.Value(want.Key)
		if !ok || v != want.Value {
			return false
		}
	}
	return true
}

// TestCallbackSpanHierarchyAndMetrics 验证同步时机的 span 层级
// （turn → llm/tools）、关键属性与指标计数。
func TestCallbackSpanHierarchyAndMetrics(t *testing.T) {
	p, rec, mr := newTestProvider(t)
	ctx := context.Background()
	ctx, turnSpan := p.StartTurn(ctx, "wbs", "sess-1", "turn-1", false)

	cb := p.NewCallback("wbs")
	h := cb.Handler()

	// 模拟 eino 图执行语义：每个节点从 Turn ctx 派生（节点间不共享回调
	// 返回的 ctx），节点内 OnStart 返回的 ctx 流向 OnEnd——真实图中
	// 节点在独立调度单元执行，否则后一节点会挂到前一节点的 span 下。
	baseCtx := ctx

	// 一轮 LLM（同步时机），末消息带 token 用量
	llmRI := &callbacks.RunInfo{Name: "model", Component: components.ComponentOfChatModel}
	llmCtx := h.OnStart(baseCtx, llmRI, []*schema.Message{schema.UserMessage("hi")})
	reply := schema.AssistantMessage("done", nil)
	reply.ResponseMeta = &schema.ResponseMeta{Usage: &schema.TokenUsage{PromptTokens: 10, CompletionTokens: 5}}
	h.OnEnd(llmCtx, llmRI, reply)

	// 一轮工具节点：OnStart 携带 tool_calls，OnEnd 回填结果
	toolsRI := &callbacks.RunInfo{Name: "tools", Component: compose.ComponentOfToolsNode}
	callMsg := &schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{
		ID:       "call_1",
		Function: schema.FunctionCall{Name: "list_draft", Arguments: `{"page":1}`},
	}}}
	toolsCtx := h.OnStart(baseCtx, toolsRI, callMsg)
	h.OnEnd(toolsCtx, toolsRI, []*schema.Message{
		{Role: schema.Tool, ToolCallID: "call_1", Content: "read-ok"},
	})

	cb.Close()
	p.EndTurn(ctx, turnSpan, "wbs", "ok", 3*time.Millisecond, nil)

	spans := rec.Ended()
	turn := findSpan(spans, "agentrix.turn")
	llm := findSpan(spans, "agentrix.llm")
	tools := findSpan(spans, "agentrix.tools")
	if turn == nil || llm == nil || tools == nil {
		t.Fatalf("span 不全，实际: %v", spanNames(spans))
	}

	// 层级：llm/tools 都是 turn 的直接子代，同一 trace
	for _, s := range []sdktrace.ReadOnlySpan{llm, tools} {
		if s.Parent().SpanID() != turn.SpanContext().SpanID() {
			t.Errorf("%s 的父 span 应为 turn，实际 %v", s.Name(), s.Parent())
		}
		if s.SpanContext().TraceID() != turn.SpanContext().TraceID() {
			t.Errorf("%s 与 turn 不在同一 trace", s.Name())
		}
	}

	// 属性：turn 归属与结局、llm 用量（GenAI 约定键）、tools 工具名清单
	if got := attrOf(turn, attrSession).AsString(); got != "sess-1" {
		t.Errorf("turn 缺少 session 属性，实际 %q", got)
	}
	if got := attrOf(turn, attrOutcome).AsString(); got != "ok" {
		t.Errorf("turn outcome 应为 ok，实际 %q", got)
	}
	if got := attrOf(llm, "gen_ai.usage.input_tokens").AsInt64(); got != 10 {
		t.Errorf("llm input_tokens 应为 10，实际 %d", got)
	}
	if got := attrOf(llm, "gen_ai.usage.output_tokens").AsInt64(); got != 5 {
		t.Errorf("llm output_tokens 应为 5，实际 %d", got)
	}
	if names := attrOf(tools, "agentrix.tool.names").AsStringSlice(); len(names) != 1 || names[0] != "list_draft" {
		t.Errorf("tools 工具名清单应为 [list_draft]，实际 %v", names)
	}

	// 事件：tool_call / tool_result 各一条，结果内容截断入事件
	var evCall, evResult bool
	for _, ev := range tools.Events() {
		if ev.Name == "tool_call" {
			evCall = true
		}
		if ev.Name == "tool_result" {
			evResult = true
		}
	}
	if !evCall || !evResult {
		t.Errorf("tools span 缺少 tool_call/tool_result 事件，实际事件: %v", tools.Events())
	}

	// 指标：turn 计数/时延、llm 计数与双类 token、tool 计数（含工具名维度）
	var rm metricdata.ResourceMetrics
	if err := mr.Collect(ctx, &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	sceneKV := attribute.String(attrScene, "wbs")
	if got := sumMetric(rm, "agentrix.turn.count", sceneKV, attribute.String(attrOutcome, "ok")); got != 1 {
		t.Errorf("turn.count 应为 1，实际 %d", got)
	}
	if got := sumMetric(rm, "agentrix.llm.count", sceneKV); got != 1 {
		t.Errorf("llm.count 应为 1，实际 %d", got)
	}
	if got := sumMetric(rm, "agentrix.llm.tokens", sceneKV, attribute.String("type", "prompt")); got != 10 {
		t.Errorf("llm.tokens(prompt) 应为 10，实际 %d", got)
	}
	if got := sumMetric(rm, "agentrix.llm.tokens", sceneKV, attribute.String("type", "completion")); got != 5 {
		t.Errorf("llm.tokens(completion) 应为 5，实际 %d", got)
	}
	if got := sumMetric(rm, "agentrix.tool.count", sceneKV, attribute.String("tool", "list_draft")); got != 1 {
		t.Errorf("tool.count 应为 1，实际 %d", got)
	}
}

// TestCallbackStreamTimings 验证流式时机：llm span 在流式输入/输出下
// 正常开闭，usage 从拼接后的全量消息提取（藏在末帧 ResponseMeta）。
func TestCallbackStreamTimings(t *testing.T) {
	p, rec, _ := newTestProvider(t)
	ctx := context.Background()
	ctx, turnSpan := p.StartTurn(ctx, "wbs", "sess-s", "turn-s", false)

	cb := p.NewCallback("wbs")
	h := cb.Handler()

	llmRI := &callbacks.RunInfo{Component: components.ComponentOfChatModel}
	inSR := schema.StreamReaderFromArray([]callbacks.CallbackInput{
		schema.UserMessage("hi"),
	})
	ctx = h.OnStartWithStreamInput(ctx, llmRI, inSR)

	f1 := schema.AssistantMessage("do", nil)
	f2 := schema.AssistantMessage("ne", nil)
	f2.ResponseMeta = &schema.ResponseMeta{Usage: &schema.TokenUsage{PromptTokens: 3, CompletionTokens: 2}}
	outSR := schema.StreamReaderFromArray([]callbacks.CallbackOutput{f1, f2})
	ctx = h.OnEndWithStreamOutput(ctx, llmRI, outSR)

	cb.Close()
	p.EndTurn(ctx, turnSpan, "wbs", "ok", time.Millisecond, nil)

	llm := findSpan(rec.Ended(), "agentrix.llm")
	if llm == nil {
		t.Fatalf("流式时机下 llm span 未生成，实际: %v", spanNames(rec.Ended()))
	}
	if got := attrOf(llm, "gen_ai.usage.input_tokens").AsInt64(); got != 3 {
		t.Errorf("流式拼接后 input_tokens 应为 3，实际 %d", got)
	}
	if llm.Parent().SpanID() != turnSpan.SpanContext().SpanID() {
		t.Error("流式时机下 llm span 仍应挂在 turn 下")
	}
}

// TestCallbackCloseDanglingSpans 验证审批中断路径：ToolsNode 的 OnStart
// 已发射但永远等不到 OnEnd/OnError，Close 须兜底关闭并打 dangling 标记。
func TestCallbackCloseDanglingSpans(t *testing.T) {
	p, rec, _ := newTestProvider(t)
	ctx := context.Background()
	ctx, _ = p.StartTurn(ctx, "wbs", "sess-d", "turn-d", false)

	cb := p.NewCallback("wbs")
	h := cb.Handler()
	toolsRI := &callbacks.RunInfo{Component: compose.ComponentOfToolsNode}
	ctx = h.OnStart(ctx, toolsRI, &schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{
		ID:       "call_x",
		Function: schema.FunctionCall{Name: "edit_draft", Arguments: `{}`},
	}}})
	// 模拟审批中断：不再有 OnEnd/OnError
	_ = ctx

	cb.Close()
	tools := findSpan(rec.Ended(), "agentrix.tools")
	if tools == nil {
		t.Fatal("中断路径上的 tools span 未被 Close 兜底关闭")
	}
	if !attrOf(tools, attrDangling).AsBool() {
		t.Error("兜底关闭的 span 应打 dangling 标记")
	}
}

// TestOnErrorMarksSpanError 验证节点错误路径：span 记录错误并置 Error 状态。
func TestOnErrorMarksSpanError(t *testing.T) {
	p, rec, _ := newTestProvider(t)
	ctx := context.Background()
	ctx, turnSpan := p.StartTurn(ctx, "wbs", "sess-e", "turn-e", false)

	cb := p.NewCallback("wbs")
	h := cb.Handler()
	llmRI := &callbacks.RunInfo{Component: components.ComponentOfChatModel}
	ctx = h.OnStart(ctx, llmRI, nil)
	ctx = h.OnError(ctx, llmRI, context.DeadlineExceeded)
	cb.Close()

	p.EndTurn(ctx, turnSpan, "wbs", "error", time.Millisecond, context.DeadlineExceeded)

	llm := findSpan(rec.Ended(), "agentrix.llm")
	if llm == nil {
		t.Fatal("错误路径 llm span 未关闭")
	}
	if llm.Status().Code != 1 { // codes.Error == 1
		t.Errorf("llm span 应置 Error 状态，实际 %v", llm.Status())
	}
	turn := findSpan(rec.Ended(), "agentrix.turn")
	if turn == nil || turn.Status().Code != 1 {
		t.Errorf("错误结局的 turn span 应置 Error 状态，实际 %+v", turn)
	}
}

// TestSetupFromEnv 验证环境变量开关：endpoint 为空返回 (nil, nil)；
// 非空时构造成功（OTLP HTTP exporter 惰性连接，无需 collector 在线）。
func TestSetupFromEnv(t *testing.T) {
	t.Setenv("AGENTRIX_OTEL_ENDPOINT", "")
	p, err := SetupFromEnv(context.Background())
	if err != nil || p != nil {
		t.Fatalf("endpoint 为空应返回 (nil, nil)，实际 (%v, %v)", p, err)
	}
	if p.Enabled() {
		t.Error("nil Provider 的 Enabled 应为 false")
	}

	t.Setenv("AGENTRIX_OTEL_ENDPOINT", "http://127.0.0.1:1")
	p, err = SetupFromEnv(context.Background())
	if err != nil {
		t.Fatalf("endpoint 非空应构造成功: %v", err)
	}
	if !p.Enabled() {
		t.Error("构造成功后 Enabled 应为 true")
	}
	// endpoint 不可达，flush 必然失败——只取停止后台 goroutine 的效果
	_ = p.Shutdown(context.Background())

	// Shutdown 会触发 exporter flush。用本地 HTTP 服务器接住 OTLP 请求，
	// 既验证导出链路真实可用，又避免依赖外部 collector。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	t.Setenv("AGENTRIX_OTEL_ENDPOINT", srv.URL)
	p2, err := SetupFromEnv(context.Background())
	if err != nil {
		t.Fatalf("本地 collector 下构造失败: %v", err)
	}
	if err := p2.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown flush 应成功: %v", err)
	}
}

func spanNames(spans []sdktrace.ReadOnlySpan) []string {
	names := make([]string, 0, len(spans))
	for _, s := range spans {
		names = append(names, s.Name())
	}
	return names
}
