package context

// PromptOverheadSnapshot 是模型每次调用都要付、但不在 messages 列表里的固定开销快照。
// System Prompt 已作为首条消息参与 EstimateMessagesTokens，不能在这里重复计入。
type PromptOverheadSnapshot struct {
	// ToolsTokens 是当前活跃工具 schema 序列化后的 token 数。
	ToolsTokens int
	// Multimodal 是为图片等固定预留的 token 数（如 1024/part）。
	// 消息级多模态开销另在 estimateMessageTokens 内计算，此字段用于装配期即可确定的预留量。
	Multimodal int
}

// Total 返回快照的总固定开销。
func (s PromptOverheadSnapshot) Total() int {
	return s.ToolsTokens + s.Multimodal
}
