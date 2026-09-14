package scene

import "sync"

type Registry struct {
	mu     sync.RWMutex
	scenes map[string]*SceneConfig
}

func NewRegistry() *Registry {
	return &Registry{scenes: make(map[string]*SceneConfig)}
}

func (r *Registry) Register(cfg *SceneConfig) {
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
