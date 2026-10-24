package scene

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/cloudwego/eino/components/model"
	einotool "github.com/cloudwego/eino/components/tool"
	"gopkg.in/yaml.v3"

	ctxengine "github.com/daqiaoliang-coder/agentrix/pkg/context"
	"github.com/daqiaoliang-coder/agentrix/pkg/projection"
	"github.com/daqiaoliang-coder/agentrix/pkg/skill"
	agenttool "github.com/daqiaoliang-coder/agentrix/pkg/tool"
)

// ModelFactory 按场景声明构造模型实例。凭据解析、provider 路由由调用方负责。
type ModelFactory func(ctx context.Context, spec ModelSpec) (model.ToolCallingChatModel, error)

// LoaderDeps 是场景加载所需的运行时依赖。
// Tools/Catalog 由宿主进程统一注册，场景只通过白名单引用。
type LoaderDeps struct {
	ModelFactory  ModelFactory
	Tools         []einotool.BaseTool
	Catalog       *agenttool.Catalog
	ArtifactStore projection.ArtifactStore
	SpillStore    ctxengine.SpillStore
}

// Loader 把 YAML 场景声明装配为可注册的 SceneConfig。
type Loader struct {
	deps LoaderDeps
}

func NewLoader(deps LoaderDeps) *Loader {
	return &Loader{deps: deps}
}

// LoadDir 加载目录下全部 *.yaml / *.yml 场景，按文件名排序保证注册顺序稳定。
func (l *Loader) LoadDir(ctx context.Context, dir string) ([]*SceneConfig, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read scenes dir %s: %w", dir, err)
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		switch strings.ToLower(filepath.Ext(e.Name())) {
		case ".yaml", ".yml":
			files = append(files, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(files)

	out := make([]*SceneConfig, 0, len(files))
	// 场景 key 是路由与 artifact/会话归属的稳定标识，重复注册会静默覆盖，
	// 必须在启动期拒绝。
	origin := make(map[string]string, len(files))
	for _, f := range files {
		cfg, err := l.LoadFile(ctx, f)
		if err != nil {
			return nil, err
		}
		if prev, dup := origin[cfg.Key]; dup {
			return nil, fmt.Errorf("duplicate scene key %q in %s and %s", cfg.Key, prev, f)
		}
		origin[cfg.Key] = f
		out = append(out, cfg)
	}
	return out, nil
}

// LoadFile 加载单个场景文件。相对路径（prompt / skills）以场景文件所在目录为基准。
func (l *Loader) LoadFile(ctx context.Context, path string) (*SceneConfig, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read scene %s: %w", path, err)
	}
	var spec SceneSpec
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		return nil, fmt.Errorf("parse scene %s: %w", path, err)
	}
	if err := validateSpec(&spec); err != nil {
		return nil, fmt.Errorf("scene %s: %w", path, err)
	}
	if l.deps.ModelFactory == nil {
		return nil, fmt.Errorf("scene %s: ModelFactory is required", path)
	}

	base := filepath.Dir(path)
	prompt, err := os.ReadFile(resolvePath(base, spec.SystemPromptFile))
	if err != nil {
		return nil, fmt.Errorf("scene %s: read system prompt: %w", path, err)
	}

	chatModel, err := l.deps.ModelFactory(ctx, spec.Model)
	if err != nil {
		return nil, fmt.Errorf("scene %s: build model %q: %w", path, spec.Model.Name, err)
	}

	var skills *skill.Loader
	if spec.SkillsDir != "" {
		skills, err = skill.LoadDir(resolvePath(base, spec.SkillsDir))
		if err != nil {
			return nil, fmt.Errorf("scene %s: load skills: %w", path, err)
		}
	}

	totalTimeout, err := parseDuration(spec.TotalTimeout)
	if err != nil {
		return nil, fmt.Errorf("scene %s: total_timeout: %w", path, err)
	}
	modelTimeout, err := parseDuration(spec.ModelCallTimeout)
	if err != nil {
		return nil, fmt.Errorf("scene %s: model_call_timeout: %w", path, err)
	}
	toolTimeout, err := parseDuration(spec.ToolCallTimeout)
	if err != nil {
		return nil, fmt.Errorf("scene %s: tool_call_timeout: %w", path, err)
	}
	// 未声明时默认 60s：单个工具卡住不应拖垮整个 Turn。
	// 显式写 "0s" 表示不限制。
	if strings.TrimSpace(spec.ToolCallTimeout) == "" {
		toolTimeout = 60 * time.Second
	}

	// 参数级策略在启动期校验：操作符/必填项非法或指向未注册工具时拒绝启动，
	// 避免带病配置在每次请求装配时才失败。
	policies, err := agenttool.NormalizeParamRules(spec.ParamPolicies)
	if err != nil {
		return nil, fmt.Errorf("scene %s: %w", path, err)
	}
	if err := validatePolicyTools(ctx, policies, l.deps.Tools); err != nil {
		return nil, fmt.Errorf("scene %s: %w", path, err)
	}

	cfg := &SceneConfig{
		Key:                spec.Key,
		Name:               spec.Name,
		SystemPrompt:       string(prompt),
		Model:              chatModel,
		Tools:              l.deps.Tools,
		Skills:             skills,
		AllowedCommands:    spec.AllowedCommands,
		ParamPolicies:      policies,
		Catalog:            l.deps.Catalog,
		MaxIterations:      spec.MaxIterations,
		ModelContextWindow: spec.ModelContextWindow,
		TokenBudget:        spec.TokenBudget,
		CompressThreshold:  spec.CompressThreshold,
		NoProgressLimit:    spec.NoProgressLimit,
		TotalTimeout:       totalTimeout,
		ModelCallTimeout:   modelTimeout,
		ToolCallTimeout:    toolTimeout,
		ArtifactStore:      l.deps.ArtifactStore,
		SpillStore:         l.deps.SpillStore,
	}
	if cfg.Name == "" {
		cfg.Name = cfg.Key
	}
	if cfg.MaxIterations <= 0 {
		cfg.MaxIterations = 10
	}
	if len(spec.AllowedCommands) > 0 && l.deps.Catalog == nil {
		return nil, fmt.Errorf("scene %s: allowed_commands requires a Catalog", path)
	}
	return cfg, nil
}

