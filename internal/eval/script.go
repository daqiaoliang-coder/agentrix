// Package eval 提供离线评测设施：用脚本化模型（不依赖真实模型 API）
// 确定性回放 Agent 场景，对最终输出、工具调用、审批中断与过程信号做
// 声明式断言，并产出结构化报告。
//
// 典型用法见 example_test.go：定义 Case（输入 + 模型脚本 + 期望），
// Runner 跑完整 Run/Resume 链路，SuiteReport.Assert 汇总成败。
package eval

import (
	"context"
	"sync"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// ScriptModel 按预设脚本依次返回模型输出，用于离线确定性回放。
//
// 状态必须走指针共享：eino 绑定工具时会调用 WithTools 克隆模型
// （BudgetModel 装饰链路上可能多次克隆），值语义的计数器会被克隆
// 归零，导致多轮脚本从头重放。
type ScriptModel struct {
	st *scriptState
}

type scriptState struct {
	mu      sync.Mutex
	call    int
	replies []*schema.Message
	// bound 为最近一次 WithTools 绑定的工具名；seenTools 在每次
	// Generate/Stream 时快照 bound，与模型实际调用一一对应。
	bound     []string
	seenTools [][]string
}

// NewScriptModel 按调用顺序构造脚本。
func NewScriptModel(replies ...*schema.Message) *ScriptModel {
	cp := make([]*schema.Message, len(replies))
	copy(cp, replies)
	return &ScriptModel{st: &scriptState{replies: cp}}
}

// ToolCall 构造一条工具调用消息的便捷助手（等价于手写 schema.ToolCall）。
func ToolCall(name, argsJSON string) *schema.Message {
	return &schema.Message{
		Role: schema.Assistant,
		ToolCalls: []schema.ToolCall{{
			ID:       "call_" + name,
			Function: schema.FunctionCall{Name: name, Arguments: argsJSON},
		}},
	}
}

// record 快照本轮绑定工具并返回下一条脚本消息；脚本耗尽后返回无
// ToolCall 的收尾消息，避免图在 MaxIterations 内空转。
func (m *ScriptModel) record() *schema.Message {
	m.st.mu.Lock()
	defer m.st.mu.Unlock()
	m.st.seenTools = append(m.st.seenTools, append([]string(nil), m.st.bound...))
	if m.st.call >= len(m.st.replies) {
		return schema.AssistantMessage("（脚本已耗尽）", nil)
	}
	out := m.st.replies[m.st.call]
	m.st.call++
	return out
}

// Generate 返回下一条脚本消息。
func (m *ScriptModel) Generate(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	return m.record(), nil
}

// Stream 模拟真实模型的流式产出：正文按 4 字符切帧，ToolCalls 置于末帧。
// 帧式产出让评测同时覆盖 llm_token 信号与遥测流式时机。
func (m *ScriptModel) Stream(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return schema.StreamReaderFromArray(chunkFrames(m.record(), 4)), nil
}

// WithTools 克隆模型但共享同一状态指针——克隆不影响轮次计数，
// bound 工具记录在共享状态上供下一次 Generate 快照。
func (m *ScriptModel) WithTools(tools []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	clone := &ScriptModel{st: m.st}
	names := make([]string, 0, len(tools))
	for _, info := range tools {
		names = append(names, info.Name)
	}
	m.st.mu.Lock()
	m.st.bound = names
	m.st.mu.Unlock()
	return clone, nil
}

// SeenTools 返回每次模型调用实际绑定的工具名快照（按调用先后）。
// 白名单评测据此断言「模型根本看不到被过滤的工具」。
func (m *ScriptModel) SeenTools() [][]string {
	m.st.mu.Lock()
	defer m.st.mu.Unlock()
	out := make([][]string, len(m.st.seenTools))
	for i, names := range m.st.seenTools {
		out[i] = append([]string(nil), names...)
	}
	return out
}

// chunkFrames 把完整消息切成流式帧：正文按 n 字符切分，工具调用并入末帧。
func chunkFrames(msg *schema.Message, n int) []*schema.Message {
	if msg == nil {
		return nil
	}
	content := []rune(msg.Content)
	var frames []*schema.Message
	for i := 0; i < len(content); i += n {
		end := i + n
		if end > len(content) {
			end = len(content)
		}
		frames = append(frames, schema.AssistantMessage(string(content[i:end]), nil))
	}
	if len(frames) == 0 {
		frames = append(frames, schema.AssistantMessage("", nil))
	}
	if len(msg.ToolCalls) > 0 {
		frames[len(frames)-1].ToolCalls = msg.ToolCalls
	}
	return frames
}
