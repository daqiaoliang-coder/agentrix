package core

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"

	ctxengine "github.com/daqiaoliang-coder/agentrix/internal/context"
	"github.com/daqiaoliang-coder/agentrix/internal/harness/budget"
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

func (m *scriptModel) record() *schema.Message {
	m.seenTools = append(m.seenTools, boundToolNames(m.boundTools))
	if m.call >= len(m.replies) {
		return schema.AssistantMessage("done", nil)
	}
	out := m.replies[m.call]
	m.call++
	return out
}

func (m *scriptModel) Generate(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	return m.record(), nil
}

// Stream 模拟真实模型的 token 流：Content 切为增量帧逐帧返回，
// ToolCalls 整体置于末帧（真实模型的工具调用也以整段收尾）。
// 帧式产出是 llm_token 信号测试的前提。
func (m *scriptModel) Stream(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return schema.StreamReaderFromArray(chunkFrames(m.record(), 4)), nil
}

// chunkFrames 把完整消息切成流式帧：正文按 n 字符切分，工具调用并入末帧。
func chunkFrames(msg *schema.Message, n int) []*schema.Message {
	if msg == nil {
		return nil
	}
	content := []rune(msg.Content)
	var frames []*schema.Message
	for i := 0; i < len(content); i += n {
		end := i + n
		if end > len(content) {
			end = len(content)
		}
		frames = append(frames, schema.AssistantMessage(string(content[i:end]), nil))
	}
	if len(frames) == 0 {
		frames = append(frames, schema.AssistantMessage("", nil))
	}
	if len(msg.ToolCalls) > 0 {
		frames[len(frames)-1].ToolCalls = msg.ToolCalls
	}
	return frames
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

func useCheckpointStore(t *testing.T) *hitl.MemoryCheckPointStore {
	t.Helper()
	previous := hitl.DefaultCheckPointStore()
	store := hitl.NewMemoryCheckPointStore()
	hitl.SetDefaultCheckPointStore(store)
	t.Cleanup(func() { hitl.SetDefaultCheckPointStore(previous) })
	return store
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
	checkpointStore := useCheckpointStore(t)
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
	if _, exists, err := checkpointStore.Get(ctx, "sess-approval"); err != nil || !exists {
		t.Fatalf("审批中断后应保留 checkpoint: exists=%v err=%v", exists, err)
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
	if _, exists, err := checkpointStore.Get(ctx, "sess-approval"); err != nil || exists {
		t.Fatalf("恢复完成后 checkpoint 应删除: exists=%v err=%v", exists, err)
	}

	_, err = agent2.Resume(ctx, "sess-approval", approval.InterruptID,
		&hitl.ApprovalDecision{Approved: true, Operator: "planner"})
	if !errors.Is(err, ErrNoPendingApproval) {
		t.Fatalf("重复 Resume 应返回 ErrNoPendingApproval，实际: %v", err)
	}
	if writeTool.ran != 1 {
		t.Errorf("重复 Resume 不得再次执行写工具，实际执行 %d 次", writeTool.ran)
	}
}

func TestApprovalRejectDoesNotExecuteWrite(t *testing.T) {
	ctx := context.Background()
	checkpointStore := useCheckpointStore(t)
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
	if _, exists, err := checkpointStore.Get(ctx, "sess-reject"); err != nil || exists {
		t.Fatalf("拒绝完成后 checkpoint 应删除: exists=%v err=%v", exists, err)
	}
}

func TestSecondApprovalInterruptKeepsCheckpoint(t *testing.T) {
	ctx := context.Background()
	checkpointStore := useCheckpointStore(t)
	catalog := wbsCatalog(t)
	mustRegisterSpec(t, catalog, agenttool.CommandSpec{
		ToolName: "publish_draft", Resource: "wbs-arrangement", Command: "publish-draft",
		Exec: agenttool.ExecInProcess, NeedsApproval: true, ApprovalReason: "发布排期需人工授权",
	})
	editTool := &fakeWriteTool{name: "edit_draft"}
	publishTool := &fakeWriteTool{name: "publish_draft"}
	store := session.NewMemoryStore()
	newCfg := func(m model.ToolCallingChatModel) *scene.SceneConfig {
		return &scene.SceneConfig{
			Key: "wbs", Model: m, Tools: []tool.BaseTool{editTool, publishTool},
			Catalog: catalog, MaxIterations: 4,
		}
	}

	agent1, err := NewAgent(ctx, newCfg(&scriptModel{replies: []*schema.Message{
		toolCallMsg("edit_draft", `{"payload":"edit"}`),
	}}), store)
	if err != nil {
		t.Fatalf("NewAgent(first): %v", err)
	}
	_, err = agent1.Run(ctx, "sess-second-approval", "编辑并发布")
	firstApproval, ok := ExtractApprovalRequired(err)
	if !ok {
		t.Fatalf("第一次审批中断未识别: %v", err)
	}

	agent2, err := NewAgent(ctx, newCfg(&scriptModel{replies: []*schema.Message{
		toolCallMsg("publish_draft", `{"payload":"publish"}`),
	}}), store)
	if err != nil {
		t.Fatalf("NewAgent(second): %v", err)
	}
	_, err = agent2.Resume(ctx, "sess-second-approval", firstApproval.InterruptID,
		&hitl.ApprovalDecision{Approved: true, Operator: "planner"})
	secondApproval, ok := ExtractApprovalRequired(err)
	if !ok {
		t.Fatalf("第二次审批中断未识别: %v", err)
	}
	if editTool.ran != 1 || publishTool.ran != 0 {
		t.Fatalf("第二次中断时工具执行次数不正确: edit=%d publish=%d", editTool.ran, publishTool.ran)
	}
	if _, exists, err := checkpointStore.Get(ctx, "sess-second-approval"); err != nil || !exists {
		t.Fatalf("第二次审批中断后应保留最新 checkpoint: exists=%v err=%v", exists, err)
	}

	agent3, err := NewAgent(ctx, newCfg(&scriptModel{replies: []*schema.Message{
		schema.AssistantMessage("已编辑，发布已取消", nil),
	}}), store)
	if err != nil {
		t.Fatalf("NewAgent(final): %v", err)
	}
	if _, err := agent3.Resume(ctx, "sess-second-approval", secondApproval.InterruptID,
		&hitl.ApprovalDecision{Approved: false, Operator: "planner"}); err != nil {
		t.Fatalf("第二次审批拒绝后应正常完成: %v", err)
	}
	if _, exists, err := checkpointStore.Get(ctx, "sess-second-approval"); err != nil || exists {
		t.Fatalf("二次审批终态后 checkpoint 应删除: exists=%v err=%v", exists, err)
	}
}

func TestRunIgnoresAndDeletesStaleCheckpoint(t *testing.T) {
	ctx := context.Background()
	checkpointStore := useCheckpointStore(t)
	const sessionID = "sess-stale"
	if err := checkpointStore.Set(ctx, sessionID, []byte("invalid stale checkpoint")); err != nil {
		t.Fatalf("Set checkpoint: %v", err)
	}

	agent, err := NewAgent(ctx, &scene.SceneConfig{
		Key:           "plain",
		Model:         &scriptModel{replies: []*schema.Message{schema.AssistantMessage("fresh", nil)}},
		MaxIterations: 2,
	}, session.NewMemoryStore())
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	out, err := agent.Run(ctx, sessionID, "new turn")
	if err != nil {
		t.Fatalf("Run 应忽略陈旧 checkpoint: %v", err)
	}
	if out.Content != "fresh" {
		t.Fatalf("Run 输出 = %q，期望 fresh", out.Content)
	}
	if _, exists, err := checkpointStore.Get(ctx, sessionID); err != nil || exists {
		t.Fatalf("新运行完成后 checkpoint 应删除: exists=%v err=%v", exists, err)
	}
}

// ---------- 拼图 4：压缩接线（开销快照 + 回读工具 + 非幂等标记）----------

// TestNewAgentWiresCompressionPlumbing 验证三项装配确实生效：
//  1. read_result 回读工具已注册到模型可见的工具集（offload 闭环的入口）；
//  2. 开销快照已按装配后的系统提示与工具 schema 计算并注入（阈值不再失真）；
//  3. Catalog 的 NeedsApproval 已接线为非幂等判定（写结果不会被淘汰）。
func TestNewAgentWiresCompressionPlumbing(t *testing.T) {
	ctx := context.Background()
	catalog := wbsCatalog(t)
	readTool := &fakeReadTool{name: "list_draft"}
	writeTool := &fakeWriteTool{name: "edit_draft"}

	agent, err := NewAgent(ctx, &scene.SceneConfig{
		Key:                "wbs",
		SystemPrompt:       "你是排期助手",
		Model:              &scriptModel{},
		Tools:              []tool.BaseTool{readTool, writeTool},
		Catalog:            catalog,
		MaxIterations:      3,
		ModelContextWindow: 8192,
		TokenBudget:        2048,
	}, session.NewMemoryStore())
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	if got := agent.engine.ModelContextWindow; got != 8192 {
		t.Fatalf("模型上下文窗口 = %d，期望 8192", got)
	}
	if got := agent.cfg.TokenBudget; got != 2048 {
		t.Fatalf("Turn Token 预算 = %d，期望 2048", got)
	}

	// ① read_result 必须对模型可见，否则被淘汰的结果无法回读
	names := assembledToolNames(t, agent.assembled)
	if !containsStr(names, "read_result") {
		t.Errorf("read_result 未注册，实际工具集: %v", names)
	}
	// 原有工具不受影响
	for _, want := range []string{"list_draft", "edit_draft"} {
		if !containsStr(names, want) {
			t.Errorf("工具 %q 意外丢失，实际: %v", want, names)
		}
	}

	// ② 固定开销只计入不在 messages 中的工具 schema；system prompt 由消息估算负责。
	overhead := agent.engine.Overhead()
	if overhead.ToolsTokens <= 0 {
		t.Errorf("工具 schema 开销未计入快照: %+v", overhead)
	}
	// 装配后的 system prompt 会作为首条消息参与估算。
	if !strings.Contains(agent.assembled.SystemPrompt, "你是排期助手") {
		t.Error("装配后的系统提示丢失了原始正文")
	}

	probe := []*schema.Message{schema.UserMessage(strings.Repeat("x", 8000))}
	effective := ctxengine.EstimateMessagesTokens(probe) + overhead.Total()
	if effective <= agent.cfg.TokenBudget {
		t.Fatalf("测试前提不成立：effective tokens %d 未超过 Turn 预算 %d", effective, agent.cfg.TokenBudget)
	}
	windowSoftLimit := agent.engine.ModelContextWindow * 8 / 10
	if effective >= windowSoftLimit {
		t.Fatalf("测试前提不成立：effective tokens %d 已达到窗口软阈值 %d", effective, windowSoftLimit)
	}
	_, stats, err := agent.engine.CompressInPlaceWithStats(ctx, probe)
	if err != nil {
		t.Fatalf("仅超过 Turn 预算不应被上下文窗口拒绝: %v", err)
	}
	if stats.Triggered {
		t.Fatal("Turn Token 预算被错误复用为上下文压缩阈值")
	}

	// ③ 写工具判定为非幂等，读工具不是
	if agent.engine.IsNonIdempotent == nil {
		t.Fatal("IsNonIdempotent 未接线，写结果会被误淘汰")
	}
	if !agent.engine.IsNonIdempotent("edit_draft") {
		t.Error("edit_draft 应被判定为写/非幂等")
	}
	if agent.engine.IsNonIdempotent("list_draft") {
		t.Error("list_draft 是只读工具，不应被判定为非幂等")
	}

	// 溢出存储已接入，offload 才可能可恢复
	if agent.engine.Overhead().Total() <= 0 {
		t.Error("开销快照总量为零，压缩阈值会失真")
	}
}

func TestBudgetModelRejectsOversizedInputBeforeModelCall(t *testing.T) {
	engine := ctxengine.NewEngine()
	engine.ModelContextWindow = 100
	raw := &scriptModel{replies: []*schema.Message{schema.AssistantMessage("不应调用", nil)}}
	decorated := NewBudgetModel(raw, BudgetModelConfig{Engine: engine})
	input := []*schema.Message{schema.UserMessage(strings.Repeat("oversized", 100))}

	if _, err := decorated.Generate(context.Background(), input); !errors.Is(err, ctxengine.ErrContextWindowExceeded) {
		t.Fatalf("Generate 错误 = %v，期望 ErrContextWindowExceeded", err)
	}
	if raw.call != 0 {
		t.Fatalf("Generate 超窗后仍调用底层模型 %d 次", raw.call)
	}
	if _, err := decorated.Stream(context.Background(), input); !errors.Is(err, ctxengine.ErrContextWindowExceeded) {
		t.Fatalf("Stream 错误 = %v，期望 ErrContextWindowExceeded", err)
	}
	if raw.call != 0 {
		t.Fatalf("Stream 超窗后仍调用底层模型 %d 次", raw.call)
	}
}

// TestBudgetModelStreamRejectsExhaustedBudget 验证 Stream 路径在预算耗尽时拒绝调用。
func TestBudgetModelStreamRejectsExhaustedBudget(t *testing.T) {
	raw := &scriptModel{replies: []*schema.Message{schema.AssistantMessage("不应调用", nil)}}
	decorated := NewBudgetModel(raw, BudgetModelConfig{})

	b := budget.NewBudget(100, 10)
	b.ConsumeTokens(budget.TokenUsage{TotalTokens: 101}) // 超过上限，触发耗尽
	ctx := budget.WithBudget(context.Background(), b)

	input := []*schema.Message{schema.UserMessage("hi")}
	if _, err := decorated.Stream(ctx, input); !errors.Is(err, budget.ErrBudgetExhausted) {
		t.Fatalf("Stream 错误 = %v，期望 ErrBudgetExhausted", err)
	}
	if raw.call != 0 {
		t.Fatalf("预算耗尽后仍调用底层模型 %d 次", raw.call)
	}
}

// TestBudgetModelStreamRecordsCompaction 验证 Stream 路径压缩后向 Budget 记账。
func TestBudgetModelStreamRecordsCompaction(t *testing.T) {
	engine := ctxengine.NewEngine()
	engine.ModelContextWindow = 2000 // softLimit=1600
	engine.SetSpillStore(ctxengine.NewMemorySpillStore())

	raw := &scriptModel{replies: []*schema.Message{schema.AssistantMessage("ok", nil)}}
	decorated := NewBudgetModel(raw, BudgetModelConfig{Engine: engine})

	b := budget.NewBudget(100000, 100)
	ctx := budget.WithBudget(context.Background(), b)

	// 构造工具调用结果（可被压缩的内容）
	input := []*schema.Message{
		schema.UserMessage("start"),
		&schema.Message{
			Role: schema.Assistant,
			ToolCalls: []schema.ToolCall{{
				ID:       "call_1",
				Function: schema.FunctionCall{Name: "search"},
			}},
		},
		func() *schema.Message {
			m := schema.ToolMessage(strings.Repeat("result1", 500), "call_1")
			m.ToolName = "search"
			return m
		}(),
		&schema.Message{
			Role: schema.Assistant,
			ToolCalls: []schema.ToolCall{{
				ID:       "call_2",
				Function: schema.FunctionCall{Name: "search"},
			}},
		},
		func() *schema.Message {
			m := schema.ToolMessage(strings.Repeat("result2", 500), "call_2")
			m.ToolName = "search"
			return m
		}(),
		schema.UserMessage("continue"),
	}

	sr, err := decorated.Stream(ctx, input)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	// 消费流以完成调用
	for {
		_, recvErr := sr.Recv()
		if recvErr != nil {
			break
		}
	}
	sr.Close()

	if b.CompressEvents == 0 {
		t.Error("Stream 路径压缩后未向 Budget 记账")
	}
}

// TestNewAgentWithoutCatalogStillRegistersReadResult 验证无 Catalog 场景
// （向后兼容路径）依然接入溢出存储与回读工具，只是不做非幂等区分。
func TestNewAgentWithoutCatalogStillRegistersReadResult(t *testing.T) {
	ctx := context.Background()
	agent, err := NewAgent(ctx, &scene.SceneConfig{
		Key:   "plain",
		Model: &scriptModel{},
		Tools: []tool.BaseTool{&fakeReadTool{name: "search"}},
	}, session.NewMemoryStore())
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	names := assembledToolNames(t, agent.assembled)
	if !containsStr(names, "read_result") {
		t.Errorf("无 Catalog 时 read_result 也应注册，实际: %v", names)
	}
	// 未接线非幂等判定时，engine 应退化为「全部可淘汰」而非 panic
	if agent.engine.IsNonIdempotent != nil {
		t.Error("无 Catalog 时不应设置非幂等判定")
	}
}

// TestNewAgentInjectsSummaryModel 验证 SummaryModel 注入后引擎启用 LLM 摘要路径。
func TestNewAgentInjectsSummaryModel(t *testing.T) {
	ctx := context.Background()
	summaryModel := &scriptModel{}

	agent, err := NewAgent(ctx, &scene.SceneConfig{
		Key:          "with_summary",
		Model:        &scriptModel{},
		SummaryModel: summaryModel,
	}, session.NewMemoryStore())
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	if !agent.engine.HasSummaryModel() {
		t.Error("SummaryModel 已提供但引擎未注入，LLM 摘要路径不会启用")
	}
}

// TestNewAgentWithoutSummaryModelFallsBackToRule 验证未提供 SummaryModel 时
// 引擎降级为规则摘要（HasSummaryModel 返回 false）。
func TestNewAgentWithoutSummaryModelFallsBackToRule(t *testing.T) {
	ctx := context.Background()
	agent, err := NewAgent(ctx, &scene.SceneConfig{
		Key:   "without_summary",
		Model: &scriptModel{},
	}, session.NewMemoryStore())
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	if agent.engine.HasSummaryModel() {
		t.Error("未提供 SummaryModel 但引擎报告已注入")
	}
}
