package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	einotool "github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

// 参数策略支持的操作符。
const (
	PolicyOpGT  = "gt"
	PolicyOpGTE = "gte"
	PolicyOpLT  = "lt"
	PolicyOpLTE = "lte"
	PolicyOpEQ  = "eq"
	PolicyOpNEQ = "neq"
)

// ParamRule 描述一条参数级授权规则：当工具参数满足条件时直接拒绝执行。
//
// 与工具级 NeedsApproval 的区别：审批是「可以问人」，参数策略是「不问人、
// 直接拒绝」——用于灰度比例 > 20 这类无论谁批都不应放行的硬约束。
//
// Argument 支持点路径（如 "opts.ratio"）读取嵌套 JSON 字段；字段不存在时
// 规则不命中（是否必填由工具自身 schema 约束）。
type ParamRule struct {
	Tool     string `yaml:"tool" json:"tool"`
	Argument string `yaml:"argument" json:"argument"`
	Operator string `yaml:"operator" json:"operator"`
	Value    any    `yaml:"value" json:"value"`
	Message  string `yaml:"message" json:"message"`
}

// IsValidPolicyOperator 报告操作符是否受支持。
func IsValidPolicyOperator(op string) bool {
	switch strings.ToLower(strings.TrimSpace(op)) {
	case PolicyOpGT, PolicyOpGTE, PolicyOpLT, PolicyOpLTE, PolicyOpEQ, PolicyOpNEQ:
		return true
	default:
		return false
	}
}

// NormalizeParamRules 校验并规范化规则（去空格、操作符转小写）。
// 任一条非法即返回错误，避免带病配置上线。
func NormalizeParamRules(rules []ParamRule) ([]ParamRule, error) {
	if len(rules) == 0 {
		return nil, nil
	}
	out := make([]ParamRule, 0, len(rules))
	for i, r := range rules {
		r.Tool = strings.TrimSpace(r.Tool)
		r.Argument = strings.TrimSpace(r.Argument)
		r.Operator = strings.ToLower(strings.TrimSpace(r.Operator))
		r.Message = strings.TrimSpace(r.Message)
		if r.Tool == "" {
			return nil, fmt.Errorf("param_policies[%d]: tool is required", i)
		}
		if r.Argument == "" {
			return nil, fmt.Errorf("param_policies[%d]: argument is required", i)
		}
		if !IsValidPolicyOperator(r.Operator) {
			return nil, fmt.Errorf(
				"param_policies[%d]: unsupported operator %q (want gt|gte|lt|lte|eq|neq)",
				i, r.Operator,
			)
		}
		if r.Value == nil {
			return nil, fmt.Errorf("param_policies[%d]: value is required", i)
		}
		out = append(out, r)
	}
	return out, nil
}

// PolicyWrapper 在工具执行前校验参数级策略，命中即拒绝并把原因作为工具结果
// 返回给模型（不中断 Turn，让模型自行调整参数）。
type PolicyWrapper struct {
	inner einotool.InvokableTool
	name  string
	rules []ParamRule
}

// Info 透传被包装工具的元信息。
func (w *PolicyWrapper) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return w.inner.Info(ctx)
}

// InvokableRun 先校验参数，再执行被包装工具。
func (w *PolicyWrapper) InvokableRun(
	ctx context.Context,
	argumentsInJSON string,
	opts ...einotool.Option,
) (string, error) {
	if denial := CheckParamRules(w.name, w.rules, argumentsInJSON); denial != "" {
		return denial, nil
	}
	return w.inner.InvokableRun(ctx, argumentsInJSON, opts...)
}

