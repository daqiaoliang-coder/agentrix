package tool

import "github.com/cloudwego/eino/components/tool"

type Registry struct {
	// 工具应该遵循最佳实践：语义单一、参数扁平化、优先批量、计算在工具侧、支持部分失败。
	tools map[string]tool.BaseTool

	// order 记录注册顺序，是 List() 的唯一遍历依据。
	// 直接遍历 tools map 会得到每次随机的顺序，而 List() 的产物会经场景装配
	// 绑定给模型——工具 schema 的排列顺序属于 prompt 前缀的一部分，
	// 顺序一变前缀失配，provider 侧的 KV 缓存作废。
	order []string
}

func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]tool.BaseTool)}
}

// Register 登记工具。同名重复登记时覆盖实例，但不改变其原有顺序。
func (r *Registry) Register(name string, t tool.BaseTool) {
	if name == "" || t == nil {
		return
	}
	if _, exists := r.tools[name]; !exists {
		r.order = append(r.order, name)
	}
	r.tools[name] = t
}

// List 按注册顺序返回全部工具，保证跨轮装配产物一致。
func (r *Registry) List() []tool.BaseTool {
	list := make([]tool.BaseTool, 0, len(r.order))
	for _, name := range r.order {
		if t, ok := r.tools[name]; ok {
			list = append(list, t)
		}
	}
	return list
}
