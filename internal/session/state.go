package session

import "github.com/cloudwego/eino/schema"

type State struct {
	MemorySummary string         `json:"memory_summary"` // 跨轮记忆摘要
	HistoryCursor int            `json:"history_cursor"` // 历史游标：已消费到哪条消息
	SkillRuntime  map[string]any `json:"skill_runtime"`  // Skill 运行时状态
	Todo          []string       `json:"todo"`           // 待办事项
	Reminder      []string       `json:"reminder"`       // 提醒
}

func (s *State) UpdateFromTurn(messages []*schema.Message, _ *schema.Message) {
	if s.SkillRuntime == nil {
		s.SkillRuntime = make(map[string]any)
	}
	// 更新游标：指向已处理的消息位置
	s.HistoryCursor = len(messages)
}