func validateSpec(spec *SceneSpec) error {
	if strings.TrimSpace(spec.Key) == "" {
		return fmt.Errorf("key is required")
	}
	if strings.TrimSpace(spec.SystemPromptFile) == "" {
		return fmt.Errorf("system_prompt_file is required")
	}
	if spec.CompressThreshold < 0 || spec.CompressThreshold >= 1 {
		return fmt.Errorf("compress_threshold must be in (0,1), got %v", spec.CompressThreshold)
	}
	return nil
}

// validatePolicyTools 确认参数策略只指向宿主已注册的业务工具。
// 策略指向不存在的工具意味着配置错误——运行时才发现会让每个 Turn 失败。
func validatePolicyTools(ctx context.Context, policies []agenttool.ParamRule, tools []einotool.BaseTool) error {
	if len(policies) == 0 {
		return nil
	}
	names := make(map[string]bool, len(tools))
	for _, t := range tools {
		if t == nil {
			continue
		}
		info, err := t.Info(ctx)
		if err != nil {
			return fmt.Errorf("param_policies: read tool info: %w", err)
		}
		names[info.Name] = true
	}
	for _, r := range policies {
		if !names[r.Tool] {
			return fmt.Errorf("param_policies: tool %q is not registered", r.Tool)
		}
	}
	return nil
}

func resolvePath(base, p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(base, p)
}

func parseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, err
	}
	if d < 0 {
		return 0, fmt.Errorf("must not be negative")
	}
	return d, nil
}
