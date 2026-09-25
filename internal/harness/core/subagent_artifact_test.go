package core

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"

	"github.com/daqiaoliang-coder/agentrix/internal/projection"
	"github.com/daqiaoliang-coder/agentrix/internal/scene"
	"github.com/daqiaoliang-coder/agentrix/internal/session"
	"github.com/daqiaoliang-coder/agentrix/internal/tool/builtin"
)

// ---------- 子代理（spawn_<key>）----------

// TestSubagentRunsInIsolatedSession 验证子代理端到端链路：
// 父模型调用 spawn_scheduler → 子代理以独立会话同步执行 → 结果作为工具结果回给父模型。
func TestSubagentRunsInIsolatedSession(t *testing.T) {
	ctx := context.Background()
	store := session.NewMemoryStore()

	childModel := &scriptModel{replies: []*schema.Message{
		schema.AssistantMessage("子任务结论：三季度排期无冲突", nil),
	}}
	parentModel := &scriptModel{replies: []*schema.Message{
		toolCallMsg("spawn_scheduler", `{"task":"核对三季度排期"}`),
		schema.AssistantMessage("已让子代理核对，结论是无冲突", nil),
	}}

	agent, err := NewAgent(ctx, &scene.SceneConfig{
		Key: "parent", Model: parentModel, MaxIterations: 4,
		Subagents: map[string]*scene.SceneConfig{
			"scheduler": {
				Key: "scheduler-child", Name: "排期核对员",
				Model: childModel, MaxIterations: 2,
			},
		},
	}, store)
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	// 装配层：spawn_scheduler 必须对父模型可见
	if !containsStr(assembledToolNames(t, agent.assembled), "spawn_scheduler") {
		t.Fatalf("spawn_scheduler 未注册，实际工具: %v", assembledToolNames(t, agent.assembled))
	}

	out, err := agent.Run(ctx, "sess-parent", "帮我核对排期")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out == nil || out.Content != "已让子代理核对，结论是无冲突" {
		t.Fatalf("父最终回复不符合预期: %+v", out)
	}
	if childModel.call == 0 {
		t.Error("子模型未被调用")
	}

	// 子代理以独立会话执行：历史落在 父会话::key 下，首条为下发的任务
	childHist, err := store.LoadHistory(ctx, "sess-parent::scheduler")
	if err != nil {
		t.Fatalf("load child history: %v", err)
	}
	if len(childHist) == 0 || childHist[0].Role != schema.User || childHist[0].Content != "核对三季度排期" {
		t.Fatalf("子会话首条应为下发的任务，实际 %+v", childHist)
	}
	var childSawReply bool
	for _, m := range childHist {
		if strings.Contains(m.Content, "子任务结论") {
			childSawReply = true
		}
	}
	if !childSawReply {
		t.Error("子会话历史缺少子代理回复")
	}

	// 父会话只留一条工具结果（子代理内部展开不污染父历史）。
	// 按 ToolCallID 配对：ToolName 落库后不还原（存于 extra），不能作为查询依据。
	parentHist, err := store.LoadHistory(ctx, "sess-parent")
	if err != nil {
		t.Fatalf("load parent history: %v", err)
	}
	var toolResult *schema.Message
	for _, m := range parentHist {
		if m.Role == schema.Tool && m.ToolCallID == "call_spawn_scheduler" {
			toolResult = m
		}
	}
	if toolResult == nil {
		t.Fatal("父历史缺少 spawn_scheduler 工具结果")
	}
	if !strings.Contains(toolResult.Content, "子任务结论") {
		t.Errorf("工具结果应为子代理最终回复，实际 %q", toolResult.Content)
	}
}

// TestSubagentNoNestedSpawn 验证防递归：子场景自身声明的 Subagents 被忽略，
// 子代理的工具集中不得出现 spawn_*。
func TestSubagentNoNestedSpawn(t *testing.T) {
	ctx := context.Background()
	grandchild := &scene.SceneConfig{
		Key: "grandchild", Model: &scriptModel{}, MaxIterations: 1,
	}
	agent, err := NewAgent(ctx, &scene.SceneConfig{
		Key: "parent", Model: &scriptModel{}, MaxIterations: 1,
		Subagents: map[string]*scene.SceneConfig{
			"worker": {
				Key: "worker", Model: &scriptModel{}, MaxIterations: 1,
				Subagents: map[string]*scene.SceneConfig{"inner": grandchild},
			},
		},
	}, session.NewMemoryStore())
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	var spawnTool *subagentTool
	for _, tl := range agent.assembled.Tools {
		if st, ok := tl.(*subagentTool); ok {
			spawnTool = st
		}
	}
	if spawnTool == nil {
		t.Fatal("父装配中缺少 subagentTool")
	}
	for _, name := range assembledToolNames(t, spawnTool.assembled) {
		if strings.HasPrefix(name, "spawn_") {
			t.Errorf("子代理不应再持有派生工具，实际: %s", name)
		}
	}
}

