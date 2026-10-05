package eval

import (
	"fmt"
	"strings"

	"github.com/daqiaoliang-coder/agentrix/internal/projection"
)

// Expectation 是一条声明式断言：检查运行报告，失败返回描述性错误。
type Expectation func(Report) error

// ExpectFinalContains 断言最终模型输出包含子串。
func ExpectFinalContains(sub string) Expectation {
	return func(r Report) error {
		if r.Output == nil {
			return fmt.Errorf("最终输出为 nil（err=%v），期望包含 %q", r.Err, sub)
		}
		if !strings.Contains(r.Output.Content, sub) {
			return fmt.Errorf("最终输出未包含 %q，实际 %q", sub, r.Output.Content)
		}
		return nil
	}
}

// ExpectFinalEquals 断言最终模型输出与期望文本完全相等。
func ExpectFinalEquals(want string) Expectation {
	return func(r Report) error {
		if r.Output == nil {
			return fmt.Errorf("最终输出为 nil（err=%v），期望等于 %q", r.Err, want)
		}
		if r.Output.Content != want {
			return fmt.Errorf("最终输出不匹配，期望 %q，实际 %q", want, r.Output.Content)
		}
		return nil
	}
}

// ExpectFinalNotContains 断言最终模型输出不包含子串。
func ExpectFinalNotContains(sub string) Expectation {
	return func(r Report) error {
		if r.Output != nil && strings.Contains(r.Output.Content, sub) {
			return fmt.Errorf("最终输出不应包含 %q，实际 %q", sub, r.Output.Content)
		}
		return nil
	}
}

// ExpectToolCalled 断言指定工具被调用（经 tool_start 信号取证，
// 不依赖工具实现自行计数）。
func ExpectToolCalled(name string) Expectation {
	return func(r Report) error {
		if !r.signalHasTool(name) {
			return fmt.Errorf("工具 %q 未被调用，实际调用: %v", name, r.toolNames())
		}
		return nil
	}
}

// ExpectToolNotCalled 断言指定工具全程未被调用。
func ExpectToolNotCalled(name string) Expectation {
	return func(r Report) error {
		if r.signalHasTool(name) {
			return fmt.Errorf("工具 %q 不应被调用，但出现了 tool_start 信号", name)
		}
		return nil
	}
}

// ExpectToolCallCount 断言指定工具恰好调用 count 次。
func ExpectToolCallCount(name string, count int) Expectation {
	return func(r Report) error {
		actual := r.toolCallCount(name)
		if actual != count {
			return fmt.Errorf("工具 %q 调用次数应为 %d，实际 %d", name, count, actual)
		}
		return nil
	}
}

// ExpectMaxToolCalls 断言一次运行的工具调用总数不超过 max。
func ExpectMaxToolCalls(max int) Expectation {
	return func(r Report) error {
		if r.Metrics.ToolCalls > max {
			return fmt.Errorf("工具调用次数超过上限 %d，实际 %d", max, r.Metrics.ToolCalls)
		}
		return nil
	}
}

// ExpectMinToolCalls 断言一次运行的工具调用总数不少于 min。
func ExpectMinToolCalls(min int) Expectation {
	return func(r Report) error {
		if r.Metrics.ToolCalls < min {
			return fmt.Errorf("工具调用次数低于下限 %d，实际 %d", min, r.Metrics.ToolCalls)
		}
		return nil
	}
}

// ExpectToolCallArgsContains 断言指定工具被调用且参数包含子串。
func ExpectToolCallArgsContains(name, argsSub string) Expectation {
	return func(r Report) error {
		for _, s := range r.Signals {
			if s.Type != projection.ToolStart {
				continue
			}
			p, _ := s.Payload.(map[string]any)
			if p["tool"] == name && strings.Contains(fmt.Sprint(p["arguments"]), argsSub) {
				return nil
			}
		}
		return fmt.Errorf("未找到工具 %q 参数包含 %q 的调用", name, argsSub)
	}
}

