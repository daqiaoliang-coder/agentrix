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

	// order 记录技能的注册顺序，是 Frontmatter() 的唯一遍历依据。
	//
	// 为什么不能直接遍历 skills map：Go 的 map 迭代顺序每次随机，而 Frontmatter()
	// 的产物会被拼进 system prompt 的最前段。Agent 每次请求重建、每次都重新装配，
	// 于是每一轮请求拼出的技能索引顺序都不同——system prompt 一变，整个 prompt
	// 前缀失配，provider 侧的 KV 缓存全部作废，每轮都按全价重算全部输入。
	//
	// 用注册顺序而非字典序，是为了让接入方能主动把高频技能排在前面。
	order []string
}

func NewLoader() *Loader {
	return &Loader{skills: make(map[string]*Skill)}
}

// Register 登记一个技能。同名重复登记时覆盖内容，但不改变其原有顺序。
func (l *Loader) Register(s *Skill) {
	if s == nil || s.Name == "" {
		return
	}
	if _, exists := l.skills[s.Name]; !exists {
		l.order = append(l.order, s.Name)
	}
	l.skills[s.Name] = s
}

// Frontmatter 返回轻量入口（name + description），始终在上下文中。
// 对应渐进式加载阶段 0：只暴露"有哪些技能、各自做什么"，不含正文与细则。
//
// 输出顺序恒等于注册顺序，与 map 迭代顺序无关——这是 system prompt 前缀
// 跨轮稳定的前提，见 order 字段注释。
func (l *Loader) Frontmatter() []*schema.Message {
	msgs := make([]*schema.Message, 0, len(l.order))
	for _, name := range l.order {
		s, ok := l.skills[name]
		if !ok || s == nil {
			continue
		}
		msgs = append(msgs, schema.SystemMessage(
			fmt.Sprintf("- %s：%s", s.Name, s.Description),
		))
	}
	return msgs
}

// LoadBody 按需加载 Skill 正文（阶段 1：模型选中 Skill 后读取入口文档）
func (l *Loader) LoadBody(name string) (string, error) {
	s, ok := l.skills[name]
	if !ok {
		return "", fmt.Errorf("skill %s not found", name)
	}
	return s.Body, nil
}

// LoadReference 按需加载某个 Skill 下指定的 reference 文档（阶段 2：模型据操作类型读取完整命令契约）
func (l *Loader) LoadReference(name, ref string) (string, error) {
	s, ok := l.skills[name]
	if !ok {
		return "", fmt.Errorf("skill %s not found", name)
	}
	body, ok := s.References[ref]
	if !ok {
		return "", fmt.Errorf("reference %s not found in skill %s", ref, name)
	}
	return body, nil
}

// Names 返回所有已注册 Skill 名称，供读取工具在名称错误时给出可用清单
func (l *Loader) Names() []string {
	names := make([]string, 0, len(l.skills))
	for n := range l.skills {
		names = append(names, n)
	}
	return names
}

// ReferenceNames 返回某个 Skill 下的全部 reference 名称
func (l *Loader) ReferenceNames(name string) ([]string, error) {
	s, ok := l.skills[name]
	if !ok {
		return nil, fmt.Errorf("skill %s not found", name)
	}
	refs := make([]string, 0, len(s.References))
	for r := range s.References {
		refs = append(refs, r)
	}
	return refs, nil
}
