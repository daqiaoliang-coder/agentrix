package eval_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	einotool "github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"

	"github.com/daqiaoliang-coder/agentrix/internal/eval"
	"github.com/daqiaoliang-coder/agentrix/internal/harness/hitl"
	"github.com/daqiaoliang-coder/agentrix/internal/projection"
	agenttool "github.com/daqiaoliang-coder/agentrix/internal/tool"
)

// fakeTool 是用例专用的最小工具实现：记录执行次数并返回固定结果。
type fakeTool struct {
	name   string
	result string
	runs   int64
}

func (t *fakeTool) Info(_ context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name:        t.name,
		Desc:        "评测用假工具：" + t.name,
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{}),
	}, nil
}

func (t *fakeTool) InvokableRun(_ context.Context, args string, _ ...einotool.Option) (string, error) {
	atomic.AddInt64(&t.runs, 1)
	if t.result != "" {
		return t.result, nil
	}
	return `{"ok":true,"args":` + args + `}`, nil
}

func mustCatalog(t *testing.T, writeApproved ...bool) *agenttool.Catalog {
	t.Helper()
	c := agenttool.NewCatalog()
	specs := []agenttool.CommandSpec{{
		ToolName: "list_draft", Resource: "wbs", Command: "list-draft",
		Exec: agenttool.ExecInProcess,
	}, {
		ToolName: "edit_draft", Resource: "wbs", Command: "edit-draft",
		Exec: agenttool.ExecInProcess, NeedsApproval: true, ApprovalReason: "写操作需授权",
	}}
	for _, s := range specs {
		if err := c.Register(s); err != nil {
			t.Fatalf("register %s: %v", s.ToolName, err)
		}
	}
	return c
}

// TestRunnerReadToolThenAnswer 演示最常见的读工具回放：
// 模型先调工具再收尾，断言输出内容、工具调用/结果与完整信号时序。
func TestRunnerReadToolThenAnswer(t *testing.T) {
	read := &fakeTool{name: "list_draft", result: `{"drafts":3}`}
	r := &eval.Runner{}
	rep := r.Run(context.Background(), eval.Case{
		Name:  "read_then_answer",
		Input: "列出草稿",
		Replies: []*schema.Message{
			eval.ToolCall("list_draft", `{}`),
			schema.AssistantMessage("草稿共 3 条", nil),
		},
		Tools: []einotool.BaseTool{read},
		Expect: []eval.Expectation{
			eval.ExpectNoError(),
			eval.ExpectFinalContains("草稿共 3 条"),
			eval.ExpectToolCalled("list_draft"),
			eval.ExpectToolCallArgsContains("list_draft", "{}"),
			eval.ExpectToolResultContains("list_draft", `"drafts":3`),
			eval.ExpectSignals(projection.LLMToken, projection.ToolStart, projection.ToolEnd),
			eval.ExpectSignalOrder(
				projection.TurnStart,
				projection.ToolStart,
				projection.ToolEnd,
				projection.TurnEnd,
			),
		},
	})

	if !rep.OK() {
		t.Fatalf("用例失败:\n%v", strings.Join(rep.Failures, "\n"))
	}
	if atomic.LoadInt64(&read.runs) != 1 {
		t.Errorf("工具应执行 1 次，实际 %d", read.runs)
	}
}

// TestRunnerApprovalInterrupt 演示 HITL 评测：不配置 AutoResume 时
// 运行停在审批中断，断言中断工具与 approve_requested 信号。
// 注意：tool_start 在节点执行前发射，审批拦截后工具不会真正运行，
// 「是否执行」须用工具自身计数（此处 fakeTool.runs）而非信号判断。
func TestRunnerApprovalInterrupt(t *testing.T) {
	write := &fakeTool{name: "edit_draft"}
	suite := (&eval.Runner{}).RunAll(context.Background(), []eval.Case{{
		Name:  "approval_blocked",
		Input: "改一下草稿",
		Replies: []*schema.Message{
			eval.ToolCall("edit_draft", `{"payload":"x"}`),
		},
		Tools:   []einotool.BaseTool{write},
		Catalog: mustCatalog(t),
		Expect: []eval.Expectation{
			eval.ExpectApprovalRequired("edit_draft"),
			eval.ExpectSignals(projection.ApproveRequested),
			eval.ExpectFinalNotContains("草稿"), // 中断时无最终输出
		},
	}})

	suite.Assert(t)
	if atomic.LoadInt64(&write.runs) != 0 {
		t.Errorf("授权前写工具不应执行，实际 %d 次", write.runs)
	}
}

