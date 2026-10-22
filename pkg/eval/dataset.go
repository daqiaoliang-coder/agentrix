package eval

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Dataset 是可版本化的 JSON 评测任务集。
// 任务仅保存业务输入、场景选择和可读的断言；模型、Skills、Tools 由
// AgentConfigFactory 从业务代码装配，避免把密钥或可执行工具写进数据文件。
type Dataset struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description,omitempty"`
	Cases       []Task `json:"cases"`
}

// Task 是数据集中的一条 Agent 任务。
type Task struct {
	ID            string   `json:"id"`
	Scene         string   `json:"scene,omitempty"`
	Input         string   `json:"input"`
	Tags          []string `json:"tags,omitempty"`
	MaxIterations int      `json:"max_iterations,omitempty"`
	Checks        []Check  `json:"checks,omitempty"`
}

// Check 描述可移植的结构化断言。Value 用于文本或参数片段，Tool 用于工具检查，
// Limit 用于最大/最小工具调用数。需要业务状态 Oracle 时请通过 Case.Expect
// 在 Go 代码中添加自定义断言。
type Check struct {
	Type  string `json:"type"`
	Tool  string `json:"tool,omitempty"`
	Value string `json:"value,omitempty"`
	Limit int    `json:"limit,omitempty"`
}

const (
	CheckNoError            = "no_error"
	CheckFinalEquals        = "final_equals"
	CheckFinalContains      = "final_contains"
	CheckFinalNotContains   = "final_not_contains"
	CheckToolCalled         = "tool_called"
	CheckToolNotCalled      = "tool_not_called"
	CheckToolCallCount      = "tool_call_count"
	CheckToolArgsContain    = "tool_args_contains"
	CheckToolResultContains = "tool_result_contains"
	CheckApprovalRequired   = "approval_required"
	CheckMaxToolCalls       = "max_tool_calls"
	CheckMinToolCalls       = "min_tool_calls"
)

// LoadDataset 从 JSON 解码并校验数据集。未知字段会报错，便于尽早发现
// 拼写错误和版本不兼容。
func LoadDataset(r io.Reader) (Dataset, error) {
	decoder := json.NewDecoder(r)
	decoder.DisallowUnknownFields()
	var dataset Dataset
	if err := decoder.Decode(&dataset); err != nil {
		return Dataset{}, fmt.Errorf("decode eval dataset: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return Dataset{}, fmt.Errorf("decode eval dataset: trailing JSON value")
		}
		return Dataset{}, fmt.Errorf("decode eval dataset trailer: %w", err)
	}
	if err := dataset.Validate(); err != nil {
		return Dataset{}, err
	}
	return dataset, nil
}

// Validate 检查任务 ID、输入与断言是否完整且可执行。
func (d Dataset) Validate() error {
	if strings.TrimSpace(d.Name) == "" {
		return fmt.Errorf("eval dataset name is required")
	}
	if strings.TrimSpace(d.Version) == "" {
		return fmt.Errorf("eval dataset version is required")
	}
	if len(d.Cases) == 0 {
		return fmt.Errorf("eval dataset %q has no cases", d.Name)
	}
	seen := make(map[string]bool, len(d.Cases))
	for i, task := range d.Cases {
		if strings.TrimSpace(task.ID) == "" {
			return fmt.Errorf("case %d: id is required", i+1)
		}
		if seen[task.ID] {
			return fmt.Errorf("case %q: duplicate id", task.ID)
		}
		seen[task.ID] = true
		if strings.TrimSpace(task.Input) == "" {
			return fmt.Errorf("case %q: input is required", task.ID)
		}
		for j, check := range task.Checks {
			if err := check.Validate(); err != nil {
				return fmt.Errorf("case %q check %d: %w", task.ID, j+1, err)
			}
		}
	}
	return nil
}

// Validate 检查单个声明式断言。
func (c Check) Validate() error {
	switch c.Type {
	case CheckNoError, CheckApprovalRequired:
		return nil
	case CheckFinalEquals, CheckFinalContains, CheckFinalNotContains:
		if c.Value == "" {
			return fmt.Errorf("%s requires value", c.Type)
		}
	case CheckToolCalled, CheckToolNotCalled, CheckToolCallCount:
		if c.Tool == "" {
			return fmt.Errorf("%s requires tool", c.Type)
		}
		if c.Type == CheckToolCallCount && c.Limit < 0 {
			return fmt.Errorf("tool_call_count limit must be non-negative")
		}
	case CheckToolArgsContain, CheckToolResultContains:
		if c.Tool == "" || c.Value == "" {
			return fmt.Errorf("%s requires tool and value", c.Type)
		}
	case CheckMaxToolCalls, CheckMinToolCalls:
		if c.Limit < 0 {
			return fmt.Errorf("%s limit must be non-negative", c.Type)
		}
	default:
		return fmt.Errorf("unsupported check type %q", c.Type)
	}
	return nil
}

// ToSuite 把数据集转为运行器使用的 Suite。
func (d Dataset) ToSuite() (Suite, error) {
	if err := d.Validate(); err != nil {
		return Suite{}, err
	}
	cases := make([]Case, 0, len(d.Cases))
	for _, task := range d.Cases {
		tc, err := task.ToCase()
		if err != nil {
			return Suite{}, err
		}
		cases = append(cases, tc)
	}
	return Suite{Name: d.Name, Version: d.Version, Cases: cases}, nil
}

// ToCase 把一个数据集任务转换为运行用例。
func (t Task) ToCase() (Case, error) {
	tc := Case{
		Name:          t.ID,
		SceneKey:      t.Scene,
		Tags:          append([]string(nil), t.Tags...),
		Input:         t.Input,
		MaxIterations: t.MaxIterations,
	}
	for _, check := range t.Checks {
		expect, err := expectationFor(check)
		if err != nil {
			return Case{}, fmt.Errorf("case %q: %w", t.ID, err)
		}
		tc.Expect = append(tc.Expect, expect)
	}
	return tc, nil
}

func expectationFor(check Check) (Expectation, error) {
	if err := check.Validate(); err != nil {
		return nil, err
	}
	switch check.Type {
	case CheckNoError:
		return ExpectNoError(), nil
	case CheckFinalEquals:
		return ExpectFinalEquals(check.Value), nil
	case CheckFinalContains:
		return ExpectFinalContains(check.Value), nil
	case CheckFinalNotContains:
		return ExpectFinalNotContains(check.Value), nil
	case CheckToolCalled:
		return ExpectToolCalled(check.Tool), nil
	case CheckToolNotCalled:
		return ExpectToolNotCalled(check.Tool), nil
	case CheckToolCallCount:
		return ExpectToolCallCount(check.Tool, check.Limit), nil
	case CheckToolArgsContain:
		return ExpectToolCallArgsContains(check.Tool, check.Value), nil
	case CheckToolResultContains:
		return ExpectToolResultContains(check.Tool, check.Value), nil
	case CheckApprovalRequired:
		return ExpectApprovalRequired(check.Tool), nil
	case CheckMaxToolCalls:
		return ExpectMaxToolCalls(check.Limit), nil
	case CheckMinToolCalls:
		return ExpectMinToolCalls(check.Limit), nil
	default:
		return nil, fmt.Errorf("unsupported check type %q", check.Type)
	}
}
