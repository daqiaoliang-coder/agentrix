package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"

	"github.com/daqiaoliang-coder/agentrix/internal/skill"
)

// ReadSkillTool 把 skill.Loader 的按需读取能力暴露为模型可调用的工具，
// 支撑渐进式加载的阶段 1（读 SKILL.md 正文）与阶段 2（读 references 细则）。
//
// 设计选择（沿用 meego-ai 原版路线 A）：reference 读取做成普通 Tool，
// 由模型自主决定何时加载，上下文占用最省；SKILL.md 中需约束
// "必须先读 reference 再生成命令"，避免模型跳过细则直接猜参数。
type ReadSkillTool struct {
	loader *skill.Loader
}

// NewReadSkillTool 构造技能读取工具。loader 为 nil 时工具仍可注册，但调用会返回错误。
func NewReadSkillTool(loader *skill.Loader) *ReadSkillTool {
	return &ReadSkillTool{loader: loader}
}

type readSkillArgs struct {
	// Skill 名称（必填）
	Name string `json:"name"`
	// reference 文档名（可选）：留空读取 SKILL.md 正文；填写则读取该 Skill 下指定的 reference
	Reference string `json:"reference,omitempty"`
}

func (t *ReadSkillTool) Info(_ context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "read_skill",
		Desc: "读取技能文档。不传 reference 时返回该技能的入口文档（能力边界与场景路由）；" +
			"传 reference 时返回该技能下指定细则文档的完整内容（命令格式、参数含义、执行顺序、错误恢复策略）。" +
			"执行具体业务操作前，必须先读取对应细则文档，不得凭猜测生成命令参数。",
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"name": {
				Type:     schema.String,
				Desc:     "技能名称，须与系统提示中列出的可用技能名完全一致",
				Required: true,
			},
			"reference": {
				Type:     schema.String,
				Desc:     "细则文档名（如 schedule_update_flow.md）；留空表示读取技能入口文档",
				Required: false,
			},
		}),
	}, nil
}

// withSkillHint 在原始错误后附上可用技能清单。
// 模型侧看到清单后可自行纠正技能名，而不是反复试错消耗迭代预算。
func withSkillHint(loader *skill.Loader, err error) error {
	names := loader.Names()
	if len(names) == 0 {
		return err
	}
	sort.Strings(names)
	return fmt.Errorf("%w; available skills: %s", err, strings.Join(names, ", "))
}

func (t *ReadSkillTool) InvokableRun(
	_ context.Context,
	argumentsInJSON string,
	_ ...tool.Option,
) (string, error) {
	if t.loader == nil {
		return "", fmt.Errorf("read_skill: skill loader not configured")
	}
	var args readSkillArgs
	if err := json.Unmarshal([]byte(argumentsInJSON), &args); err != nil {
		return "", fmt.Errorf("invalid read_skill arguments: %w", err)
	}
	if strings.TrimSpace(args.Name) == "" {
		return "", fmt.Errorf("read_skill: name is required")
	}

	// 阶段 2：读取指定 reference
	if strings.TrimSpace(args.Reference) != "" {
		body, err := t.loader.LoadReference(args.Name, args.Reference)
		if err != nil {
			// 技能存在但 reference 名错误：给出该技能下的可用细则清单
			if refs, refErr := t.loader.ReferenceNames(args.Name); refErr == nil && len(refs) > 0 {
				sort.Strings(refs)
				return "", fmt.Errorf("%w; available references: %s", err, strings.Join(refs, ", "))
			}
			// 技能名本身错误：给出可用技能清单
			return "", withSkillHint(t.loader, err)
		}
		return body, nil
	}

	// 阶段 1：读取 SKILL.md 正文
	body, err := t.loader.LoadBody(args.Name)
	if err != nil {
		return "", withSkillHint(t.loader, err)
	}

	// 正文后附带 reference 清单，让模型知道阶段 2 可读什么
	if refs, refErr := t.loader.ReferenceNames(args.Name); refErr == nil && len(refs) > 0 {
		sort.Strings(refs)
		body += "\n\n---\n可用细则文档（执行具体操作前用 read_skill 传入 reference 参数读取）：\n- " +
			strings.Join(refs, "\n- ")
	}
	return body, nil
}
