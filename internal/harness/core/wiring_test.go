package core

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"

	"github.com/daqiaoliang-coder/agentrix/internal/harness/hitl"
	"github.com/daqiaoliang-coder/agentrix/internal/scene"
	"github.com/daqiaoliang-coder/agentrix/internal/session"
	"github.com/daqiaoliang-coder/agentrix/internal/skill"
	agenttool "github.com/daqiaoliang-coder/agentrix/internal/tool"
)

// ---------- 测试替身 ----------

// scriptModel 按预设脚本返回模型输出，并记录每次实际绑定的工具列表，
// 用于验证白名单过滤是否真正作用到模型侧。
type scriptModel struct {
	replies    []*schema.Message
	call       int
	seenTools  [][]string
	boundTools []*schema.ToolInfo
}

func (m *scriptModel) Generate(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	m.seenTools = append(m.seenTools, boundToolNames(m.boundTools))
	if m.call >= len(m.replies) {
		return schema.AssistantMessage("done", nil), nil
	}
	out := m.replies[m.call]
	m.call++
	return out, nil
}

func (m *scriptModel) Stream(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return nil, context.DeadlineExceeded
}

func (m *scriptModel) WithTools(tools []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	clone := *m
	clone.boundTools = tools
	return &clone, nil
}

func boundToolNames(infos []*schema.ToolInfo) []string {
	names := make([]string, 0, len(infos))
	for _, i := range infos {
		names = append(names, i.Name)
	}
	return names
}

// fakeWriteTool 模拟写类业务工具（如编辑排期草稿）。
type fakeWriteTool struct {
	name string
	ran  int
}

func (t *fakeWriteTool) Info(_ context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: t.name,
		Desc: "模拟写操作工具",
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"payload": {Type: schema.String, Desc: "写入内容", Required: true},
		}),
	}, nil
}

func (t *fakeWriteTool) InvokableRun(_ context.Context, _ string, _ ...tool.Option) (string, error) {
	t.ran++
	return `{"success":1,"failed":0}`, nil
}

// fakeReadTool 模拟读类业务工具，无需审批。
type fakeReadTool struct{ name string }

func (t *fakeReadTool) Info(_ context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name:        t.name,
		Desc:        "模拟读操作工具",
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{}),
	}, nil
}

func (t *fakeReadTool) InvokableRun(_ context.Context, _ string, _ ...tool.Option) (string, error) {
	return "read-ok", nil
}

func toolCallMsg(name, args string) *schema.Message {
	return &schema.Message{
		Role: schema.Assistant,
		ToolCalls: []schema.ToolCall{{
			ID:       "call_" + name,
			Function: schema.FunctionCall{Name: name, Arguments: args},
		}},
	}
}

// assembledToolNames 取出装配产物中所有工具名。
func assembledToolNames(t *testing.T, res *scene.AssembleResult) []string {
	t.Helper()
	ctx := context.Background()
	names := make([]string, 0, len(res.Tools))
	for _, tl := range res.Tools {
		info, err := tl.Info(ctx)
		if err != nil {
			t.Fatalf("tool info: %v", err)
		}
		names = append(names, info.Name)
	}
	return names
}

func mustRegisterSpec(t *testing.T, c *agenttool.Catalog, spec agenttool.CommandSpec) {
	t.Helper()
	if err := c.Register(spec); err != nil {
		t.Fatalf("catalog register: %v", err)
	}
}

func containsStr(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

func wbsCatalog(t *testing.T) *agenttool.Catalog {
	t.Helper()
	c := agenttool.NewCatalog()
	mustRegisterSpec(t, c, agenttool.CommandSpec{
		ToolName: "edit_draft", Resource: "wbs-arrangement", Command: "edit-draft",
		Exec: agenttool.ExecInProcess, NeedsApproval: true, ApprovalReason: "修改排期需人工授权",
	})
	mustRegisterSpec(t, c, agenttool.CommandSpec{
		ToolName: "list_draft", Resource: "wbs-arrangement", Command: "list-draft",
		Exec: agenttool.ExecInProcess,
	})
	return c
}

// ---------- 拼图 1：Skill 接线 ----------

func TestSkillWiringInjectsFrontmatterAndReadSkillTool(t *testing.T) {
	ctx := context.Background()

	loader := skill.NewLoader()
	loader.Register(&skill.Skill{
		Name:        "meego-wbs-arrangement-update",
		Description: "修改已有行的排期、负责人或交付物",
		Body:        "# 排期更新技能\n能力边界：仅可修改草稿态行。",
		References: map[string]string{
			"schedule_update_flow.md": "meego-cli wbs-arrangement edit-draft --params ...",
		},
	})

	cfg := &scene.SceneConfig{
		Key:           "wbs",
		SystemPrompt:  "你是排期助手。",
		Model:         &scriptModel{replies: []*schema.Message{schema.AssistantMessage("ok", nil)}},
		Tools:         []tool.BaseTool{},
		Skills:        loader,
		MaxIterations: 3,
	}

	res, err := cfg.Assemble(ctx)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}

	// 阶段 0：系统提示须含技能名、用途与读取约束
	sp := res.SystemPrompt
	for _, want := range []string{
		"meego-wbs-arrangement-update",
		"修改已有行的排期",
		"read_skill",
	} {
		if !strings.Contains(sp, want) {
			t.Errorf("系统提示缺少 %q，实际:\n%s", want, sp)
		}
	}

	// 阶段 1/2 入口：read_skill 工具须被自动注册
	if !containsStr(assembledToolNames(t, res), "read_skill") {
		t.Errorf("read_skill 未自动注册，实际工具: %v", assembledToolNames(t, res))
	}

	// 未配置 Skills 时行为不变（向后兼容）
	res2, err := (&scene.SceneConfig{Key: "plain", SystemPrompt: "原样", Model: cfg.Model}).Assemble(ctx)
	if err != nil {
		t.Fatalf("Assemble(plain): %v", err)
	}
	if res2.SystemPrompt != "原样" {
		t.Errorf("未配置技能时系统提示被改写: %q", res2.SystemPrompt)
	}
	if len(res2.Tools) != 0 {
		t.Errorf("未配置技能时不应注入工具，实际: %v", assembledToolNames(t, res2))
	}
}

