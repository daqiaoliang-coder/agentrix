package scene

import agenttool "github.com/daqiaoliang-coder/agentrix/pkg/tool"

// SceneSpec 是场景的 YAML 声明。与 SceneConfig 的区别：
// Spec 只描述可序列化的配置，运行时依赖（模型实例、工具、存储）由
// Loader 注入后装配为 SceneConfig。
type SceneSpec struct {
	Key              string    `yaml:"key"`
	Name             string    `yaml:"name"`
	SystemPromptFile string    `yaml:"system_prompt_file"`
	SkillsDir        string    `yaml:"skills_dir"`
	Model            ModelSpec `yaml:"model"`

	// AllowedCommands 限定本场景可用的命令白名单；空表示全部可见。
	// 需配合 LoaderDeps.Catalog 使用。
	AllowedCommands []string `yaml:"allowed_commands"`

	// ParamPolicies 是参数级硬约束：命中即拒绝执行，不进入人工审批。
	// 例如 ratio > 20 应在任何审批之前直接拒绝。
	ParamPolicies []agenttool.ParamRule `yaml:"param_policies"`

	MaxIterations      int     `yaml:"max_iterations"`
	ModelContextWindow int     `yaml:"model_context_window"`
	TokenBudget        int     `yaml:"token_budget"`
	CompressThreshold  float64 `yaml:"compress_threshold"`
	NoProgressLimit    int     `yaml:"no_progress_limit"`

	// 超时统一用 Go duration 字面量（如 "60s"、"10m"）。
	TotalTimeout     string `yaml:"total_timeout"`
	ModelCallTimeout string `yaml:"model_call_timeout"`
	ToolCallTimeout  string `yaml:"tool_call_timeout"`
}

// ModelSpec 指定场景使用的模型。
// Provider 决定默认的凭据来源（由 ModelFactory 解释），Name 为模型名/接入点 ID。
type ModelSpec struct {
	Provider  string `yaml:"provider"`
	Name      string `yaml:"name"`
	BaseURL   string `yaml:"base_url"`
	APIKeyEnv string `yaml:"api_key_env"`
}
