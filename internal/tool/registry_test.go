package tool

import (
	"context"
	"testing"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

// stubTool 是最小的 BaseTool 实现，仅用于验证注册表顺序。
type stubTool struct{ name string }

func (t *stubTool) Info(_ context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: t.name, Desc: t.name}, nil
}

// listNames 取出 List() 产物中每个工具的名称，便于按序比较。
func listNames(t *testing.T, r *Registry) []string {
	t.Helper()
	ctx := context.Background()
	tools := r.List()
	names := make([]string, 0, len(tools))
	for _, tl := range tools {
		info, err := tl.Info(ctx)
		if err != nil {
			t.Fatalf("tool info: %v", err)
		}
		names = append(names, info.Name)
	}
	return names
}

func joinNames(names []string) string {
	out := ""
	for i, n := range names {
		if i > 0 {
			out += ","
		}
		out += n
	}
	return out
}

// TestRegistryListOrderIsStableAcrossCalls 验证 §2 的同源问题：List() 直接遍历 map
// 会得到每次随机的顺序，而工具 schema 的排列属于 prompt 前缀的一部分，顺序一变缓存作废。
func TestRegistryListOrderIsStableAcrossCalls(t *testing.T) {
	r := NewRegistry()
	// 乱序注册，排除字典序恰好等于注册序的假阳性
	for _, n := range []string{"zeta", "alpha", "mid", "beta", "omega"} {
		r.Register(n, &stubTool{name: n})
	}
	first := joinNames(listNames(t, r))
	for i := 0; i < 50; i++ {
		if got := joinNames(listNames(t, r)); got != first {
			t.Fatalf("第 %d 次 List 顺序漂移：首次=%q 本次=%q", i, first, got)
		}
	}
}

// TestRegistryListFollowsRegistrationOrder 验证 List() 顺序恒等于注册顺序。
func TestRegistryListFollowsRegistrationOrder(t *testing.T) {
	r := NewRegistry()
	order := []string{"zeta", "alpha", "mid"}
	for _, n := range order {
		r.Register(n, &stubTool{name: n})
	}
	got := listNames(t, r)
	for i, want := range order {
		if got[i] != want {
			t.Errorf("第 %d 个应为 %q，实际 %q", i, want, got[i])
		}
	}
}

// TestRegistryOverwriteKeepsPosition 验证同名重复注册覆盖实例但不改变原有顺序。
func TestRegistryOverwriteKeepsPosition(t *testing.T) {
	r := NewRegistry()
	r.Register("a", &stubTool{name: "a"})
	r.Register("b", &stubTool{name: "b"})
	r.Register("a", &stubTool{name: "a"}) // 覆盖 a

	got := listNames(t, r)
	if len(got) != 2 {
		t.Fatalf("重复注册不应新增条目，应有 2 个，实际 %d: %v", len(got), got)
	}
	if got[0] != "a" || got[1] != "b" {
		t.Errorf("覆盖后顺序应保持 a,b，实际 %v", got)
	}
}

// TestRegistryIgnoresInvalid 验证空名/nil 工具被安全忽略。
func TestRegistryIgnoresInvalid(t *testing.T) {
	r := NewRegistry()
	r.Register("", &stubTool{name: "x"})
	r.Register("nil", nil)
	r.Register("ok", &stubTool{name: "ok"})

	got := listNames(t, r)
	if len(got) != 1 || got[0] != "ok" {
		t.Errorf("应只保留有效工具 ok，实际 %v", got)
	}
}

// 编译期确认 stubTool 满足 tool.BaseTool 接口
var _ tool.BaseTool = (*stubTool)(nil)
