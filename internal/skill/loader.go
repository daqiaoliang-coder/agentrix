package skill

import (
	"fmt"

	"github.com/cloudwego/eino/schema"
)

// Skill 遵循渐进式披露原则：YAML frontmatter（name + description，始终在上下文）→ SKILL.md 正文（触发时加载）→ references/（按需读取）。
// Skill 由模型自主判断调用，不是用户手动触发。
type Skill struct {
	Name        string
	Description string            // 始终在上下文中，用于模型路由
	Body        string            // SKILL.md 正文，触发时加载
	References  map[string]string // 按需读取
}

type Loader struct {
	// 渐进式 Skill 加载：初始只加载轻量入口和核心 Skill，模型根据语义自主路由加载子 Skill
	skills map[string]*Skill
}

func NewLoader() *Loader {
	return &Loader{skills: make(map[string]*Skill)}
}

func (l *Loader) Register(s *Skill) {
	l.skills[s.Name] = s
}

// Frontmatter 返回轻量入口（name + description），始终在上下文中
func (l *Loader) Frontmatter() []*schema.Message {
	msgs := make([]*schema.Message, 0, len(l.skills))
	for _, s := range l.skills {
		msgs = append(msgs, schema.SystemMessage(
			fmt.Sprintf("可用技能: %s - %s", s.Name, s.Description),
		))
	}
	return msgs
}

// LoadBody 按需加载 Skill 正文
func (l *Loader) LoadBody(name string) (string, error) {
	s, ok := l.skills[name]
	if !ok {
		return "", fmt.Errorf("skill %s not found", name)
	}
	return s.Body, nil
}
