package session

import (
	"time"
)

// SchemaVersionV1 是当前 SessionState 的 schema 版本号。
// 写入 State.Version，未来做版本迁移时据此判断来源结构。
const SchemaVersionV1 = "1"

// SessionStateV1 是 Agent 的工作记忆——轻量、结构化、可被模型高效消费。
//
// 它与 RawHistory 通过 HistoryCursor 关联：
//   - RawHistory 是完整的、不可篡改的审计记录（"实际发生了什么"）；
//   - SessionStateV1 是模型需要记住的精简摘要（"模型需要记住什么"）。
//
// 关键约束：SessionStateV1 不等于业务执行账本，也不等于任意失败恢复点。
// 业务状态应由业务方通过 Tool 维护，这里只放跨轮工作记忆。
//
// Version 字段用于未来 schema 迁移（SessionStateV2 等）。
type SessionStateV1 struct {
	Version       string            `json:"version"`        // schema 版本号（用于版本迁移）
	SessionID     string            `json:"session_id"`     // 会话 ID
	HistoryCursor int               `json:"history_cursor"` // RawHistory 游标（上次处理到的最大记录 seq）
	Memory        MemoryState       `json:"memory"`         // 跨轮记忆摘要
	SkillRuntimes SkillRuntimeState `json:"skill_runtimes"` // Skill 运行时状态
	Todo          TodoState         `json:"todo"`           // Todo 列表快照
	Reminder      ReminderState     `json:"reminder"`       // Reminder 状态
	HitlState     HitlState         `json:"hitl_state"`     // HITL 状态（如果有挂起的审批）
	VersionLock   int64             `json:"version_lock"`   // 版本锁定（CAS 乐观锁用）
	UpdateTime    time.Time         `json:"update_time"`    // 最后更新时间
}

// State 保留为 SessionStateV1 的别名，兼容现有调用点（Store 接口、engine、agent）。
// 新代码优先使用 SessionStateV1；待所有调用点迁移后可移除该别名。
type State = SessionStateV1

// NewState 构造带默认版本号的空状态。
func NewState(sessionID string) *SessionStateV1 {
	return &SessionStateV1{
		Version:   SchemaVersionV1,
		SessionID: sessionID,
		HitlState: HitlState{Status: HitlIdle},
	}
}

// ---------------------------------------------------------------------------
// MemoryState：跨轮记忆摘要
// ---------------------------------------------------------------------------

// MemoryState 承载跨轮压缩后的对话摘要，是「用结构化状态换 token」的核心。
// Summary 替代了被压缩掉的中间段历史，模型读一行摘要即可重建上下文。
type MemoryState struct {
	Summary         string `json:"summary"`          // 历史对话压缩摘要
	TokenBudget     int    `json:"token_budget"`     // 记忆区 token 预算
	CompressedTurns [2]int `json:"compressed_turns"` // 已压缩的轮次范围 [start, end]
}

// ---------------------------------------------------------------------------
// SkillRuntimeState：Skill 运行时状态
// ---------------------------------------------------------------------------

// SkillRuntimeState 记录技能的运行时快照。
// 核心价值是 LoadedReferences 去重：避免模型每轮重复 read_skill 同一份文档，
// 技能正文动辄数千 token，重复读一次就是实打实的浪费。
type SkillRuntimeState struct {
	ActiveSkills     []string       `json:"active_skills"`     // 当前激活的 Skill 列表
	SkillState       map[string]any `json:"skill_state"`       // 各 Skill 的自定义状态
	LoadedReferences []string       `json:"loaded_references"` // 已加载的 Skill reference 文档
}

// ---------------------------------------------------------------------------
// TodoState：Todo 列表快照
// ---------------------------------------------------------------------------

// TodoStatus 是 Todo 项/列表的完成状态。
type TodoStatus string

const (
	TodoPending    TodoStatus = "pending"
	TodoInProgress TodoStatus = "in_progress"
	TodoDone       TodoStatus = "done"
)

// TodoItem 是单条待办事项。
type TodoItem struct {
	ID      string     `json:"id"`      // 唯一标识（用于增删改）
	Content string     `json:"content"` // 事项内容
	Status  TodoStatus `json:"status"`  // 完成状态
}

// TodoState 是 Todo 列表快照，每轮装配时作为 system 消息注入上下文，
// 模型不必从长历史里重建「做到哪一步」。
type TodoState struct {
	Items  []TodoItem `json:"items"`  // 待办事项列表
	Status TodoStatus `json:"status"` // 整体完成状态
}

// ---------------------------------------------------------------------------
// ReminderState：Reminder 状态
// ---------------------------------------------------------------------------

// Reminder 是任务级约束（如「不得超过 20% 灰度」），固化在此可替代反复出现在
// 历史里的自然语言叮嘱，减少历史膨胀。
type Reminder struct {
	ID      string `json:"id"`      // 唯一标识
	Content string `json:"content"` // 约束内容
}

// ReminderState 维护激活中的 Reminder 及其注入轮次记录。
// InjectedTurns 用于避免同一轮重复注入同一条 Reminder。
type ReminderState struct {
	ActiveReminders []Reminder       `json:"active_reminders"` // 当前激活的 Reminder 列表
	InjectedTurns   map[string][]int `json:"injected_turns"`   // 各 Reminder 已注入的轮次记录（reminderID -> turn 序号）
}

// ---------------------------------------------------------------------------
// HitlState：HITL 状态
// ---------------------------------------------------------------------------

// HitlStatus 是 HITL（人工审批）状态机的状态。
type HitlStatus string

const (
	HitlIdle      HitlStatus = "IDLE"      // 无挂起审批
	HitlSuspended HitlStatus = "SUSPENDED" // 有挂起的工具调用，等待人工决策
	HitlResuming  HitlStatus = "RESUMING"  // 正在恢复执行
)

// ToolCallSnapshot 是挂起工具调用的快照，恢复时据此定位。
type ToolCallSnapshot struct {
	ToolName  string `json:"tool_name"` // 工具名
	Arguments string `json:"arguments"` // 调用参数（JSON）
	CallID    string `json:"call_id"`   // tool_call_id
}

// HitlState 记录 HITL 中断/恢复的运行时状态。
// 正常轮次为 IDLE；触发审批中断后置为 SUSPENDED 并保存挂起调用快照；
// 恢复流程中短暂为 RESUMING。
type HitlState struct {
	Status            HitlStatus        `json:"status"`                        // IDLE / SUSPENDED / RESUMING
	SuspendedToolCall *ToolCallSnapshot `json:"suspended_tool_call,omitempty"` // 挂起时的工具调用
	BudgetConsumed    int               `json:"budget_consumed"`               // 挂起时已消耗的预算
}

// ---------------------------------------------------------------------------
// 更新逻辑
// ---------------------------------------------------------------------------

// UpdateFromTurn 在每轮结束后更新工作记忆（懒初始化 map、刷新时间戳）。
//
// HistoryCursor 不在此处推进：由调用方在 AppendHistory 后用返回的最后记录
// seq 回写（游标指向 RawHistory 记录 seq，而非压缩后装配产物的长度）。
func (s *SessionStateV1) UpdateFromTurn() {
	if s.SkillRuntimes.SkillState == nil {
		s.SkillRuntimes.SkillState = make(map[string]any)
	}
	if s.Reminder.InjectedTurns == nil {
		s.Reminder.InjectedTurns = make(map[string][]int)
	}
	s.UpdateTime = time.Now()
}
