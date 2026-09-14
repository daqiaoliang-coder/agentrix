package tool

import "github.com/cloudwego/eino/components/tool"

type Registry struct {
	// 工具应该遵循最佳实践：语义单一、参数扁平化、优先批量、计算在工具侧、支持部分失败。
	tools map[string]tool.BaseTool
}

func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]tool.BaseTool)}
}

func (r *Registry) Register(name string, t tool.BaseTool) {
	r.tools[name] = t
}

func (r *Registry) List() []tool.BaseTool {
	list := make([]tool.BaseTool, 0, len(r.tools))
	for _, t := range r.tools {
		list = append(list, t)
	}
	return list
}
