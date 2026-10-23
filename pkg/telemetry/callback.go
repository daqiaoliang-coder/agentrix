package telemetry

import (
	"context"
	"sync"

	"github.com/cloudwego/eino/callbacks"
	"github.com/cloudwego/eino/components"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// Callback 把 eino 图回调翻译为 OTel span 与指标。
//
// 每个 Turn 创建一个实例：实例持有该 Turn 内未关闭的 span，Close 兜底
// 关闭——审批中断时 ToolsNode 拿不到 OnEnd/OnError，span 会悬挂。
//
// span 层级经 ctx 传播：OnStart 返回注入 span 的 ctx，eino 保证同一节点
// 执行的后续时机（OnEnd/OnError）拿到该 ctx（compose.runWithCallbacks
// 链式传递），OnEnd 经 trace.SpanFromContext 取回句柄关闭。
type Callback struct {
	p     *Provider
	scene string

	mu   sync.Mutex
	open map[trace.Span]struct{} // 未关闭 span，Close 兜底用
}

// NewCallback 创建 Turn 级回调实例。scene 作为维度写入 LLM/工具指标。
func (p *Provider) NewCallback(scene string) *Callback {
	return &Callback{p: p, scene: scene, open: map[trace.Span]struct{}{}}
}

// Handler 构造 eino callbacks.Handler。五个时机全部注册：eino 的
// TimingChecker 会跳过未注册时机，流式模式下缺注册的 handler 收不到回调。
func (c *Callback) Handler() callbacks.Handler {
	return callbacks.NewHandlerBuilder().
		OnStartFn(func(ctx context.Context, info *callbacks.RunInfo, input callbacks.CallbackInput) context.Context {
			return c.onStart(ctx, info, input)
		}).
		OnStartWithStreamInputFn(func(ctx context.Context, info *callbacks.RunInfo, input *schema.StreamReader[callbacks.CallbackInput]) context.Context {
			return c.onStartStream(ctx, info, input)
		}).
		OnEndFn(func(ctx context.Context, info *callbacks.RunInfo, output callbacks.CallbackOutput) context.Context {
			return c.onEnd(ctx, info, output)
		}).
		OnEndWithStreamOutputFn(func(ctx context.Context, info *callbacks.RunInfo, output *schema.StreamReader[callbacks.CallbackOutput]) context.Context {
			return c.onEndStream(ctx, info, output)
		}).
		OnErrorFn(func(ctx context.Context, info *callbacks.RunInfo, err error) context.Context {
			return c.onError(ctx, info, err)
		}).
		Build()
}

// Close 兜底关闭本 Turn 内所有未关闭的 span（审批中断路径）。
// 被兜底关闭的 span 打 dangling 属性，便于在后端区分「正常结束」与
// 「中断悬置」的节点执行。
func (c *Callback) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for span := range c.open {
		span.SetAttributes(attribute.Bool(attrDangling, true))
		span.End()
	}
	c.open = map[trace.Span]struct{}{}
}

func (c *Callback) track(span trace.Span) {
	c.mu.Lock()
	c.open[span] = struct{}{}
	c.mu.Unlock()
}

func (c *Callback) finish(span trace.Span) {
	c.mu.Lock()
	delete(c.open, span)
	c.mu.Unlock()
	span.End()
}

func (c *Callback) onStart(ctx context.Context, info *callbacks.RunInfo, input callbacks.CallbackInput) context.Context {
	if info == nil {
		return ctx
	}
	switch info.Component {
	case components.ComponentOfChatModel:
		ctx, span := c.p.tracer.Start(ctx, "agentrix.llm",
			trace.WithAttributes(attribute.String(attrScene, c.scene)))
		c.track(span)
		c.p.llmCount.Add(ctx, 1, metric.WithAttributes(attribute.String(attrScene, c.scene)))
		return ctx
	case compose.ComponentOfToolsNode:
		return c.startTools(ctx, input)
	}
	return ctx
}

