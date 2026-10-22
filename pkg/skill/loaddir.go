package skill

import (
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// SKILLFileName 是每个技能目录的入口文档。
const SKILLFileName = "SKILL.md"

// LoadDir 从目录加载技能集合，目录约定：
//
//	<dir>/<skill-name>/SKILL.md          入口文档（YAML frontmatter + 正文）
//	<dir>/<skill-name>/references/*.md   按需读取的细则文档
//
// frontmatter 至少提供 name 与 description；name 缺省时取目录名。
// 不含 SKILL.md 的子目录被忽略，便于目录中放置其他资源。
// 返回的 Loader 按目录字典序注册，保证系统提示前缀跨轮稳定。
func LoadDir(dir string) (*Loader, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	loader := NewLoader()
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		root := filepath.Join(dir, entry.Name())
		raw, err := os.ReadFile(filepath.Join(root, SKILLFileName))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		meta, body := splitFrontmatter(string(raw))
		name := strings.TrimSpace(meta.Name)
		if name == "" {
			name = entry.Name()
		}
		refs, err := loadReferences(filepath.Join(root, "references"))
		if err != nil {
			return nil, err
		}
		loader.Register(&Skill{
			Name:        name,
			Description: strings.TrimSpace(meta.Description),
			Body:        body,
			References:  refs,
		})
	}
	return loader, nil
}

// frontmatter 是 SKILL.md 顶部的元数据段。
type frontmatter struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
}

// splitFrontmatter 解析 `---` 包裹的 YAML 头。格式非法或缺失时
// 视为无 frontmatter，整篇作为正文（description 为空由调用方决定是否报错）。
func splitFrontmatter(content string) (frontmatter, string) {
	var meta frontmatter
	content = strings.TrimPrefix(content, "\ufeff")
	if !strings.HasPrefix(content, "---") {
		return meta, content
	}
	rest := strings.TrimPrefix(content[3:], "\r\n")
	rest = strings.TrimPrefix(rest, "\n")
	idx := strings.Index(rest, "\n---")
	if idx < 0 {
		return meta, content
	}
	header := rest[:idx]
	tail := strings.TrimPrefix(rest[idx+len("\n---"):], "\r")
	tail = strings.TrimPrefix(tail, "\n")
	_ = yaml.Unmarshal([]byte(header), &meta)
	return meta, tail
}

// loadReferences 读取 references 目录下的 .md 文件，key 为去扩展名的文件名。
// 目录不存在时返回空 map（references 可选）。
func loadReferences(dir string) (map[string]string, error) {
	refs := make(map[string]string)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return refs, nil
		}
		return nil, err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		refs[strings.TrimSuffix(entry.Name(), ".md")] = string(raw)
	}
	return refs, nil
}