// ---------- artifact 工具与信号 ----------

// writeArtifactCall 构造一次 write_artifact 工具调用消息。
func writeArtifactCall(id string) *schema.Message {
	args := `{"type":"plan","title":"发布计划","content":"{\"steps\":[\"构建\",\"回归\"]}"}`
	if id != "" {
		args = `{"id":"` + id + `","type":"plan","title":"发布计划v2","content":"{\"steps\":[\"灰度\"]}"}`
	}
	return toolCallMsg(builtin.ToolWriteArtifact, args)
}

// TestArtifactToolsWriteReadUpdate 验证 artifact 工具闭环：
// 写入归属当前会话/轮次、可按 ID 回读、带 ID 再写为更新且禁止跨会话篡改。
func TestArtifactToolsWriteReadUpdate(t *testing.T) {
	ctx := context.Background()
	artStore := projection.NewMemoryArtifactStore()

	// 模型脚本：写 artifact → 收尾。两轮 Run 模拟跨轮更新。
	m := &scriptModel{replies: []*schema.Message{
		writeArtifactCall(""),
		schema.AssistantMessage("已保存", nil),
	}}
	newAgent := func(model *scriptModel) *Agent {
		agent, err := NewAgent(ctx, &scene.SceneConfig{
			Key: "art", Model: model, ArtifactStore: artStore, MaxIterations: 3,
		}, session.NewMemoryStore())
		if err != nil {
			t.Fatalf("NewAgent: %v", err)
		}
		return agent
	}

	if _, err := newAgent(m).Run(ctx, "sess-art", "把发布计划存下来"); err != nil {
		t.Fatalf("Run(write): %v", err)
	}

	arts, err := artStore.ListBySession(ctx, "sess-art")
	if err != nil || len(arts) != 1 {
		t.Fatalf("应写入 1 个 artifact，实际 %d (err=%v)", len(arts), err)
	}
	art := arts[0]
	if art.SessionID != "sess-art" || art.TurnID == "" {
		t.Errorf("artifact 归属信息不正确: %+v", art)
	}
	if art.Type != projection.ArtifactPlan || art.Title != "发布计划" {
		t.Errorf("artifact 元数据不正确: %+v", art)
	}

	// read_artifact 回读（脱离 Agent 直接调用工具，read 不依赖 TurnScope）
	reader := builtin.NewReadArtifactTool(artStore)
	raw, err := reader.InvokableRun(ctx, `{"id":"`+art.ID+`"}`)
	if err != nil {
		t.Fatalf("read_artifact: %v", err)
	}
	if !strings.Contains(raw, "发布计划") || !strings.Contains(raw, "回归") {
		t.Errorf("回读内容不完整: %q", raw)
	}

	// 带 id 再写 = 更新
	m2 := &scriptModel{replies: []*schema.Message{
		writeArtifactCall(art.ID),
		schema.AssistantMessage("已更新", nil),
	}}
	if _, err := newAgent(m2).Run(ctx, "sess-art", "更新一下计划"); err != nil {
		t.Fatalf("Run(update): %v", err)
	}
	got, err := artStore.Get(ctx, art.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Title != "发布计划v2" || !strings.Contains(string(got.Content), "灰度") {
		t.Errorf("更新未生效: %+v", got)
	}
	if n, _ := artStore.ListBySession(ctx, "sess-art"); len(n) != 1 {
		t.Errorf("更新不应新增 artifact，实际 %d 个", len(n))
	}

	// 跨会话篡改防护：另一个会话的 Turn 不得更新该 artifact。
	// 工具返回错误会使整个 Turn 以工具失败收尾（与 read_skill 的参数错误一致），
	// 关键是存储内容未被改写。
	m3 := &scriptModel{replies: []*schema.Message{
		writeArtifactCall(art.ID),
		schema.AssistantMessage("不应发生", nil),
	}}
	if _, err := newAgent(m3).Run(ctx, "sess-other", "篡改计划"); err == nil {
		t.Error("跨会话更新应被拒绝（Turn 报错）")
	} else if !strings.Contains(err.Error(), "belongs to another session") {
		t.Errorf("错误应指明跨会话拒绝，实际: %v", err)
	}
	got2, _ := artStore.Get(ctx, art.ID)
	if got2.Title != "发布计划v2" {
		t.Errorf("跨会话更新不应改写内容，实际标题 %q", got2.Title)
	}
}

// TestArtifactSignalAndWhitelist 验证：
//  1. write_artifact 成功后发射 artifact_new 信号（携带 artifact_id/title）；
//  2. artifact 框架工具不受业务命令白名单过滤（与 read_skill 同等待遇）。
func TestArtifactSignalAndWhitelist(t *testing.T) {
	ctx := context.Background()
	artStore := projection.NewMemoryArtifactStore()
	catalog := wbsCatalog(t)

	m := &scriptModel{replies: []*schema.Message{
		writeArtifactCall(""),
		schema.AssistantMessage("完成", nil),
	}}
	agent, err := NewAgent(ctx, &scene.SceneConfig{
		Key: "art-wl", Model: m, ArtifactStore: artStore,
		Tools:   []tool.BaseTool{&fakeReadTool{name: "list_draft"}},
		Catalog: catalog,
		// 白名单只放行读命令：业务写工具被过滤，但框架 artifact 工具必须保留
		AllowedCommands: []string{"wbs-arrangement list-draft"},
		MaxIterations:   3,
	}, session.NewMemoryStore())
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	names := assembledToolNames(t, agent.assembled)
	for _, want := range []string{builtin.ToolWriteArtifact, builtin.ToolReadArtifact} {
		if !containsStr(names, want) {
			t.Fatalf("框架工具 %s 被白名单误过滤，实际: %v", want, names)
		}
	}

	rec := &signalRecorder{}
	if _, err := agent.RunStream(ctx, "sess-art-sig", "存个计划", rec.sink()); err != nil {
		t.Fatalf("RunStream: %v", err)
	}

	var found *projection.Signal
	for i, s := range rec.sigs {
		if s.Type == projection.ArtifactNew {
			found = &rec.sigs[i]
		}
	}
	if found == nil {
		t.Fatalf("缺少 artifact_new 信号，实际序列 %v", rec.types())
	}
	payload, _ := found.Payload.(map[string]any)
	if payload["title"] != "发布计划" || payload["artifact_id"] == "" {
		t.Errorf("artifact_new 载荷不正确: %v", payload)
	}
	if found.ThreadID != "sess-art-sig" {
		t.Errorf("信号归属会话不正确: %q", found.ThreadID)
	}

	// 同步路径（sink=nil）也应正常写入 artifact，仅不发射信号
	m2 := &scriptModel{replies: []*schema.Message{
		writeArtifactCall(""),
		schema.AssistantMessage("ok", nil),
	}}
	agent2, err := NewAgent(ctx, &scene.SceneConfig{
		Key: "art-sync", Model: m2, ArtifactStore: artStore, MaxIterations: 3,
	}, session.NewMemoryStore())
	if err != nil {
		t.Fatalf("NewAgent(sync): %v", err)
	}
	if _, err := agent2.Run(ctx, "sess-art-sync", "存"); err != nil {
		t.Fatalf("Run(sync): %v", err)
	}
	if n, _ := artStore.ListBySession(ctx, "sess-art-sync"); len(n) != 1 {
		t.Errorf("同步路径应写入 1 个 artifact，实际 %d", len(n))
	}
}

// TestWriteArtifactToolArgValidation 直接验证工具参数校验（不经图）：
// 非法 type / 非 JSON content / 缺 TurnScope 均应报错而非静默成功。
func TestWriteArtifactToolArgValidation(t *testing.T) {
	ctx := projection.WithTurnScope(context.Background(), "sess-v", "turn_v")
	w := builtin.NewWriteArtifactTool(projection.NewMemoryArtifactStore())

	if _, err := w.InvokableRun(ctx, `{"type":"bogus","title":"t","content":"{}"}`); err == nil {
		t.Error("非法 type 应报错")
	}
	if _, err := w.InvokableRun(ctx, `{"type":"text","title":"t","content":"not-json"}`); err == nil {
		t.Error("非 JSON content 应报错")
	}
	if _, err := w.InvokableRun(context.Background(), `{"type":"text","title":"t","content":"{}"}`); err == nil {
		t.Error("缺少 TurnScope 应报错")
	}
	// content 为 JSON 字符串字面量也应合法（纯文本场景）
	if _, err := w.InvokableRun(ctx, `{"type":"text","title":"t","content":"\"纯文本\""}`); err != nil {
		t.Errorf("JSON 字符串字面量 content 应合法: %v", err)
	}
}

// 确保 writeArtifactResult 契约字段与 signalCallback 解析保持一致：
// 若工具返回字段改名而信号端未同步，本测试会在信号断言处失败。
func TestArtifactResultContract(t *testing.T) {
	ctx := projection.WithTurnScope(context.Background(), "sess-c", "turn_c")
	store := projection.NewMemoryArtifactStore()
	w := builtin.NewWriteArtifactTool(store)
	raw, err := w.InvokableRun(ctx, `{"type":"json","title":"契约","content":"{}"}`)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	var res struct {
		ArtifactID string `json:"artifact_id"`
		Type       string `json:"type"`
		Title      string `json:"title"`
		Updated    bool   `json:"updated"`
	}
	if err := json.Unmarshal([]byte(raw), &res); err != nil {
		t.Fatalf("工具结果必须是 JSON（信号端按此解析）: %v", err)
	}
	if res.ArtifactID == "" || res.Type != "json" || res.Title != "契约" || res.Updated {
		t.Errorf("契约字段不符合预期: %+v", res)
	}
}