// startTools 为一次 ToolsNode 执行开单个 span（节点内工具并发执行、回调
// 只有节点级起止，按调用开 span 只会得到一批起止相同的虚假时长）。
// 各工具调用作为 span 事件记录，参数截断后入属性。
func (c *Callback) startTools(ctx context.Context, input callbacks.CallbackInput) context.Context {
	m, _ := input.(*schema.Message)
	var names []string
	if m != nil {
		for _, tc := range m.ToolCalls {
			names = append(names, tc.Function.Name)
		}
	}
	ctx, span := c.p.tracer.Start(ctx, "agentrix.tools",
		trace.WithAttributes(
			attribute.String(attrScene, c.scene),
			attribute.StringSlice("agentrix.tool.names", names),
		))
	c.track(span)

	toolAttr := metric.WithAttributes(attribute.String(attrScene, c.scene))
	if m != nil {
		for _, tc := range m.ToolCalls {
			span.AddEvent("tool_call", trace.WithAttributes(
				attribute.String("call_id", tc.ID),
				attribute.String("tool", tc.Function.Name),
				attribute.String("arguments", truncateRunes(tc.Function.Arguments, 500)),
			))
			c.p.toolCount.Add(ctx, 1, toolAttr,
				metric.WithAttributes(attribute.String("tool", tc.Function.Name)))
		}
	}
	return ctx
}

func (c *Callback) onEnd(ctx context.Context, info *callbacks.RunInfo, output callbacks.CallbackOutput) context.Context {
	if info == nil {
		return ctx
	}
	switch info.Component {
	case components.ComponentOfChatModel:
		span := trace.SpanFromContext(ctx)
		if m, ok := output.(*schema.Message); ok && m != nil {
			c.recordUsage(ctx, span, m)
		}
		c.finish(span)
	case compose.ComponentOfToolsNode:
		span := trace.SpanFromContext(ctx)
		if msgs, ok := output.([]*schema.Message); ok {
			for _, m := range msgs {
				if m == nil {
					continue
				}
				// 结果截断口径与 projection 信号一致：观测摘要而非数据通道
				span.AddEvent("tool_result", trace.WithAttributes(
					attribute.String("call_id", m.ToolCallID),
					attribute.String("result", truncateRunes(m.Content, 2000)),
				))
			}
		}
		c.finish(span)
	}
	return ctx
}

func (c *Callback) onError(ctx context.Context, info *callbacks.RunInfo, err error) context.Context {
	if info == nil {
		return ctx
	}
	switch info.Component {
	case components.ComponentOfChatModel, compose.ComponentOfToolsNode:
		span := trace.SpanFromContext(ctx)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		c.finish(span)
	}
	return ctx
}

// recordUsage 把 token 用量写入 llm span 属性（GenAI 语义约定键）与指标。
func (c *Callback) recordUsage(ctx context.Context, span trace.Span, m *schema.Message) {
	if m.ResponseMeta == nil || m.ResponseMeta.Usage == nil {
		return
	}
	u := m.ResponseMeta.Usage
	span.SetAttributes(
		attribute.Int("gen_ai.usage.input_tokens", u.PromptTokens),
		attribute.Int("gen_ai.usage.output_tokens", u.CompletionTokens),
	)
	sceneAttr := attribute.String(attrScene, c.scene)
	c.p.llmTokens.Add(ctx, int64(u.PromptTokens),
		metric.WithAttributes(sceneAttr, attribute.String("type", "prompt")))
	c.p.llmTokens.Add(ctx, int64(u.CompletionTokens),
		metric.WithAttributes(sceneAttr, attribute.String("type", "completion")))
}

// onStartStream 处理流式输入时机。回调拿到的是私有副本流，eino 文档要求
// handler 必须 Close 防 goroutine 泄漏：ToolsNode 读出完整输入后按普通
// 路径处理；ChatModel 不需要输入内容，直接关闭。
func (c *Callback) onStartStream(ctx context.Context, info *callbacks.RunInfo, input *schema.StreamReader[callbacks.CallbackInput]) context.Context {
	if info == nil {
		input.Close()
		return ctx
	}
	if info.Component == compose.ComponentOfToolsNode {
		return c.onStart(ctx, info, drainInputMessages(input))
	}
	input.Close()
	return c.onStart(ctx, info, nil)
}

// onEndStream 处理流式输出时机：读空输出流、拼接还原节点输出后按普通
// 路径处理（llm span 的 usage 藏在末帧 ResponseMeta，拼接后统一提取）。
func (c *Callback) onEndStream(ctx context.Context, info *callbacks.RunInfo, output *schema.StreamReader[callbacks.CallbackOutput]) context.Context {
	if info == nil {
		output.Close()
		return ctx
	}
	return c.onEnd(ctx, info, drainOutputMessages(output))
}
