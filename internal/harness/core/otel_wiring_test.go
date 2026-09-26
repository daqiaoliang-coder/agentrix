package core

import (
	"context"
	"testing"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/daqiaoliang-coder/agentrix/internal/scene"
	"github.com/daqiaoliang-coder/agentrix/internal/session"
	"github.com/daqiaoliang-coder/agentrix/internal/telemetry"
	agenttool "github.com/daqiaoliang-coder/agentrix/internal/tool"
)

// setupTestTelemetry 装配内存版遥测并设为进程默认，返回 span 记录器与
// 指标读取器。测试结束须调用返回的 cleanup 恢复禁用，避免串扰其他用例。
func setupTestTelemetry(t *testing.T) (*tracetest.SpanRecorder, *sdkmetric.ManualReader, func()) {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	mr := sdkmetric.NewManualReader()
	p, err := telemetry.New(tp, sdkmetric.NewMeterProvider(sdkmetric.WithReader(mr)))
	if err != nil {
		t.Fatalf("telemetry.New: %v", err)
	}
	telemetry.SetDefault(p)
	return rec, mr, func() { telemetry.SetDefault(nil) }
}

func otelSpanNames(spans []sdktrace.ReadOnlySpan) []string {
	names := make([]string, 0, len(spans))
	for _, s := range spans {
		names = append(names, s.Name())
	}
	return names
}

func otelFindSpan(spans []sdktrace.ReadOnlySpan, name string) sdktrace.ReadOnlySpan {
	for _, s := range spans {
		if s.Name() == name {
			return s
		}
	}
	return nil
}

func otelAttr(span sdktrace.ReadOnlySpan, key string) attribute.Value {
	for _, kv := range span.Attributes() {
		if string(kv.Key) == key {
			return kv.Value
		}
	}
	return attribute.Value{}
}

func otelMetricSum(rm metricdata.ResourceMetrics, name string, kv ...attribute.KeyValue) int64 {
	var total int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			if sum, ok := m.Data.(metricdata.Sum[int64]); ok {
				for _, dp := range sum.DataPoints {
					match := true
					for _, want := range kv {
						v, ok := dp.Attributes.Value(want.Key)
						if !ok || v != want.Value {
							match = false
							break
						}
					}
					if match {
						total += dp.Value
					}
				}
			}
		}
	}
	return total
}

