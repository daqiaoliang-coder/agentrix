package context

// PromptOverheadSnapshot 是「模型每次调用都要付、但不在 messages 列表里」的固定开销快照。
//
// 为什么需要它：
// shouldCompress 原先只用 estimateTokens(messages) 判断是否超窗，而 messages 里
// 并不包含两块真实开销——
//
//  1. System Prompt：由 Agent 装配后作为首条消息注入，压缩后会被摘要替换重建，
//     其体积（含注入的技能索引）是固定成本；
//  2. 活跃工具的 JSON Schema：eino 在 ReAct 循环内通过 WithTools 绑定给模型，
//     完全不出现在 messages 里，但对模型而言是实打实的输入 token。
//
// 漏算这两块的后果是阈值失真：配置 TokenBudget=8192、阈值 80% 时，
// 实际可用给对话历史的额度远小于 6553，压缩触发得过晚甚至永不触发。
//
// 分层约定（与 meego-ai 设计一致）：
// engine 只消费快照，不反向依赖工具注册表；快照由上层（harness/core 的装配阶段）
// 按真实 active toolset 计算一次后注入。这样 context 包保持零 SDK 副作用。
type PromptOverheadSnapshot struct {
	// SystemTokens 是系统提示（含注入的技能索引）的 token 数。
	SystemTokens int
	// ToolsTokens 是当前活跃工具 schema 序列化后的 token 数。
	ToolsTokens int
	// Multimodal 是为图片等固定预留的 token 数（如 1024/part）。
	// 消息级多模态开销另在 estimateMessageTokens 内计算，此字段用于装配期即可确定的预留量。
	Multimodal int
}

// Total 返回快照的总固定开销。
func (s PromptOverheadSnapshot) Total() int {
	return s.SystemTokens + s.ToolsTokens + s.Multimodal
}
