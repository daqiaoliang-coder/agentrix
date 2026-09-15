package tool

import (
	"context"
	"fmt"
	"sort"
	"strings"

	einotool "github.com/cloudwego/eino/components/tool"
)

// ExecLocation 表示工具的实际执行位置。
type ExecLocation string

const (
	// ExecInProcess 在 Agent 进程内直接执行（默认）。
	ExecInProcess ExecLocation = "in_process"
	// ExecSandbox 在隔离沙箱内执行（预留，当前回退为进程内）。
	ExecSandbox ExecLocation = "sandbox"
	// ExecSidecar 经 sidecar 转发执行（预留，需配合凭据隔离）。
	ExecSidecar ExecLocation = "sidecar"
)

// Display 描述命令在前端展示时的归类。
type Display struct {
	Action   string // 动作，如 "edit"
	Resource string // 资源，如 "wbs-arrangement"
}

// CommandSpec 描述一个工具对应的 CLI 命令契约。
// 模型侧看到的是统一命令 "<resource> <command>"，经 Catalog 映射到具体 Tool。
type CommandSpec struct {
	ToolName string       // 对应注册的工具名
	Resource string       // 命令资源段，如 "wbs-arrangement"
	Command  string       // 命令动作段，如 "edit-draft"
	Display  Display      // 前端展示归类
	Exec     ExecLocation // 执行位置
	// NeedsApproval 标记写类命令，执行前需人工审批（HITL）。
	NeedsApproval bool
	// ApprovalReason 审批提示语，NeedsApproval 为 true 时展示给审批人。
	ApprovalReason string
}

// FullCommand 返回完整命令字符串，如 "wbs-arrangement edit-draft"。
func (s CommandSpec) FullCommand() string {
	if s.Resource == "" {
		return s.Command
	}
	if s.Command == "" {
		return s.Resource
	}
	return s.Resource + " " + s.Command
}

// Catalog 维护「命令 → 工具」映射，承担展示与白名单治理职责。
// 它不改变执行链路：工具仍是 eino 的 tool.BaseTool，Catalog 只附加元数据。
type Catalog struct {
	specs map[string]CommandSpec // key = ToolName
}

func NewCatalog() *Catalog {
	return &Catalog{specs: make(map[string]CommandSpec)}
}

// Register 登记一个命令契约。spec.ToolName 为空时返回错误。
func (c *Catalog) Register(spec CommandSpec) error {
	if strings.TrimSpace(spec.ToolName) == "" {
		return fmt.Errorf("catalog: spec.ToolName is required")
	}
	if spec.Exec == "" {
		spec.Exec = ExecInProcess
	}
	c.specs[spec.ToolName] = spec
	return nil
}

// SpecOf 返回某工具名对应的命令契约；未登记时返回 nil。
func (c *Catalog) SpecOf(toolName string) (CommandSpec, bool) {
	spec, ok := c.specs[toolName]
	return spec, ok
}

// Commands 返回所有已登记命令（按字典序），用于诊断与展示。
func (c *Catalog) Commands() []string {
	out := make([]string, 0, len(c.specs))
	for _, spec := range c.specs {
		out = append(out, spec.FullCommand())
	}
	sort.Strings(out)
	return out
}

// FilterTools 按场景白名单过滤工具集：
//   - allowed 为空：不过滤，全量放行（向后兼容）。
//   - 已登记 Catalog 的工具：仅当完整命令在 allowed 内才保留。
//   - 未登记 Catalog 的工具（如框架内建的 read_skill）：始终放行，
//     因为白名单治理的是业务命令，框架工具不属于业务命令。
//
// 约定：业务命令类工具必须登记 Catalog，否则不受白名单管控。
func (c *Catalog) FilterTools(ctx context.Context, tools []einotool.BaseTool, allowed []string) ([]einotool.BaseTool, error) {
	if len(allowed) == 0 {
		return tools, nil
	}
	allowedSet := make(map[string]bool, len(allowed))
	for _, cmd := range allowed {
		if s := strings.TrimSpace(cmd); s != "" {
			allowedSet[s] = true
		}
	}
	out := make([]einotool.BaseTool, 0, len(tools))
	for _, t := range tools {
		info, err := t.Info(ctx)
		if err != nil {
			return nil, fmt.Errorf("catalog: read tool info: %w", err)
		}
		spec, ok := c.SpecOf(info.Name)
		if !ok {
			out = append(out, t) // 框架工具，放行
			continue
		}
		if allowedSet[spec.FullCommand()] {
			out = append(out, t)
		}
	}
	return out, nil
}
