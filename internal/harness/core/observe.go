package core

import (
	"context"
	"encoding/json"
	"io"

	"github.com/cloudwego/eino/callbacks"
	"github.com/cloudwego/eino/components"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"

	"github.com/daqiaoliang-coder/agentrix/internal/projection"
	"github.com/daqiaoliang-coder/agentrix/internal/tool/builtin"
)

// signalCallback 把图内回调翻译为过程信号（projection.Signal）。
//
// 与 toolMsgCollector 的分工：collector 收集完整消息供 RawHistory 审计（写存储），
// 本回调只产出轻量运行事实供外层实时观测（写 SSE），两者互不依赖。
//
// 必须同时实现普通与流式两组时机：eino 的 TimingChecker 机制会跳过
// 未注册时机的 handler——图以 Stream 模式运行时，只注册 OnStart/OnEnd
// 的 handler 一个回调都收不到。
type signalCallback struct {
	emitter *projection.Emitter
}

func newSignalCallback(emitter *projection.Emitter) *signalCallback {
	return &signalCallback{emitter: emitter}
}

func (c *signalCallback) handler() callbacks.Handler {
	return callbacks.NewHandlerBuilder().
		OnStartFn(func(ctx context.Context, info *callbacks.RunInfo, input callbacks.CallbackInput) context.Context {
			c.onStart(info, input)
			return ctx
		}).
		OnStartWithStreamInputFn(func(ctx context.Context, info *callbacks.RunInfo, input *schema.StreamReader[callbacks.CallbackInput]) context.Context {
			c.onStart(info, drainInputMessages(input))
			return ctx
		}).
		OnEndFn(func(ctx context.Context, info *callbacks.RunInfo, output callbacks.CallbackOutput) context.Context {
			c.onEnd(info, output)
			return ctx
		}).
		OnEndWithStreamOutputFn(func(ctx context.Context, info *callbacks.RunInfo, output *schema.StreamReader[callbacks.CallbackOutput]) context.Context {
			c.onEndStream(info, output)
			return ctx
		}).
		Build()
}

func (c *signalCallback) onStart(info *callbacks.RunInfo, input callbacks.CallbackInput) {
	if info == nil {
		return
	}
	switch info.Component {
	case components.ComponentOfChatModel:
		c.emitter.Emit(projection.LLMRequesting, nil)
	case compose.ComponentOfToolsNode:
		c.emitToolStart(input)
	}
}

func (c *signalCallback) onEnd(info *callbacks.RunInfo, output callbacks.CallbackOutput) {
	if info == nil {
		return
	}
	switch info.Component {
	case components.ComponentOfChatModel:
		c.emitLLMEnd(output)
	case compose.ComponentOfToolsNode:
		c.emitToolEnd(output)
	}
}

// onEndStream 处理流式输出：ChatModel 逐帧发射 llm_token（token 级流式的
// 核心），拼接全量后补发 llm_end；ToolsNode 输出拼接后按普通结果处理。
func (c *signalCallback) onEndStream(info *callbacks.RunInfo, output *schema.StreamReader[callbacks.CallbackOutput]) {
	if info == nil {
		output.Close()
		return
	}
	if info.Component == components.ComponentOfChatModel {
		var frames []*schema.Message
		for {
			frame, err := output.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				output.Close()
				return
			}
			m, ok := frame.(*schema.Message)
			if !ok || m == nil {
				continue
			}
			frames = append(frames, m)
			if m.Content != "" {
				c.emitter.Emit(projection.LLMToken, map[string]any{"delta": m.Content})
			}
		}
		if full, err := schema.ConcatMessages(frames); err == nil {
			c.emitLLMEnd(full)
		} else {
			c.emitter.Emit(projection.LLMEnd, nil)
		}
		return
	}
	c.onEnd(info, drainOutputMessages(output))
}

