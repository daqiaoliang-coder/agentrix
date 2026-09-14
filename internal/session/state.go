package session

import "github.com/cloudwego/eino/schema"

type State struct {
	MemorySummary string         `json:"memory_summary"`
	HistoryCursor int            `json:"history_cursor"`
	SkillRuntime  map[string]any `json:"skill_runtime"`
	Todo          []string       `json:"todo"`
	Reminder      []string       `json:"reminder"`
}

func (s *State) UpdateFromTurn(messages []*schema.Message, _ *schema.Message) {
	if s.SkillRuntime == nil {
		s.SkillRuntime = make(map[string]any)
	}
	s.HistoryCursor = len(messages)
}