func TestReadSkillToolServesBodyAndReference(t *testing.T) {
	ctx := context.Background()

	loader := skill.NewLoader()
	loader.Register(&skill.Skill{
		Name:        "wbs-update",
		Description: "修改排期",
		Body:        "入口文档正文",
		References:  map[string]string{"schedule_update_flow.md": "完整命令契约"},
	})

	res, err := (&scene.SceneConfig{Key: "wbs", Model: &scriptModel{}, Skills: loader}).Assemble(ctx)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	var readSkill tool.InvokableTool
	for _, tl := range res.Tools {
		info, _ := tl.Info(ctx)
		if info.Name == "read_skill" {
			readSkill = tl.(tool.InvokableTool)
		}
	}
	if readSkill == nil {
		t.Fatal("read_skill 未注册")
	}

	// 阶段 1：读正文，并附带 reference 清单
	body, err := readSkill.InvokableRun(ctx, `{"name":"wbs-update"}`)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !strings.Contains(body, "入口文档正文") || !strings.Contains(body, "schedule_update_flow.md") {
		t.Errorf("正文或 reference 清单缺失: %q", body)
	}

	// 阶段 2：读 reference 全文
	ref, err := readSkill.InvokableRun(ctx, `{"name":"wbs-update","reference":"schedule_update_flow.md"}`)
	if err != nil {
		t.Fatalf("read reference: %v", err)
	}
	if !strings.Contains(ref, "完整命令契约") {
		t.Errorf("reference 内容不正确: %q", ref)
	}

	// 名称错误时给出可用清单，帮助模型自纠
	if _, err := readSkill.InvokableRun(ctx, `{"name":"nope","reference":"x.md"}`); err == nil {
		t.Error("未知技能应报错")
	} else if !strings.Contains(err.Error(), "available skills") {
		t.Errorf("错误信息应含可用技能清单，实际: %v", err)
	}
}

// ---------- 拼图 2：Catalog 白名单过滤 ----------

