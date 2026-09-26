// Package telemetry 提供 OpenTelemetry 可观测导出：Turn 根 span 下挂
// LLM / 工具节点子 span，并导出调用次数、Turn 时延与 token 用量指标。
//
// 经环境变量启用（SetupFromEnv）；未启用时 Default() 返回 nil，
// Agent 执行路径零开销、零行为变化。eino-ext 官方暂无通用 OTel
// callbacks 组件（仅有 langfuse/cozeloop 等 SaaS 定向实现），故本包
// 基于标准 OTel SDK 自实现。
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

// span 属性键：挂在 agentrix 命名空间下，与宿主应用自身的遥测区分。
const (
	attrScene    = "agentrix.scene"
	attrSession  = "agentrix.session"
	attrTurn     = "agentrix.turn"
	attrOutcome  = "agentrix.outcome" // ok / approval_required / error
	attrResuming = "agentrix.resuming"
	attrDangling = "agentrix.dangling" // 审批中断路径上被兜底关闭的 span
)

// Provider 持有 Tracer/Meter 与全部指标仪器。
//
// 指标口径说明：LLM/工具节点只导计数与 token 用量，不导时延直方图——
// 节点时延已由 span 精确承载，而 eino ToolsNode 内工具并发执行、回调
// 只有节点级起止，按节点时长给每个工具记直方图是虚假精度。
type Provider struct {
	tracer trace.Tracer

	turnCount metric.Int64Counter
	turnDur   metric.Float64Histogram
	llmCount  metric.Int64Counter
	llmTokens metric.Int64Counter
	toolCount metric.Int64Counter

	shutdown func(context.Context) error
}

// New 用给定的 TracerProvider/MeterProvider 构造 Provider。
// 生产路径应走 SetupFromEnv；New 主要供测试注入内存 exporter。
func New(tp trace.TracerProvider, mp metric.MeterProvider) (*Provider, error) {
	p := &Provider{tracer: tp.Tracer("github.com/daqiaoliang-coder/agentrix")}
	meter := mp.Meter("github.com/daqiaoliang-coder/agentrix")

	// 仪器名均为合法常量，创建错误理论上不可达；聚合返回而非静默丢弃
	var errs error
	counter := func(name string) metric.Int64Counter {
		c, err := meter.Int64Counter(name)
		errs = errors.Join(errs, err)
		return c
	}
	p.turnCount = counter("agentrix.turn.count")
	p.llmCount = counter("agentrix.llm.count")
	p.llmTokens = counter("agentrix.llm.tokens")
	p.toolCount = counter("agentrix.tool.count")

	var err error
	p.turnDur, err = meter.Float64Histogram("agentrix.turn.duration", metric.WithUnit("ms"))
	errs = errors.Join(errs, err)
	if errs != nil {
		return nil, errs
	}
	return p, nil
}

// Enabled 报告遥测是否激活。nil 接收者返回 false，
// 使 Default() 未设置时调用方无需判空。
func (p *Provider) Enabled() bool { return p != nil }

// Shutdown 冲刷并关闭底层 exporter。未走 SetupFromEnv 的 Provider
// （测试注入）无 shutdown 钩子，调用为 no-op。
func (p *Provider) Shutdown(ctx context.Context) error {
	if p == nil || p.shutdown == nil {
		return nil
	}
	return p.shutdown(ctx)
}

var defaultProvider atomic.Pointer[Provider]

// SetDefault 设置进程级默认 Provider，Agent 执行路径经 Default() 读取。
// 传 nil 恢复禁用（测试清理用）。
func SetDefault(p *Provider) { defaultProvider.Store(p) }

// Default 返回进程级默认 Provider；未设置返回 nil（Enabled() 为 false）。
func Default() *Provider { return defaultProvider.Load() }

// SetupFromEnv 按环境变量装配 OTLP HTTP 导出：
//
//	AGENTRIX_OTEL_ENDPOINT      OTLP HTTP endpoint（如 http://collector:4318），空=禁用
//	AGENTRIX_OTEL_SERVICE_NAME  服务名，默认 agentrix
//
// 禁用是头等路径：endpoint 为空时返回 (nil, nil)，不构造任何 exporter。
func SetupFromEnv(ctx context.Context) (*Provider, error) {
	endpoint := os.Getenv("AGENTRIX_OTEL_ENDPOINT")
	if endpoint == "" {
		return nil, nil
	}
	serviceName := os.Getenv("AGENTRIX_OTEL_SERVICE_NAME")
	if serviceName == "" {
		serviceName = "agentrix"
	}

	res, err := resource.New(ctx, resource.WithAttributes(semconv.ServiceName(serviceName)))
	if err != nil {
		return nil, fmt.Errorf("build resource: %w", err)
	}

	texp, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(endpoint))
	if err != nil {
		return nil, fmt.Errorf("build trace exporter: %w", err)
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(texp),
		sdktrace.WithResource(res),
	)

	mexp, err := otlpmetrichttp.New(ctx, otlpmetrichttp.WithEndpointURL(endpoint))
	if err != nil {
		return nil, fmt.Errorf("build metric exporter: %w", err)
	}
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(mexp)),
		sdkmetric.WithResource(res),
	)

	p, err := New(tp, mp)
	if err != nil {
		return nil, err
	}
	p.shutdown = func(ctx context.Context) error {
		return errors.Join(tp.Shutdown(ctx), mp.Shutdown(ctx))
	}
	return p, nil
}

// StartTurn 开启 Turn 根 span 并把句柄注入 ctx：图回调里创建的
// LLM/工具子 span 经 ctx 自动挂到其下（子代理 Turn 亦同——子代理在
// 父代理工具节点内执行，其根 span 自然成为父工具 span 的后代）。
func (p *Provider) StartTurn(ctx context.Context, scene, sessionID, turnID string, resuming bool) (context.Context, trace.Span) {
	return p.tracer.Start(ctx, "agentrix.turn",
		trace.WithAttributes(
			attribute.String(attrScene, scene),
			attribute.String(attrSession, sessionID),
			attribute.String(attrTurn, turnID),
			attribute.Bool(attrResuming, resuming),
		))
}

// EndTurn 结束 Turn 根 span 并记录 Turn 指标。
// outcome 取 ok / approval_required / error；审批中断是暂停而非失败，
// span 状态保持 OK，仅经 outcome 属性区分。runErr 仅在 outcome=error 时记录。
func (p *Provider) EndTurn(ctx context.Context, span trace.Span, scene, outcome string, dur time.Duration, runErr error) {
	span.SetAttributes(attribute.String(attrOutcome, outcome))
	if outcome == "error" && runErr != nil {
		span.RecordError(runErr)
		span.SetStatus(codes.Error, runErr.Error())
	}
	span.End()

	attrs := metric.WithAttributes(
		attribute.String(attrScene, scene),
		attribute.String(attrOutcome, outcome),
	)
	p.turnCount.Add(ctx, 1, attrs)
	p.turnDur.Record(ctx, float64(dur.Milliseconds()), attrs)
}
