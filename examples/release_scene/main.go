// 发布变更 Agent 示例：演示三块拼图如何组合成一个完整业务应用。
//
// 以用户输入「把变更单 CHG-20260915-003 灰度发布到 10% 流量」为例，
// 完整走通四阶段流程：
//
//	阶段 0  会话启动   → Skills 的 frontmatter 注入系统提示（只含 name+description）
//	阶段 1  模型选 Skill → 调 read_skill 读 SKILL.md（能力边界 + 场景路由）
//	阶段 2  模型选 reference → 调 read_skill(reference) 读灰度发布细则
//	阶段 3  执行工具调用 → meego-cli 命令经 Catalog 映射到 Tool；
//	                       写命令由 ApprovalWrapper 触发人工授权后才执行
//
// 本示例还刻意演示了「两层治理」：
//   - Skill 描述能力全集（灰度 / 全量 / 回滚三种路由）
//   - 场景白名单决定本场景实际可用的命令子集（全量上线不放行）
//
// 装配产物（阶段 0 注入结果 + 白名单过滤结果）不依赖模型即可检视，
// 因此未配置 API Key 时也能运行，只是不会执行阶段 1-3 的模型决策。
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"sync/atomic"
	"time"

	openai "github.com/cloudwego/eino-ext/components/model/openai"
	einomodel "github.com/cloudwego/eino/components/model"
	einotool "github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"

	"github.com/daqiaoliang-coder/agentrix/internal/harness/core"
	"github.com/daqiaoliang-coder/agentrix/internal/harness/hitl"
	"github.com/daqiaoliang-coder/agentrix/internal/scene"
	"github.com/daqiaoliang-coder/agentrix/internal/session"
	"github.com/daqiaoliang-coder/agentrix/internal/skill"
	agenttool "github.com/daqiaoliang-coder/agentrix/internal/tool"
)

// ── 业务工具：实际项目中由 RPC 客户端实现，此处用占位实现演示链路 ──

// GetReleasePipelineTool 对应命令 release get-pipeline（只读，无需授权）
type GetReleasePipelineTool struct{ calls int64 }

// Calls 返回该工具被实际执行的次数（测试用）。
func (t *GetReleasePipelineTool) Calls() int64 { return atomic.LoadInt64(&t.calls) }

func (t *GetReleasePipelineTool) Info(_ context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "get_release_pipeline",
		Desc: "查询变更单的流水线状态、预检结论与当前灰度比例",
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"change_id": {
				Type:     schema.String,
				Desc:     "变更单号，如 CHG-20260915-003",
				Required: true,
			},
		}),
	}, nil
}

func (t *GetReleasePipelineTool) InvokableRun(_ context.Context, args string, _ ...einotool.Option) (string, error) {
	atomic.AddInt64(&t.calls, 1)
	fmt.Printf("  [tool] get_release_pipeline %s\n", args)
	return `{"change_id":"CHG-20260915-003","app":"order-service",` +
		`"pipeline":"SUCCESS","precheck":"PASSED","canary_allowed":true,` +
		`"current_canary_ratio":0}`, nil
}

// DeployCanaryTool 对应命令 release deploy-canary（写操作，需人工授权）
type DeployCanaryTool struct{ calls int64 }

// Calls 返回该工具被实际执行的次数（测试用）。
func (t *DeployCanaryTool) Calls() int64 { return atomic.LoadInt64(&t.calls) }

func (t *DeployCanaryTool) Info(_ context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "deploy_canary",
		Desc: "将变更单灰度发布到指定流量比例；ratio 置 0 表示回滚灰度",
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"change_id": {Type: schema.String, Desc: "变更单号", Required: true},
			"ratio": {
				Type:     schema.Integer,
				Desc:     "灰度流量百分比，取值 0-20；0 表示回滚",
				Required: true,
			},
			"instances": {
				Type:     schema.Array,
				Desc:     "目标实例列表；留空表示由平台自动选取",
				ElemInfo: &schema.ParameterInfo{Type: schema.String},
			},
		}),
	}, nil
}

func (t *DeployCanaryTool) InvokableRun(_ context.Context, args string, _ ...einotool.Option) (string, error) {
	atomic.AddInt64(&t.calls, 1)
	fmt.Printf("  [tool] deploy_canary %s\n", args)
	return `{"success":2,"failed":0,"canary_ratio":10,"observe_window_sec":900}`, nil
}

// DeployFullTool 对应命令 release deploy-full。
//
// 它已登记 Catalog，但本场景白名单刻意不放行 —— 装配时会被过滤掉，
// 模型根本看不到这个工具。用于直观演示场景级命令白名单的治理效果：
// 技能文档可以描述"全量上线"这项能力，但能不能用由场景策略决定。
type DeployFullTool struct{ calls int64 }