// TestRunEmitsOTelSpans 验证真实图执行下（同步路径）的遥测接线：
// turn 根 span 携带场景/会话属性，llm/tools 子 span 同 trace 挂在其下，
// Turn 指标按结局计数。scriptModel 无 usage，llm 用量属性缺省属预期。
func TestRunEmitsOTelSpans(t *testing.T) {
	rec, mr, cleanup := setupTestTelemetry(t)
	defer cleanup()

	ctx := context.Background()
	m := &scriptModel{replies: []*schema.Message{
		toolCallMsg("list_draft", `{}`),
		schema.AssistantMessage("已完成", nil),
	}}
	agent, err := NewAgent(ctx, &scene.SceneConfig{
		Key: "wbs", Model: m,
		Tools:         []tool.BaseTool{&fakeReadTool{name: "list_draft"}},
		MaxIterations: 4,
	}, session.NewMemoryStore())
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	if _, err := agent.Run(ctx, "sess-otel", "列出草稿"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	spans := rec.Ended()
	turn := otelFindSpan(spans, "agentrix.turn")
	if turn == nil {
		t.Fatalf("缺少 turn 根 span，实际: %v", otelSpanNames(spans))
	}
	if got := otelAttr(turn, "agentrix.scene").AsString(); got != "wbs" {
		t.Errorf("turn span 场景属性应为 wbs，实际 %q", got)
	}
	if got := otelAttr(turn, "agentrix.session").AsString(); got != "sess-otel" {
		t.Errorf("turn span 会话属性应为 sess-otel，实际 %q", got)
	}
	if got := otelAttr(turn, "agentrix.outcome").AsString(); got != "ok" {
		t.Errorf("成功 Turn 的 outcome 应为 ok，实际 %q", got)
	}

	// 两轮模型调用 → 两个 llm span；一轮工具节点 → 一个 tools span
	var llmCount int
	tools := otelFindSpan(spans, "agentrix.tools")
	for _, s := range spans {
		if s.Name() != "agentrix.llm" && s.Name() != "agentrix.tools" {
			continue
		}
		if s.SpanContext().TraceID() != turn.SpanContext().TraceID() {
			t.Errorf("%s 未挂在 turn 的 trace 下", s.Name())
		}
		if s.Parent().SpanID() != turn.SpanContext().SpanID() {
			t.Errorf("%s 的父 span 应为 turn", s.Name())
		}
		if s.Name() == "agentrix.llm" {
			llmCount++
		}
	}
	if llmCount != 2 {
		t.Errorf("两轮模型调用应产生 2 个 llm span，实际 %d", llmCount)
	}
	if tools == nil {
		t.Error("工具节点 span 缺失")
	}

	// Turn 指标经真实执行路径导出
	var rm metricdata.ResourceMetrics
	if err := mr.Collect(ctx, &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if got := otelMetricSum(rm, "agentrix.turn.count",
		attribute.String("agentrix.scene", "wbs"), attribute.String("agentrix.outcome", "ok")); got != 1 {
		t.Errorf("turn.count(scene=wbs,outcome=ok) 应为 1，实际 %d", got)
	}
	if got := otelMetricSum(rm, "agentrix.tool.count",
		attribute.String("tool", "list_draft")); got != 1 {
		t.Errorf("tool.count(tool=list_draft) 应为 1，实际 %d", got)
	}
}

// TestRunOTelApprovalOutcome 验证审批中断 Turn 的遥测语义：
// turn span 的 outcome 为 approval_required 且状态不置 Error
// （中断是暂停而非失败），指标按同口径计数。
func TestRunOTelApprovalOutcome(t *testing.T) {
	rec, mr, cleanup := setupTestTelemetry(t)
	defer cleanup()

	ctx := context.Background()
	catalog := agenttool.NewCatalog()
	mustRegisterSpec(t, catalog, agenttool.CommandSpec{
		ToolName: "edit_draft", Resource: "wbs-arrangement", Command: "edit-draft",
		Exec: agenttool.ExecInProcess, NeedsApproval: true, ApprovalReason: "修改排期需人工授权",
	})
	m := &scriptModel{replies: []*schema.Message{
		toolCallMsg("edit_draft", `{"payload":"x"}`),
	}}
	agent, err := NewAgent(ctx, &scene.SceneConfig{
		Key: "wbs", Model: m,
		Tools:         []tool.BaseTool{&fakeWriteTool{name: "edit_draft"}},
		Catalog:       catalog,
		MaxIterations: 3,
	}, session.NewMemoryStore())
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	_, runErr := agent.Run(ctx, "sess-otel-approval", "改一下")
	if _, ok := ExtractApprovalRequired(runErr); !ok {
		t.Fatalf("应触发审批中断，err=%v", runErr)
	}

	spans := rec.Ended()
	turn := otelFindSpan(spans, "agentrix.turn")
	if turn == nil {
		t.Fatalf("缺少 turn span，实际: %v", otelSpanNames(spans))
	}
	if got := otelAttr(turn, "agentrix.outcome").AsString(); got != "approval_required" {
		t.Errorf("审批中断 Turn 的 outcome 应为 approval_required，实际 %q", got)
	}
	if turn.Status().Code == 1 { // codes.Error
		t.Error("审批中断是暂停而非失败，turn span 不应置 Error")
	}

	var rm metricdata.ResourceMetrics
	if err := mr.Collect(ctx, &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if got := otelMetricSum(rm, "agentrix.turn.count",
		attribute.String("agentrix.outcome", "approval_required")); got != 1 {
		t.Errorf("turn.count(outcome=approval_required) 应为 1，实际 %d", got)
	}
}
