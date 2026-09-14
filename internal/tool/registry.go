package tool

import "github.com/cloudwego/eino/components/tool"

type Registry struct {
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