func TestCatalogWhitelistFiltersTools(t *testing.T) {
	ctx := context.Background()
	catalog := wbsCatalog(t)
	tools := []tool.BaseTool{
		&fakeWriteTool{name: "edit_draft"},
		&fakeReadTool{name: "list_draft"},
	}

	// 白名单只放行读命令：写工具应被过滤
	res, err := (&scene.SceneConfig{
		Key: "wbs", Catalog: catalog, Tools: tools,
		AllowedCommands: []string{"wbs-arrangement list-draft"},
	}).Assemble(ctx)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	names := assembledToolNames(t, res)
	if containsStr(names, "edit_draft") {
		t.Errorf("白名单外命令未被过滤，实际可见: %v", names)
	}
	if !containsStr(names, "list_draft") {
		t.Errorf("白名单内命令被误过滤，实际可见: %v", names)
	}

	// 白名单为空：全量放行（向后兼容）
	res2, err := (&scene.SceneConfig{Key: "wbs", Catalog: catalog, Tools: tools}).Assemble(ctx)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	names2 := assembledToolNames(t, res2)
	if !containsStr(names2, "edit_draft") || !containsStr(names2, "list_draft") {
		t.Errorf("空白名单应全量放行，实际可见: %v", names2)
	}

	// 框架工具不受业务白名单管控
	loader := skill.NewLoader()
	loader.Register(&skill.Skill{Name: "s1", Description: "d1", Body: "b1"})
	res3, err := (&scene.SceneConfig{
		Key: "wbs", Catalog: catalog, Tools: tools, Skills: loader,
		AllowedCommands: []string{"wbs-arrangement list-draft"},
	}).Assemble(ctx)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if !containsStr(assembledToolNames(t, res3), "read_skill") {
		t.Errorf("框架工具 read_skill 被白名单误过滤，实际: %v", assembledToolNames(t, res3))
	}

	// 过滤结果须真正作用到模型侧
	m := &scriptModel{replies: []*schema.Message{schema.AssistantMessage("ok", nil)}}
	agent, err := NewAgent(ctx, &scene.SceneConfig{
		Key: "wbs", Model: m, Catalog: catalog, Tools: tools,
		AllowedCommands: []string{"wbs-arrangement list-draft"},
		MaxIterations:   2,
	}, session.NewMemoryStore())
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	if _, err := agent.Run(ctx, "sess-wl", "列出草稿"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(m.seenTools) == 0 {
		t.Fatal("模型未被调用，无法验证工具绑定")
	}
	if containsStr(m.seenTools[0], "edit_draft") {
		t.Errorf("模型仍看到白名单外的写工具: %v", m.seenTools[0])
	}
}

// ---------- 拼图 3：HITL 审批触发与恢复 ----------

func TestApprovalInterruptBlocksWriteUntilAuthorized(t *testing.T) {
	ctx := context.Background()
	catalog := wbsCatalog(t)
	writeTool := &fakeWriteTool{name: "edit_draft"}
	store := session.NewMemoryStore()

	newCfg := func(m model.ToolCallingChatModel) *scene.SceneConfig {
		return &scene.SceneConfig{
			Key: "wbs", Model: m, Tools: []tool.BaseTool{writeTool},
			Catalog: catalog, MaxIterations: 3,
		}
	}

	// ① 首跑：应中断等待审批，写工具不得执行
	m1 := &scriptModel{replies: []*schema.Message{toolCallMsg("edit_draft", `{"payload":"shift 5 days"}`)}}
	agent1, err := NewAgent(ctx, newCfg(m1), store)
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	_, runErr := agent1.Run(ctx, "sess-approval", "把选中两行各后移5天")
	if runErr == nil {
		t.Fatal("写操作未触发审批中断，Run 直接返回成功")
	}
	approval, ok := ExtractApprovalRequired(runErr)
	if !ok {
		t.Fatalf("未识别为审批中断，err=%v", runErr)
	}
	if approval.InterruptID == "" {
		t.Error("审批中断缺少 InterruptID，无法恢复")
	}
	if approval.Request == nil || approval.Request.ToolName != "edit_draft" {
		t.Errorf("审批请求信息不正确: %+v", approval.Request)
	}
	if writeTool.ran != 0 {
		t.Errorf("未授权情况下写工具被执行了 %d 次", writeTool.ran)
	}

	// ② 授权通过：恢复后写工具执行一次。
	//    恢复从工具节点继续，随后模型被调用，故脚本只给收尾回复。
	m2 := &scriptModel{replies: []*schema.Message{schema.AssistantMessage("已完成排期后移", nil)}}
	agent2, err := NewAgent(ctx, newCfg(m2), store)
	if err != nil {
		t.Fatalf("NewAgent(resume): %v", err)
	}
	out, err := agent2.Resume(ctx, "sess-approval", approval.InterruptID,
		&hitl.ApprovalDecision{Approved: true, Operator: "planner"})
	if err != nil {
		t.Fatalf("Resume(approve): %v", err)
	}
	if writeTool.ran != 1 {
		t.Errorf("授权后写工具应执行 1 次，实际 %d 次", writeTool.ran)
	}
	if out == nil || strings.TrimSpace(out.Content) == "" {
		t.Error("恢复后未返回模型输出")
	}
}

func TestApprovalRejectDoesNotExecuteWrite(t *testing.T) {
	ctx := context.Background()
	catalog := wbsCatalog(t)
	writeTool := &fakeWriteTool{name: "edit_draft"}
	store := session.NewMemoryStore()

	// 先制造一次中断（独立会话，避免与批准用例争用检查点）
	m1 := &scriptModel{replies: []*schema.Message{toolCallMsg("edit_draft", `{"payload":"x"}`)}}
	agent1, err := NewAgent(ctx, &scene.SceneConfig{
		Key: "wbs", Model: m1, Tools: []tool.BaseTool{writeTool},
		Catalog: catalog, MaxIterations: 3,
	}, store)
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	_, runErr := agent1.Run(ctx, "sess-reject", "改一下")
	approval, ok := ExtractApprovalRequired(runErr)
	if !ok {
		t.Fatalf("未识别为审批中断，err=%v", runErr)
	}

	// 拒绝授权：写工具不得执行
	m2 := &scriptModel{replies: []*schema.Message{schema.AssistantMessage("已取消该操作", nil)}}
	agent2, err := NewAgent(ctx, &scene.SceneConfig{
		Key: "wbs", Model: m2, Tools: []tool.BaseTool{writeTool},
		Catalog: catalog, MaxIterations: 3,
	}, store)
	if err != nil {
		t.Fatalf("NewAgent(reject): %v", err)
	}
	if _, err := agent2.Resume(ctx, "sess-reject", approval.InterruptID,
		&hitl.ApprovalDecision{Approved: false, Comment: "不在变更窗口", Operator: "planner"}); err != nil {
		t.Fatalf("Resume(reject) 不应报错，拒绝应作为工具结果回给模型: %v", err)
	}
	if writeTool.ran != 0 {
		t.Errorf("拒绝授权后写工具仍被执行 %d 次", writeTool.ran)
	}
}
