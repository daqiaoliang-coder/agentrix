package scene

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
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

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