// Calls 返回该工具被实际执行的次数（测试用）。
// 本场景白名单未放行该命令，任何测试中此值都应恒为 0。
func (t *DeployFullTool) Calls() int64 { return atomic.LoadInt64(&t.calls) }

func (t *DeployFullTool) Info(_ context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "deploy_full",
		Desc: "将变更单全量上线到全部生产实例",
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"change_id": {Type: schema.String, Desc: "变更单号", Required: true},
		}),
	}, nil
}

func (t *DeployFullTool) InvokableRun(_ context.Context, args string, _ ...einotool.Option) (string, error) {
	atomic.AddInt64(&t.calls, 1)
	fmt.Printf("  [tool] deploy_full %s\n", args)
	return `{"success":8,"failed":0}`, nil
}

// ── Skill 内容：实际项目中由 go:embed 从 SKILL.md / references/*.md 嵌入 ──

const deploySkillBody = `# 发布变更技能

## 能力边界
- 仅可操作预检结论为 PASSED 的变更单；FAILED 或 RUNNING 必须停止并回报。
- 单次灰度比例不得超过 20%；更高比例须拆分为多次放量。
- 本技能不负责代码构建，构建失败请先联系流水线负责人。

## 场景路由
- 灰度放量 / 小流量验证 → 读 reference: canary_deploy_flow.md
- 紧急回滚            → 读 reference: rollback_flow.md
- 全量上线            → 读 reference: full_rollout_flow.md

## 共性约束
1. 写入授权：所有发布动作必须经人工确认，你不得自行完成授权，
   也不得以"风险很低"为由代替用户判断。
2. 参数校验：生成命令前必须先读取对应 reference，严禁凭猜测填写 change_id 或 ratio。
3. 先查后写：任何发布动作前，必须先用 get-pipeline 确认预检结论。
4. 部分失败：多实例发布允许部分成功，须如实汇报成功与失败实例，不得掩盖失败。`

const canaryFlowRef = `# 灰度发布细则

## 命令契约
查询流水线与预检结论（必须先执行）：
  meego-cli release get-pipeline --change-id <CHG-xxx>

灰度放量：
  meego-cli release deploy-canary --params '{"change_id":"<CHG-xxx>","ratio":<0-20>}'

## 执行顺序
1. get-pipeline 确认 precheck=PASSED 且 canary_allowed=true；否则停止并回报。
2. 依据用户要求的比例确定 ratio，单次不超过 20%。
3. 一次 deploy-canary 提交全部实例，不要按实例多次调用。

## 结果解释
返回 {"success":N,"failed":M,"canary_ratio":R,"observe_window_sec":S}。
failed>0 时须逐个列出失败实例与原因，并给出是否回滚的建议。

## 错误恢复
- precheck 非 PASSED：不得发布，回报预检详情。
- ratio 超上限：拆分为多次放量，先执行本次允许的最大比例。
- 部分实例失败：保留已成功实例，不得自动重试失败实例，等待用户决定。`

const rollbackFlowRef = `# 紧急回滚细则

## 命令契约
  meego-cli release deploy-canary --params '{"change_id":"<CHG-xxx>","ratio":0}'

## 说明
回滚通过将灰度比例置 0 实现，复用 deploy-canary 命令，无需独立命令，
因此它与灰度放量共享同一条白名单授权与同一套人工审批。`

const fullRolloutFlowRef = `# 全量上线细则

## 命令契约
  meego-cli release deploy-full --params '{"change_id":"<CHG-xxx>"}'

## 前置条件
灰度观察窗口已满，且核心指标（错误率、P99 延迟）无异常。

## 说明
全量上线属高风险变更，只在「全量上线」场景的白名单中放行。
若当前场景未放行该命令，调用会失败——此时应如实回报用户改走对应场景，
不要尝试用其他命令绕过白名单。`

const inspectSkillBody = `# 发布巡检技能

只读操作，无需人工授权。

## 场景路由
- 查流水线 / 预检结论 / 灰度进度 → 读 reference: pipeline_inspect_flow.md

## 约束
不得基于本技能执行任何发布动作；需要变更时改用发布变更技能。`

const pipelineInspectRef = `# 流水线巡检细则

## 命令契约
  meego-cli release get-pipeline --change-id <CHG-xxx>

## 结果解释
- pipeline: SUCCESS / RUNNING / FAILED
- precheck: PASSED / FAILED
- canary_allowed: 是否允许灰度
- current_canary_ratio: 当前灰度比例，0 表示尚未灰度`

