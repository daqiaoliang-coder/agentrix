package scene

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	einotool "github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

type stubModel struct{ name string }

func (m stubModel) Generate(context.Context, []*schema.Message, ...model.Option) (*schema.Message, error) {
	return schema.AssistantMessage("ok", nil), nil
}

func (m stubModel) Stream(context.Context, []*schema.Message, ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return schema.StreamReaderFromArray([]*schema.Message{schema.AssistantMessage("ok", nil)}), nil
}

func (m stubModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return m, nil
}

func TestLoaderLoadDir(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "prompts", "demo.md"), "你是演示助手。")
	write(t, filepath.Join(dir, "skills", "demo", "SKILL.md"), `---
name: demo
description: 演示技能
---
# Demo
`)
	write(t, filepath.Join(dir, "demo.yaml"), `
key: demo
name: 演示场景
system_prompt_file: prompts/demo.md
skills_dir: skills
model:
  provider: ark
  name: test-model
max_iterations: 5
compress_threshold: 0.8
total_timeout: 10m
tool_call_timeout: 30s
`)

	var gotSpec ModelSpec
	loader := NewLoader(LoaderDeps{
		ModelFactory: func(_ context.Context, spec ModelSpec) (model.ToolCallingChatModel, error) {
			gotSpec = spec
			return stubModel{name: spec.Name}, nil
		},
	})

	cfgs, err := loader.LoadDir(context.Background(), dir)
	if err != nil {
		t.Fatalf("load dir: %v", err)
	}
	if len(cfgs) != 1 {
		t.Fatalf("场景数 = %d", len(cfgs))
	}
	cfg := cfgs[0]
	if cfg.Key != "demo" || cfg.Name != "演示场景" {
		t.Errorf("基本字段不正确: %+v", cfg)
	}
	if cfg.SystemPrompt != "你是演示助手。" {
		t.Errorf("系统提示 = %q", cfg.SystemPrompt)
	}
	if cfg.Skills == nil {
		t.Error("skills_dir 应加载出技能集合")
	}
	if cfg.MaxIterations != 5 || cfg.CompressThreshold != 0.8 {
		t.Errorf("数值字段不正确: %+v", cfg)
	}
	if cfg.TotalTimeout != 10*time.Minute || cfg.ToolCallTimeout != 30*time.Second {
		t.Errorf("时长解析不正确: total=%v tool=%v", cfg.TotalTimeout, cfg.ToolCallTimeout)
	}
	if gotSpec.Name != "test-model" || gotSpec.Provider != "ark" {
		t.Errorf("ModelFactory 收到的声明不正确: %+v", gotSpec)
	}
}

func TestLoaderValidation(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "bad.yaml"), `
key: ""
system_prompt_file: missing.md
`)
	loader := NewLoader(LoaderDeps{
		ModelFactory: func(context.Context, ModelSpec) (model.ToolCallingChatModel, error) {
			return stubModel{}, nil
		},
	})
	_, err := loader.LoadDir(context.Background(), dir)
	if err == nil || !strings.Contains(err.Error(), "key is required") {
		t.Fatalf("缺少 key 应报错，实际: %v", err)
	}
}

func TestLoaderMaxIterationsDefault(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "p.md"), "prompt")
	write(t, filepath.Join(dir, "s.yaml"), `
key: s
system_prompt_file: p.md
model:
  name: m
`)
	loader := NewLoader(LoaderDeps{
		ModelFactory: func(context.Context, ModelSpec) (model.ToolCallingChatModel, error) {
			return stubModel{}, nil
		},
	})
	cfgs, err := loader.LoadDir(context.Background(), dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfgs[0].MaxIterations != 10 || cfgs[0].Name != "s" {
		t.Errorf("默认值不正确: %+v", cfgs[0])
	}
	if cfgs[0].ToolCallTimeout != 60*time.Second {
		t.Errorf("tool_call_timeout 默认值 = %v，期望 60s", cfgs[0].ToolCallTimeout)
	}
}

// stubTool 提供策略校验所需的工具名。
type stubTool struct{ name string }

func (t stubTool) Info(_ context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: t.name}, nil
}

func (t stubTool) InvokableRun(_ context.Context, _ string, _ ...einotool.Option) (string, error) {
	return "", nil
}

func TestLoaderParamPolicies(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "p.md"), "prompt")
	write(t, filepath.Join(dir, "s.yaml"), `
key: release
system_prompt_file: p.md
model:
  name: m
param_policies:
  - tool: release
    argument: ratio
    operator: gt
    value: 20
    message: 单次灰度比例不得超过 20
`)
	loader := NewLoader(LoaderDeps{
		ModelFactory: func(context.Context, ModelSpec) (model.ToolCallingChatModel, error) {
			return stubModel{}, nil
		},
		Tools: []einotool.BaseTool{stubTool{name: "release"}},
	})
	cfgs, err := loader.LoadDir(context.Background(), dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfgs[0].ParamPolicies) != 1 {
		t.Fatalf("param_policies 未解析: %+v", cfgs[0].ParamPolicies)
	}
	r := cfgs[0].ParamPolicies[0]
	if r.Tool != "release" || r.Argument != "ratio" || r.Operator != "gt" || r.Message == "" {
		t.Errorf("策略字段不正确: %+v", r)
	}
}

func TestLoaderParamPoliciesRejectsUnknownTool(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "p.md"), "prompt")
	write(t, filepath.Join(dir, "s.yaml"), `
key: release
system_prompt_file: p.md
model:
  name: m
param_policies:
  - tool: missing
    argument: ratio
    operator: gt
    value: 20
`)
	loader := NewLoader(LoaderDeps{
		ModelFactory: func(context.Context, ModelSpec) (model.ToolCallingChatModel, error) {
			return stubModel{}, nil
		},
		Tools: []einotool.BaseTool{stubTool{name: "release"}},
	})
	_, err := loader.LoadDir(context.Background(), dir)
	if err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Fatalf("策略指向未注册工具应拒绝加载，实际: %v", err)
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestExamplesScenesLoad 保证仓库交付的示例场景（examples/scenes）始终可被
// 加载器解析：改了字段名或目录约定而没同步示例时，这里会失败。
func TestExamplesScenesLoad(t *testing.T) {
	dir := filepath.Join("..", "..", "examples", "scenes")
	loader := NewLoader(LoaderDeps{
		ModelFactory: func(_ context.Context, spec ModelSpec) (model.ToolCallingChatModel, error) {
			return stubModel{name: spec.Name}, nil
		},
	})
	cfgs, err := loader.LoadDir(context.Background(), dir)
	if err != nil {
		t.Fatalf("示例场景加载失败: %v", err)
	}
	if len(cfgs) == 0 {
		t.Fatal("示例场景目录为空")
	}
	var echo *SceneConfig
	for _, cfg := range cfgs {
		if cfg.Key == "echo" {
			echo = cfg
		}
	}
	if echo == nil {
		t.Fatalf("示例场景缺少 echo，实际 %d 个", len(cfgs))
	}
	if echo.Skills == nil || len(echo.Skills.Frontmatter()) == 0 {
		t.Error("echo 场景应加载出技能索引")
	}
	if echo.TotalTimeout <= 0 || echo.ToolCallTimeout <= 0 {
		t.Errorf("超时配置未生效: total=%v tool=%v", echo.TotalTimeout, echo.ToolCallTimeout)
	}
}
