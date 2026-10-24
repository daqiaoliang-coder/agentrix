package tool

import (
	"context"
	"strings"
	"testing"

	einotool "github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

type policyEchoTool struct {
	ran int
}

func (t *policyEchoTool) Info(_ context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: "release"}, nil
}

func (t *policyEchoTool) InvokableRun(_ context.Context, _ string, _ ...einotool.Option) (string, error) {
	t.ran++
	return "released", nil
}

func TestCheckParamRulesNumericLimit(t *testing.T) {
	rules := []ParamRule{{
		Tool:     "release",
		Argument: "ratio",
		Operator: PolicyOpGT,
		Value:    20,
		Message:  "单次灰度比例不得超过 20%",
	}}

	if got := CheckParamRules("release", rules, `{"ratio":20}`); got != "" {
		t.Errorf("等于上限不应拒绝: %q", got)
	}
	if got := CheckParamRules("release", rules, `{"ratio":30}`); !strings.Contains(got, "ratio=30") ||
		!strings.Contains(got, "不得超过 20%") {
		t.Errorf("超限应拒绝并带上策略说明，实际: %q", got)
	}
	if got := CheckParamRules("release", rules, `{"other":1}`); got != "" {
		t.Errorf("字段缺失时规则不应命中: %q", got)
	}
	if got := CheckParamRules("release", rules, `{"ratio":"30"}`); !strings.Contains(got, "不是数值") {
		t.Errorf("非数值参数应拒绝，实际: %q", got)
	}
	if got := CheckParamRules("release", rules, `not-json`); !strings.Contains(got, "不是合法 JSON") {
		t.Errorf("非法 JSON 应拒绝，实际: %q", got)
	}
}

func TestCheckParamRulesNestedAndEquality(t *testing.T) {
	rules := []ParamRule{
		{Tool: "release", Argument: "opts.env", Operator: PolicyOpEQ, Value: "prod"},
		{Tool: "release", Argument: "force", Operator: PolicyOpEQ, Value: true},
	}
	if got := CheckParamRules("release", rules, `{"opts":{"env":"prod"}}`); !strings.Contains(got, "opts.env=prod") {
		t.Errorf("嵌套字段应命中 eq 策略: %q", got)
	}
	if got := CheckParamRules("release", rules, `{"opts":{"env":"staging"},"force":true}`); !strings.Contains(got, "force=true") {
		t.Errorf("布尔字段应命中 eq 策略: %q", got)
	}
	if got := CheckParamRules("release", rules, `{"opts":{"env":"staging"},"force":false}`); got != "" {
		t.Errorf("未命中时不应拒绝: %q", got)
	}
}

func TestPolicyWrapperBlocksBeforeExecution(t *testing.T) {
	ctx := context.Background()
	inner := &policyEchoTool{}
	wrapped, err := WrapPolicies(ctx, []einotool.BaseTool{inner}, []ParamRule{
		{Tool: "release", Argument: "ratio", Operator: PolicyOpGT, Value: 20},
	})
	if err != nil {
		t.Fatalf("wrap: %v", err)
	}
	out, err := wrapped[0].(einotool.InvokableTool).InvokableRun(ctx, `{"ratio":50}`)
	if err != nil {
		t.Fatalf("拒绝应作为工具结果返回而非 error: %v", err)
	}
	if !strings.Contains(out, "参数校验拒绝") {
		t.Errorf("应返回拒绝说明，实际: %q", out)
	}
	if inner.ran != 0 {
		t.Errorf("命中策略后工具仍被执行 %d 次", inner.ran)
	}

	out, err = wrapped[0].(einotool.InvokableTool).InvokableRun(ctx, `{"ratio":10}`)
	if err != nil || out != "released" || inner.ran != 1 {
		t.Errorf("未命中策略应正常执行: out=%q err=%v ran=%d", out, err, inner.ran)
	}
}

func TestWrapPoliciesRejectsUnknownTool(t *testing.T) {
	_, err := WrapPolicies(context.Background(), []einotool.BaseTool{&policyEchoTool{}}, []ParamRule{
		{Tool: "missing", Argument: "ratio", Operator: PolicyOpGT, Value: 20},
	})
	if err == nil || !strings.Contains(err.Error(), `"missing"`) {
		t.Fatalf("策略指向未注册工具应报错，实际: %v", err)
	}
}

func TestNormalizeParamRulesValidation(t *testing.T) {
	cases := []struct {
		name string
		rule ParamRule
		want string
	}{
		{"缺 tool", ParamRule{Argument: "a", Operator: "gt", Value: 1}, "tool is required"},
		{"缺 argument", ParamRule{Tool: "t", Operator: "gt", Value: 1}, "argument is required"},
		{"非法操作符", ParamRule{Tool: "t", Argument: "a", Operator: "between", Value: 1}, "unsupported operator"},
		{"缺 value", ParamRule{Tool: "t", Argument: "a", Operator: "gt"}, "value is required"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := NormalizeParamRules([]ParamRule{c.rule})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("期望错误包含 %q，实际: %v", c.want, err)
			}
		})
	}
}