func (c *signalCallback) emitToolStart(input callbacks.CallbackInput) {
	m, ok := input.(*schema.Message)
	if !ok || m == nil {
		return
	}
	for _, tc := range m.ToolCalls {
		c.emitter.Emit(projection.ToolStart, map[string]any{
			"call_id":   tc.ID,
			"tool":      tc.Function.Name,
			"arguments": tc.Function.Arguments,
		})
	}
}

func (c *signalCallback) emitToolEnd(output callbacks.CallbackOutput) {
	msgs, ok := output.([]*schema.Message)
	if !ok {
		return
	}
	for _, m := range msgs {
		if m == nil {
			continue
		}
		c.emitter.Emit(projection.ToolEnd, map[string]any{
			"call_id": m.ToolCallID,
			"result":  truncateRunes(m.Content, 2000),
		})
		c.maybeEmitArtifact(m)
	}
}

// maybeEmitArtifact 在 write_artifact 成功后补发 artifact_new / artifact_updated
// 信号，让外层无需轮询存储即可感知产物变更。工具结果 JSON 契约由
// builtin.writeArtifactResult 定义，解析失败按普通工具处理，不影响主链路。
func (c *signalCallback) maybeEmitArtifact(m *schema.Message) {
	if m.ToolName != builtin.ToolWriteArtifact {
		return
	}
	var res struct {
		ArtifactID string `json:"artifact_id"`
		Type       string `json:"type"`
		Title      string `json:"title"`
		Updated    bool   `json:"updated"`
	}
	if err := json.Unmarshal([]byte(m.Content), &res); err != nil || res.ArtifactID == "" {
		return
	}
	typ := projection.ArtifactNew
	if res.Updated {
		typ = projection.ArtifactUpdated
	}
	c.emitter.Emit(typ, map[string]any{
		"artifact_id":   res.ArtifactID,
		"artifact_type": res.Type,
		"title":         res.Title,
	})
}

func (c *signalCallback) emitLLMEnd(output callbacks.CallbackOutput) {
	m, ok := output.(*schema.Message)
	if !ok || m == nil {
		c.emitter.Emit(projection.LLMEnd, nil)
		return
	}
	var payload map[string]any
	if m.ResponseMeta != nil && m.ResponseMeta.Usage != nil {
		payload = map[string]any{"usage": m.ResponseMeta.Usage}
	}
	c.emitter.Emit(projection.LLMEnd, payload)
}

// drainInputMessages 读空回调输入流并拼接为单条消息。
// 流式模式下节点间消息以帧传输，回调拿到的是副本，必须读到 EOF。
func drainInputMessages(sr *schema.StreamReader[callbacks.CallbackInput]) callbacks.CallbackInput {
	defer sr.Close()
	var frames []*schema.Message
	for {
		frame, err := sr.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil
		}
		if m, ok := frame.(*schema.Message); ok && m != nil {
			frames = append(frames, m)
		}
	}
	if len(frames) == 0 {
		return nil
	}
	out, err := schema.ConcatMessages(frames)
	if err != nil {
		return frames[len(frames)-1]
	}
	return out
}

// drainOutputMessages 读空回调输出流并还原节点输出。
// ToolsNode 输出为 []*schema.Message（可能单帧整体到达），
// ChatModel 输出为 *schema.Message 增量帧，统一按帧类型归并。
func drainOutputMessages(sr *schema.StreamReader[callbacks.CallbackOutput]) callbacks.CallbackOutput {
	defer sr.Close()
	var list []*schema.Message
	var frames []*schema.Message
	isList := false
	for {
		frame, err := sr.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil
		}
		switch v := frame.(type) {
		case []*schema.Message:
			isList = true
			list = append(list, v...)
		case *schema.Message:
			if v != nil {
				frames = append(frames, v)
			}
		}
	}
	if isList {
		return list
	}
	if len(frames) == 0 {
		return nil
	}
	out, err := schema.ConcatMessages(frames)
	if err != nil {
		return frames[len(frames)-1]
	}
	return out
}

// truncateRunes 截断工具结果：信号是观测摘要而非数据通道，
// 完整结果已在 RawHistory，超长结果原样推流只会拖垮 SSE 带宽。
func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}