// TestRunnerAutoResume 配置 AutoResume 后，中断自动批准并恢复，
// 模型用第二条脚本收尾，断言恢复后的最终输出。
func TestRunnerAutoResume(t *testing.T) {
	write := &fakeTool{name: "edit_draft"}
	rep := (&eval.Runner{}).Run(context.Background(), eval.Case{
		Name:  "approval_resumed",
		Input: "改一下草稿",
		Replies: []*schema.Message{
			eval.ToolCall("edit_draft", `{"payload":"x"}`),
			schema.AssistantMessage("已修改草稿", nil),
		},
		Tools:      []einotool.BaseTool{write},
		Catalog:    mustCatalog(t),
		AutoResume: &hitl.ApprovalDecision{Approved: true, Operator: "eval"},
		Expect: []eval.Expectation{
			eval.ExpectNoError(),
			eval.ExpectFinalContains("已修改草稿"),
			eval.ExpectSignals(projection.ApproveRequested, projection.ToolEnd),
		},
	})
	if !rep.OK() {
		t.Fatalf("用例失败:\n%v", strings.Join(rep.Failures, "\n"))
	}
	if atomic.LoadInt64(&write.runs) != 1 {
		t.Errorf("授权后写工具应执行 1 次，实际 %d", write.runs)
	}
}

// TestRunnerWhitelistHidesTool 演示白名单治理评测：模型任何一轮
// 都看不到白名单外工具——比「没调用」更强的装配侧保证。
func TestRunnerWhitelistHidesTool(t *testing.T) {
	suite := (&eval.Runner{}).RunAll(context.Background(), []eval.Case{{
		Name:            "whitelist_filters_write",
		Input:           "列出草稿",
		Replies:         []*schema.Message{schema.AssistantMessage("ok", nil)},
		Tools:           []einotool.BaseTool{&fakeTool{name: "list_draft"}, &fakeTool{name: "edit_draft"}},
		Catalog:         mustCatalog(t),
		AllowedCommands: []string{"wbs list-draft"},
		Expect: []eval.Expectation{
			eval.ExpectNoError(),
			eval.ExpectToolNotCalled("edit_draft"),
			eval.ExpectModelNeverSeesTool("edit_draft"),
		},
	}})
	suite.Assert(t)
}

// TestRunnerCollectsAssertionFailures 验证失败路径：断言不通过时
// Report.OK() 为 false 且 Failures 聚合全部失败明细（不短路）。
func TestRunnerCollectsAssertionFailures(t *testing.T) {
	rep := (&eval.Runner{}).Run(context.Background(), eval.Case{
		Name:    "intentional_failure",
		Input:   "hi",
		Replies: []*schema.Message{schema.AssistantMessage("实际内容", nil)},
		Expect: []eval.Expectation{
			eval.ExpectFinalContains("不存在的内容"),
			eval.ExpectToolCalled("从未出现的工具"),
		},
	})
	if rep.OK() || len(rep.Failures) != 2 {
		t.Fatalf("应收录 2 条失败，实际 %d: %v", len(rep.Failures), rep.Failures)
	}
	if !strings.Contains(rep.CaseName, "intentional_failure") {
		t.Errorf("报告用例名不正确: %q", rep.CaseName)
	}
}

func TestRunnerIDsAreUniqueAcrossInstances(t *testing.T) {
	first := (&eval.Runner{}).Run(context.Background(), eval.Case{
		Name: "first", Replies: []*schema.Message{schema.AssistantMessage("ok", nil)},
	})
	second := (&eval.Runner{}).Run(context.Background(), eval.Case{
		Name: "second", Replies: []*schema.Message{schema.AssistantMessage("ok", nil)},
	})
	if first.RunID == second.RunID {
		t.Fatalf("不同 Runner 复用了 RunID %q，会串用进程级 checkpoint", first.RunID)
	}
}