// WrapPolicies 按工具名为工具集套上参数策略包装。
// 规则指向未注册工具时返回错误（策略未生效比配置错误更危险）。
func WrapPolicies(
	ctx context.Context,
	tools []einotool.BaseTool,
	rules []ParamRule,
) ([]einotool.BaseTool, error) {
	normalized, err := NormalizeParamRules(rules)
	if err != nil {
		return nil, err
	}
	if len(normalized) == 0 {
		return tools, nil
	}

	byTool := make(map[string][]ParamRule, len(normalized))
	for _, r := range normalized {
		byTool[r.Tool] = append(byTool[r.Tool], r)
	}

	matched := make(map[string]bool, len(byTool))
	out := make([]einotool.BaseTool, 0, len(tools))
	for _, t := range tools {
		if t == nil {
			continue
		}
		info, err := t.Info(ctx)
		if err != nil {
			return nil, fmt.Errorf("param policy: read tool info: %w", err)
		}
		rs := byTool[info.Name]
		if len(rs) == 0 {
			out = append(out, t)
			continue
		}
		invokable, ok := t.(einotool.InvokableTool)
		if !ok {
			return nil, fmt.Errorf("param policy: tool %q does not implement InvokableTool", info.Name)
		}
		matched[info.Name] = true
		out = append(out, &PolicyWrapper{inner: invokable, name: info.Name, rules: rs})
	}

	for name := range byTool {
		if !matched[name] {
			return nil, fmt.Errorf("param policy: tool %q is not registered in this scene", name)
		}
	}
	return out, nil
}

// CheckParamRules 校验参数并返回拒绝文案；空串表示通过。
//
// 数值比较操作符遇到非数值参数时按拒绝处理：类型不符可能是绕过上限的尝试，
// 放行比误拒更危险。
func CheckParamRules(toolName string, rules []ParamRule, argumentsInJSON string) string {
	if len(rules) == 0 {
		return ""
	}
	args, err := decodeArguments(argumentsInJSON)
	if err != nil {
		return fmt.Sprintf("参数校验拒绝：工具 %s 的参数不是合法 JSON 对象（%v），未执行。", toolName, err)
	}
	for _, r := range rules {
		actual, ok := lookupArgument(args, r.Argument)
		if !ok {
			continue
		}
		violated, reason := matchesRule(r, actual)
		if !violated {
			continue
		}
		msg := fmt.Sprintf(
			"参数校验拒绝：工具 %s 的 %s=%v 触发策略 %s %v，未执行。",
			toolName, r.Argument, actual, r.Operator, r.Value,
		)
		if reason != "" {
			msg += reason
		}
		if r.Message != "" {
			msg += r.Message
		}
		return msg
	}
	return ""
}

func decodeArguments(argumentsInJSON string) (map[string]any, error) {
	s := strings.TrimSpace(argumentsInJSON)
	if s == "" {
		return map[string]any{}, nil
	}
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var args map[string]any
	if err := dec.Decode(&args); err != nil {
		return nil, err
	}
	if args == nil {
		args = map[string]any{}
	}
	return args, nil
}

func lookupArgument(args map[string]any, path string) (any, bool) {
	var cur any = args
	for _, seg := range strings.Split(path, ".") {
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = obj[seg]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

func matchesRule(r ParamRule, actual any) (bool, string) {
	switch r.Operator {
	case PolicyOpGT, PolicyOpGTE, PolicyOpLT, PolicyOpLTE:
		af, ok := toFloat(actual)
		if !ok {
			return true, "参数类型不是数值，无法满足数值上限策略。"
		}
		bf, ok := toFloat(r.Value)
		if !ok {
			return true, "策略配置的 value 不是数值，拒绝执行。"
		}
		switch r.Operator {
		case PolicyOpGT:
			return af > bf, ""
		case PolicyOpGTE:
			return af >= bf, ""
		case PolicyOpLT:
			return af < bf, ""
		default:
			return af <= bf, ""
		}
	case PolicyOpEQ:
		return scalarEqual(actual, r.Value), ""
	case PolicyOpNEQ:
		return !scalarEqual(actual, r.Value), ""
	default:
		return true, "策略操作符不受支持，拒绝执行。"
	}
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int8:
		return float64(n), true
	case int16:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint:
		return float64(n), true
	case uint8:
		return float64(n), true
	case uint16:
		return float64(n), true
	case uint32:
		return float64(n), true
	case uint64:
		return float64(n), true
	case float32:
		return float64(n), true
	case float64:
		return n, true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}

func scalarEqual(a, b any) bool {
	af, aok := toFloat(a)
	bf, bok := toFloat(b)
	if aok && bok {
		return af == bf
	}
	if aok != bok {
		return false
	}
	return fmt.Sprint(a) == fmt.Sprint(b)
}
