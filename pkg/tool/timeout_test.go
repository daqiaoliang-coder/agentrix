package tool

import (
	"context"
	"strings"
	"testing"
	"time"

	einotool "github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

// slowTool 是忽略 ctx 的慢工具，用于验证超时兜底。
type slowTool struct {
	delay time.Duration
}

func (s *slowTool) Info(_ context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: "slow"}, nil
}

func (s *slowTool) InvokableRun(_ context.Context, _ string, _ ...einotool.Option) (string, error) {
	time.Sleep(s.delay)
	return "done", nil
}

type fastTool struct{}

func (f *fastTool) Info(_ context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: "fast"}, nil
}

func (f *fastTool) InvokableRun(_ context.Context, _ string, _ ...einotool.Option) (string, error) {
	return "ok", nil
}

func TestTimeoutWrapper(t *testing.T) {
	ctx := context.Background()

	wrapped, err := WrapTimeouts(ctx, []einotool.BaseTool{&slowTool{delay: 200 * time.Millisecond}}, 20*time.Millisecond)
	if err != nil {
		t.Fatalf("wrap: %v", err)
	}
	out, err := wrapped[0].(einotool.InvokableTool).InvokableRun(ctx, "{}")
	if err != nil {
		t.Fatalf("超时应作为工具结果返回而非 error: %v", err)
	}
	if !strings.Contains(out, "超时") || !strings.Contains(out, "slow") {
		t.Errorf("超时文案缺少工具名或提示: %q", out)
	}

	fast, err := WrapTimeouts(ctx, []einotool.BaseTool{&fastTool{}}, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("wrap: %v", err)
	}
	out, err = fast[0].(einotool.InvokableTool).InvokableRun(ctx, "{}")
	if err != nil || out != "ok" {
		t.Errorf("正常工具应透传结果: out=%q err=%v", out, err)
	}
}

func TestWrapTimeoutsDisabled(t *testing.T) {
	tools := []einotool.BaseTool{&fastTool{}}
	got, err := WrapTimeouts(context.Background(), tools, 0)
	if err != nil {
		t.Fatalf("wrap: %v", err)
	}
	if len(got) != 1 || got[0] != tools[0] {
		t.Error("timeout<=0 时应原样返回工具集")
	}
}