// ExpectToolResultContains 断言指定工具的回填结果包含子串（按 call_id 配对）。
func ExpectToolResultContains(name, resultSub string) Expectation {
	return func(r Report) error {
		callIDs := map[string]bool{}
		for _, s := range r.Signals {
			if s.Type != projection.ToolStart {
				continue
			}
			p, _ := s.Payload.(map[string]any)
			if p["tool"] == name {
				callIDs[fmt.Sprint(p["call_id"])] = true
			}
		}
		for _, s := range r.Signals {
			if s.Type != projection.ToolEnd {
				continue
			}
			p, _ := s.Payload.(map[string]any)
			if callIDs[fmt.Sprint(p["call_id"])] && strings.Contains(fmt.Sprint(p["result"]), resultSub) {
				return nil
			}
		}
		return fmt.Errorf("工具 %q 的结果未包含 %q", name, resultSub)
	}
}

// ExpectApprovalRequired 断言发生审批中断；tool 非空时进一步断言
// 请求审批的工具名。AutoResume 的用例不会停在中断，不应使用本断言。
func ExpectApprovalRequired(tool string) Expectation {
	return func(r Report) error {
		if r.Approval == nil {
			return fmt.Errorf("未发生审批中断（err=%v）", r.Err)
		}
		if tool != "" && r.Approval.Request.ToolName != tool {
			return fmt.Errorf("审批工具应为 %q，实际 %q", tool, r.Approval.Request.ToolName)
		}
		return nil
	}
}

// ExpectNoError 断言整个运行（含自动恢复）以非错误结束。
func ExpectNoError() Expectation {
	return func(r Report) error {
		if r.Err != nil {
			return fmt.Errorf("运行意外失败: %v", r.Err)
		}
		return nil
	}
}

// ExpectSignals 断言所有给定信号类型都至少出现一次（无序）。
func ExpectSignals(types ...projection.SignalType) Expectation {
	return func(r Report) error {
		for _, want := range types {
			if !r.hasSignal(want) {
				return fmt.Errorf("缺少信号 %q，实际序列 %v", want, r.signalTypes())
			}
		}
		return nil
	}
}

// ExpectSignalOrder 断言给定信号按顺序出现（子序列，允许中间穿插其他信号）。
func ExpectSignalOrder(types ...projection.SignalType) Expectation {
	return func(r Report) error {
		i := 0
		for _, s := range r.Signals {
			if i < len(types) && s.Type == types[i] {
				i++
			}
		}
		if i < len(types) {
			return fmt.Errorf("信号顺序不满足，缺失自 %q 起的子序列，实际 %v", types[i], r.signalTypes())
		}
		return nil
	}
}

// ExpectModelNeverSeesTool 断言模型任何一轮绑定的工具集中都不含 name。
// 与 ExpectToolNotCalled 的区别：本断言验证白名单在装配侧生效——
// 模型「看不到」该工具，而不只是「没调用」。
func ExpectModelNeverSeesTool(name string) Expectation {
	return func(r Report) error {
		if r.Model == nil {
			return fmt.Errorf("报告缺少模型引用，无法校验可见工具")
		}
		for round, names := range r.Model.SeenTools() {
			for _, n := range names {
				if n == name {
					return fmt.Errorf("第 %d 轮模型仍可见白名单外工具 %q: %v", round, name, names)
				}
			}
		}
		return nil
	}
}

func (r Report) hasSignal(typ projection.SignalType) bool {
	for _, s := range r.Signals {
		if s.Type == typ {
			return true
		}
	}
	return false
}

func (r Report) signalHasTool(name string) bool {
	for _, s := range r.Signals {
		if s.Type != projection.ToolStart {
			continue
		}
		if p, _ := s.Payload.(map[string]any); p["tool"] == name {
			return true
		}
	}
	return false
}

func (r Report) toolCallCount(name string) int {
	count := 0
	for _, s := range r.Signals {
		if s.Type != projection.ToolStart {
			continue
		}
		if p, _ := s.Payload.(map[string]any); p["tool"] == name {
			count++
		}
	}
	return count
}

func (r Report) signalTypes() []projection.SignalType {
	out := make([]projection.SignalType, 0, len(r.Signals))
	for _, s := range r.Signals {
		out = append(out, s.Type)
	}
	return out
}

func (r Report) toolNames() []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range r.Signals {
		if s.Type != projection.ToolStart {
			continue
		}
		if p, _ := s.Payload.(map[string]any); !seen[fmt.Sprint(p["tool"])] {
			seen[fmt.Sprint(p["tool"])] = true
			out = append(out, fmt.Sprint(p["tool"]))
		}
	}
	return out
}
