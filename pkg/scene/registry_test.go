package scene

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/cloudwego/eino/components/model"
)

func TestRegistryAssignsDefaultArtifactStore(t *testing.T) {
	reg := NewRegistry()
	reg.Register(&SceneConfig{Key: "a"})
	reg.Register(&SceneConfig{Key: "b"})

	a, _ := reg.Get("a")
	b, _ := reg.Get("b")
	if a.ArtifactStore == nil || b.ArtifactStore == nil {
		t.Fatal("注册时应为场景分配默认 artifact 存储")
	}
	if a.ArtifactStore == b.ArtifactStore {
		t.Error("不同场景不应共享同一进程内存储实例")
	}

	list := reg.List()
	if len(list) != 2 || list[0].Key != "a" || list[1].Key != "b" {
		t.Errorf("List 应按 key 排序返回全部场景，实际 %+v", list)
	}
}

// TestLoaderRejectsDuplicateKey 验证同一目录内重复 key 在启动期被拒绝，
// 避免后加载场景静默覆盖先加载场景。
func TestLoaderRejectsDuplicateKey(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "prompt.md"), "你是助手")
	for _, name := range []string{"a.yaml", "b.yaml"} {
		write(t, filepath.Join(dir, name), "key: dup\nsystem_prompt_file: prompt.md\n")
	}

	loader := NewLoader(LoaderDeps{
		ModelFactory: func(_ context.Context, _ ModelSpec) (model.ToolCallingChatModel, error) {
			return &stubModel{}, nil
		},
	})
	if _, err := loader.LoadDir(context.Background(), dir); err == nil {
		t.Fatal("重复 key 应导致加载失败")
	}
}
