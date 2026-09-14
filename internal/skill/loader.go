package skill

import (
	"fmt"

	"github.com/cloudwego/eino/schema"
)

type Skill struct {
	Name        string
	Description string
	Body        string
	References  map[string]string
}

type Loader struct {
	skills map[string]*Skill
}

func NewLoader() *Loader {
	return &Loader{skills: make(map[string]*Skill)}
}

func (l *Loader) Register(s *Skill) {
	l.skills[s.Name] = s
}

func (l *Loader) Frontmatter() []*schema.Message {
	msgs := make([]*schema.Message, 0, len(l.skills))
	for _, s := range l.skills {
		msgs = append(msgs, schema.SystemMessage(
			fmt.Sprintf("可用技能: %s - %s", s.Name, s.Description),
		))
	}
	return msgs
}

func (l *Loader) LoadBody(name string) (string, error) {
	s, ok := l.skills[name]
	if !ok {
		return "", fmt.Errorf("skill %s not found", name)
	}
	return s.Body, nil
}
