package scene

import (
	"context"
	"fmt"
	"strings"

	einotool "github.com/cloudwego/eino/components/tool"

	agenttool "github.com/daqiaoliang-coder/agentrix/pkg/tool"
	"github.com/daqiaoliang-coder/agentrix/pkg/tool/builtin"
)

// AssembleResult 是场景装配的产物：最终系统提示与最终工具集。
type AssembleResult struct {
	SystemPrompt string
	Tools        []einotool.BaseTool
}

// Assemble 在 Agent 构建前完成四件事（均按配置开关生效，未配置时行为与旧版一致）：
//
//  1. Skill 接线（渐进式加载阶段 0）：把全部技能的 name+description 注入系统提示，
//     并自动注册 read_skill 工具，让模型可自主触发阶段 1（读 SKILL.md）与阶段 2（读 reference）。
//  2. 审批包装（HITL 触发点）：Catalog 中 NeedsApproval=true 的工具被套上
//     ApprovalWrapper，写操作执行前触发人工授权中断。
//  3. 参数级硬约束：命中 ParamPolicies 的调用在审批之前直接拒绝。
//  4. 命令白名单过滤：cfg.Catalog 与 cfg.AllowedCommands 同时存在时，
//     只保留命令在白名单内的业务工具；框架工具（read_skill 等）始终放行。
//
// 顺序很重要：参数策略必须包在审批包装之外（后包者在外层），命中硬约束的
// 调用不应再询问人工；审批先于白名单过滤——过滤基于 Info().Name，
// 各包装器均透传名称，互不干扰。
func (c *SceneConfig) Assemble(ctx context.Context) (*AssembleResult, error) {
	prompt := c.SystemPrompt
	tools := make([]einotool.BaseTool, len(c.Tools))
	copy(tools, c.Tools)

	// ① Skill 接线
	if c.Skills != nil {
		tools = append(tools, builtin.NewReadSkillTool(c.Skills))
		if idx := c.Skills.Frontmatter(); len(idx) > 0 {
			var sb strings.Builder
			sb.WriteString(prompt)
			if !strings.HasSuffix(prompt, "\n") && prompt != "" {
				sb.WriteString("\n")
			}
			sb.WriteString("\n## 可用技能\n")
			sb.WriteString("下列技能仅展示名称与用途。处理对应任务时，先用 read_skill 工具读取技能入口文档了解能力边界与场景路由；")
			sb.WriteString("执行具体操作前，必须再用 read_skill（传 reference 参数）读取对应细则文档，")
			sb.WriteString("严格按细则中的命令契约、执行顺序与错误恢复策略行事，不得凭猜测生成参数。\n")
			for _, m := range idx {
				sb.WriteString(m.Content)
				sb.WriteString("\n")
			}
			prompt = sb.String()
		}
	}

	// ② 审批包装
	if c.Catalog != nil {
		wrapped, err := agenttool.WrapTools(ctx, c.Catalog, tools)
		if err != nil {
			return nil, fmt.Errorf("assemble scene %s: %w", c.Key, err)
		}
		tools = wrapped
	}

	// ③ 参数级硬约束：必须包在审批之外——命中策略的调用不应先中断等人工，
	// 而应在进入审批前直接拒绝。包装顺序上「后包者在外层」。
	if len(c.ParamPolicies) > 0 {
		wrapped, err := agenttool.WrapPolicies(ctx, tools, c.ParamPolicies)
		if err != nil {
			return nil, fmt.Errorf("assemble scene %s: %w", c.Key, err)
		}
		tools = wrapped
	}

	// ④ 命令白名单过滤
	if c.Catalog != nil && len(c.AllowedCommands) > 0 {
		filtered, err := c.Catalog.FilterTools(ctx, tools, c.AllowedCommands)
		if err != nil {
			return nil, fmt.Errorf("assemble scene %s: %w", c.Key, err)
		}
		tools = filtered
	}

	// ⑤ 工具调用超时（最外层）：覆盖审批恢复后的实际执行
	if c.ToolCallTimeout > 0 {
		timed, err := agenttool.WrapTimeouts(ctx, tools, c.ToolCallTimeout)
		if err != nil {
			return nil, fmt.Errorf("assemble scene %s: %w", c.Key, err)
		}
		tools = timed
	}

	return &AssembleResult{SystemPrompt: prompt, Tools: tools}, nil
}
