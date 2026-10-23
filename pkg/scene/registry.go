package scene

import (
	"sort"
	"sync"

	"github.com/daqiaoliang-coder/agentrix/pkg/projection"
)

// Registry 保存进程内已注册的场景。
//
// 场景的 ArtifactStore 在处理该场景配置时确定：注册时若未显式配置，
// Registry 会为其分配一个进程内 MemoryArtifactStore（按场景隔离）。
// 这样 artifact 的归属与生命周期由场景配置本身承载，不再依赖包级全局表。
type Registry struct {
	mu     sync.RWMutex
	scenes map[string]*SceneConfig
}

func NewRegistry() *Registry {
	return &Registry{scenes: make(map[string]*SceneConfig)}
}

// Register 注册场景。cfg.ArtifactStore 为 nil 时分配进程内默认存储，
// 保证经 Registry 运行的场景跨轮 artifact 可累积、可被 API 层枚举。
func (r *Registry) Register(cfg *SceneConfig) {
	if cfg == nil {
		return
	}
	if cfg.ArtifactStore == nil {
		cfg.ArtifactStore = projection.NewMemoryArtifactStore()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.scenes[cfg.Key] = cfg
}

func (r *Registry) Get(key string) (*SceneConfig, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	cfg, ok := r.scenes[key]
	return cfg, ok
}

// List 返回全部已注册场景，按 Key 排序保证枚举结果稳定。
func (r *Registry) List() []*SceneConfig {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*SceneConfig, 0, len(r.scenes))
	for _, cfg := range r.scenes {
		out = append(out, cfg)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}