// buildSkills 装配技能装载器（阶段 0/1/2 的内容来源）
func buildSkills() *skill.Loader {
	loader := skill.NewLoader()
	loader.Register(&skill.Skill{
		Name:        "meego-release-deploy",
		Description: "执行应用变更单的灰度发布、紧急回滚与全量上线",
		Body:        deploySkillBody,
		References: map[string]string{
			"canary_deploy_flow.md": canaryFlowRef,
			"rollback_flow.md":      rollbackFlowRef,
			"full_rollout_flow.md":  fullRolloutFlowRef,
		},
	})
	loader.Register(&skill.Skill{
		Name:        "meego-release-inspect",
		Description: "查询变更单流水线状态、预检结论与灰度进度",
		Body:        inspectSkillBody,
		References: map[string]string{
			"pipeline_inspect_flow.md": pipelineInspectRef,
		},
	})
	return loader
}

// buildCatalog 登记命令契约（命令 → 工具映射 + 展示归类 + 审批标记）
func buildCatalog() *agenttool.Catalog {
	catalog := agenttool.NewCatalog()

	// 只读命令：无需授权
	mustRegister(catalog, agenttool.CommandSpec{
		ToolName: "get_release_pipeline",
		Resource: "release", Command: "get-pipeline",
		Display: agenttool.Display{Action: "get", Resource: "release-pipeline"},
		Exec:    agenttool.ExecInProcess,
	})

	// 写命令：需人工授权（灰度放量与回滚共用）
	mustRegister(catalog, agenttool.CommandSpec{
		ToolName: "deploy_canary",
		Resource: "release", Command: "deploy-canary",
		Display:        agenttool.Display{Action: "deploy", Resource: "release-canary"},
		Exec:           agenttool.ExecInProcess,
		NeedsApproval:  true,
		ApprovalReason: "将对生产流量执行灰度发布，需人工确认变更窗口与回滚预案",
	})

	// 高风险写命令：已登记，但本场景白名单不放行
	mustRegister(catalog, agenttool.CommandSpec{
		ToolName: "deploy_full",
		Resource: "release", Command: "deploy-full",
		Display:        agenttool.Display{Action: "deploy", Resource: "release-full"},
		Exec:           agenttool.ExecInProcess,
		NeedsApproval:  true,
		ApprovalReason: "全量上线影响全部生产流量，需人工确认",
	})

	return catalog
}

func mustRegister(catalog *agenttool.Catalog, spec agenttool.CommandSpec) {
	if err := catalog.Register(spec); err != nil {
		log.Fatalf("register command %s: %v", spec.FullCommand(), err)
	}
}

// printAssembleResult 打印装配产物，让三块拼图的静态效果离线可见。
func printAssembleResult(ctx context.Context, res *scene.AssembleResult, catalog *agenttool.Catalog, allowed []string) {
	fmt.Println("═══ 阶段 0：装配后的系统提示（技能 frontmatter 已注入）═══")
	for _, line := range strings.Split(strings.TrimSpace(res.SystemPrompt), "\n") {
		fmt.Println("  " + line)
	}

	fmt.Println("\n═══ 模型可见工具（Catalog 白名单过滤后）═══")
	for _, tl := range res.Tools {
		info, err := tl.Info(ctx)
		if err != nil {
			log.Fatalf("tool info: %v", err)
		}
		spec, ok := catalog.SpecOf(info.Name)
		if !ok {
			fmt.Printf("  %-24s [框架工具，不受业务白名单管控]\n", info.Name)
			continue
		}
		gate := "只读"
		if spec.NeedsApproval {
			gate = "写操作 · 需人工授权"
		}
		fmt.Printf("  %-24s ← %-26s (%s)\n", info.Name, spec.FullCommand(), gate)
	}

	allowedSet := make(map[string]bool, len(allowed))
	for _, cmd := range allowed {
		allowedSet[cmd] = true
	}
	fmt.Println("\n═══ 被白名单拦截的命令（模型不可见）═══")
	blocked := 0
	for _, cmd := range catalog.Commands() {
		if !allowedSet[cmd] {
			fmt.Printf("  %-26s 本场景未放行\n", cmd)
			blocked++
		}
	}
	if blocked == 0 {
		fmt.Println("  （无）")
	}
	fmt.Println()
}

// AllowedCommands 是本场景放行的命令白名单。
//
// 刻意不放行 "release deploy-full"：技能文档描述了全量上线这项能力，
// 但能否使用由场景策略决定 —— 这正是「技能描述能力全集、白名单决定可用子集」
// 两层治理的体现。
var AllowedCommands = []string{
	"release get-pipeline",
	"release deploy-canary",
}

