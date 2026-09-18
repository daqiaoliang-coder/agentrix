package session

import (
	"time"

	"github.com/cloudwego/eino/schema"
)

// RawHistoryRecord 是 RawHistory 的存储单元，对应 ai_raw_history 表的一行。
//
// RawHistory 是完整、append-only 的审计记录（"实际发生了什么"），与 SessionStateV1
// 的工作记忆（"模型需要记住什么"）分离。记录一旦写入不可修改、不可物理删除；
// 历史范围读取统一走 Store.LoadHistoryFrom 的游标语义。
//
// 字段与 ai_raw_history 表结构一一对应；内存实现按同一结构存放，
// 未来落库（MySQL 等）时可直接序列化映射为行。
type RawHistoryRecord struct {
	ID         int64            `json:"id"`                     // 自增主键（全局有序，用于范围查询和游标）
	SessionID  string           `json:"session_id"`             // 会话 ID（分区键）
	Seq        int64            `json:"seq"`                    // 会话内序号（从 0 递增，保证顺序）
	Role       string           `json:"role"`                   // user / assistant / tool / system
	Content    string           `json:"content"`                // 消息正文
	ToolCalls  []ToolCallRecord `json:"tool_calls,omitempty"`   // assistant 消息携带的工具调用列表
	ToolCallID string           `json:"tool_call_id,omitempty"` // tool 消息携带的配对 ID（与 tool_calls[].ID 对应）
	CreateTime time.Time        `json:"create_time"`            // 写入时间戳
	Extra      map[string]any   `json:"extra,omitempty"`        // 扩展字段（如 token 用量、模型信息等元数据）
}

// ToolCallRecord 是 assistant 消息中单个工具调用的快照。
type ToolCallRecord struct {
	ID        string `json:"id"`        // 与 tool 消息的 ToolCallID 配对
	Name      string `json:"name"`      // 工具名
	Arguments string `json:"arguments"` // 调用参数（JSON）
}

// newRecord 将 schema.Message 映射为一条审计记录。
// 补齐存储侧元数据（id/seq/session_id/create_time），并从 ResponseMeta
// 提取 token 用量等模型信息写入 extra。
//
// 注意：多模态字段（MultiContent 等）不参与映射，当前框架全为文本消息；
// 落库实现如需完整保真，可扩展 Content 为整条消息的 JSON 序列化。
func newRecord(id, seq int64, sessionID string, msg *schema.Message, now time.Time) RawHistoryRecord {
	rec := RawHistoryRecord{
		ID:         id,
		SessionID:  sessionID,
		Seq:        seq,
		Role:       string(msg.Role),
		Content:    msg.Content,
		CreateTime: now,
	}
	if msg.Role == schema.Tool {
		rec.ToolCallID = msg.ToolCallID
	}
	for _, tc := range msg.ToolCalls {
		rec.ToolCalls = append(rec.ToolCalls, ToolCallRecord{
			ID:        tc.ID,
			Name:      tc.Function.Name,
			Arguments: tc.Function.Arguments,
		})
	}
	rec.Extra = extractExtra(msg)
	return rec
}

// extractExtra 汇总消息元数据：工具名（tool 消息）、finish_reason、token 用量。
func extractExtra(msg *schema.Message) map[string]any {
	var extra map[string]any
	set := func(k string, v any) {
		if extra == nil {
			extra = make(map[string]any)
		}
		extra[k] = v
	}
	if msg.ToolName != "" {
		set("tool_name", msg.ToolName)
	}
	if msg.ResponseMeta != nil {
		if msg.ResponseMeta.FinishReason != "" {
			set("finish_reason", msg.ResponseMeta.FinishReason)
		}
		if u := msg.ResponseMeta.Usage; u != nil {
			set("prompt_tokens", u.PromptTokens)
			set("completion_tokens", u.CompletionTokens)
			set("total_tokens", u.TotalTokens)
		}
	}
	return extra
}

// recordToMessage 将审计记录还原为模型消息。
func recordToMessage(rec RawHistoryRecord) *schema.Message {
	m := &schema.Message{
		Role:    schema.RoleType(rec.Role),
		Content: rec.Content,
	}
	if rec.ToolCallID != "" {
		m.ToolCallID = rec.ToolCallID
	}
	for _, tc := range rec.ToolCalls {
		m.ToolCalls = append(m.ToolCalls, schema.ToolCall{
			ID:       tc.ID,
			Function: schema.FunctionCall{Name: tc.Name, Arguments: tc.Arguments},
		})
	}
	return m
}

// messagesToRecords 批量将消息映射为待写入的记录（未分配 id/seq）。
func messagesToRecords(sessionID string, msgs []*schema.Message, startSeq int64, now time.Time) []RawHistoryRecord {
	records := make([]RawHistoryRecord, 0, len(msgs))
	seq := startSeq
	for _, m := range msgs {
		if m == nil {
			continue
		}
		records = append(records, newRecord(0, seq, sessionID, m, now))
		seq++
	}
	return records
}

// recordsToMessages 将记录批量还原为消息（每次重建新对象，避免内部 slice 泄漏）。
func recordsToMessages(records []RawHistoryRecord) []*schema.Message {
	msgs := make([]*schema.Message, 0, len(records))
	for _, rec := range records {
		msgs = append(msgs, recordToMessage(rec))
	}
	return msgs
}