// NewSceneConfig 构造发布灰度场景配置。
//
// model 允许外部注入：main 传入真实模型观察阶段 1-3，测试传入脚本化假模型
// 做确定性验证。tools 同样可注入，以便测试持有实例引用来断言调用次数。
func NewSceneConfig(m einomodel.ToolCallingChatModel, tools []einotool.BaseTool) *scene.SceneConfig {
	return &scene.SceneConfig{
		Key:  "release_canary",
		Name: "发布灰度管理",
		SystemPrompt: "你是发布变更助手。处理任何具体操作前，" +
			"必须先用 read_skill 读取技能入口文档，再读取对应细则文档，严格按细则执行。",
		Model:            m,
		Tools:            tools,
		Skills:           buildSkills(),
		Catalog:          buildCatalog(),
		AllowedCommands:  AllowedCommands,
		MaxIterations:    8,
		TokenBudget:      16384,
		TotalTimeout:     2 * time.Minute,
		ModelCallTimeout: 20 * time.Second,
		NoProgressLimit:  3,
	}
}

// DefaultTools 返回本场景的默认业务工具集（含会被白名单过滤的 deploy_full）。
func DefaultTools() []einotool.BaseTool {
	return []einotool.BaseTool{
		&GetReleasePipelineTool{},
		&DeployCanaryTool{},
		&DeployFullTool{}, // 会被白名单过滤掉
	}
}

func main() {
	ctx := context.Background()

	// 装配产物不依赖模型，离线即可检视框架要素。
	// 此处先用 nil 模型装配，只打印静态结果。
	probe := NewSceneConfig(nil, DefaultTools())
	assembled, err := probe.Assemble(ctx)
	if err != nil {
		log.Fatalf("assemble scene: %v", err)
	}
	printAssembleResult(ctx, assembled, probe.Catalog, AllowedCommands)

	// ── 以下需要真实模型，用于观察阶段 1-3 的模型自主决策 ──
	apiKey := os.Getenv("ARK_API_KEY")
	if apiKey == "" {
		apiKey = os.Getenv("OPENAI_API_KEY")
	}
	modelName := os.Getenv("ARK_MODEL")
	if modelName == "" {
		modelName = os.Getenv("OPENAI_MODEL")
	}
	if apiKey == "" || modelName == "" {
		fmt.Println("已展示阶段 0 的框架装配要素（技能注入 + 白名单过滤 + 审批标记）。")
		fmt.Println("设置 ARK_API_KEY 与 ARK_MODEL 后重跑，可继续观察阶段 1-3：")
		fmt.Println("模型自主读 SKILL.md → 读灰度细则 → 触发人工授权 → 执行发布。")
		return
	}

	baseURL := os.Getenv("OPENAI_BASE_URL")
	if baseURL == "" {
		baseURL = "https://ark.cn-beijing.volces.com/api/v3"
	}
	chatModel, err := openai.NewChatModel(ctx, &openai.ChatModelConfig{
		Model: modelName, APIKey: apiKey, BaseURL: baseURL,
	})
	if err != nil {
		log.Fatalf("create chat model: %v", err)
	}

	// 用真实模型重新构造场景（前面的 probe 仅用于离线检视装配产物）
	cfg := NewSceneConfig(chatModel, DefaultTools())

	store := session.NewMemoryStore()
	agent, err := core.NewAgent(ctx, cfg, store)
	if err != nil {
		log.Fatalf("new agent: %v", err)
	}

	const sessionID = "release-session-1"
	const userInput = "把变更单 CHG-20260915-003 灰度发布到 10% 流量"
	fmt.Println("用户:", userInput)
	fmt.Println("─── 阶段 1-3：模型决策与工具执行 ───")

	output, err := agent.Run(ctx, sessionID, userInput)

	// ── 拼图 3：HITL 人工授权 ──
	// 写工具执行前会中断并返回审批错误。真实应用中这一步应交给人审批，
	// 此处为演示自动放行；把 Approved 改成 false 可观察拒绝分支。
	for round := 0; round < 5; round++ {
		approval, needApproval := core.ExtractApprovalRequired(err)
		if !needApproval {
			break
		}
		fmt.Printf("\n[待审批] 工具=%s\n         原因=%s\n         参数=%s\n",
			approval.Request.ToolName, approval.Request.Reason, approval.Request.Arguments)
		fmt.Println("[模拟审批人] 已批准")

		output, err = agent.Resume(ctx, sessionID, approval.InterruptID,
			&hitl.ApprovalDecision{
				Approved: true,
				Operator: "release-manager",
				Comment:  "在变更窗口内，回滚预案已确认",
			})
	}

	if err != nil {
		log.Fatalf("run agent: %v", err)
	}
	fmt.Println("\nAgent:", output.Content)
}
