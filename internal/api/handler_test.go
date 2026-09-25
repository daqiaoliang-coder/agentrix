package api

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/daqiaoliang-coder/agentrix/internal/scene"
	"github.com/daqiaoliang-coder/agentrix/internal/session"
)

// fakeModel 返回固定回复的最小模型实现。
type fakeModel struct{}

func (fakeModel) Generate(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	return schema.AssistantMessage("pong", nil), nil
}

func (fakeModel) Stream(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	// 流式端点走 Stream 路径：以增量帧模拟 token 产出
	return schema.StreamReaderFromArray([]*schema.Message{
		schema.AssistantMessage("po", nil),
		schema.AssistantMessage("ng", nil),
	}), nil
}

func (fakeModel) WithTools(_ []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return fakeModel{}, nil
}

func newTestHandler(t *testing.T) *Handler {
	t.Helper()
	registry := scene.NewRegistry()
	registry.Register(&scene.SceneConfig{
		Key: "echo", Model: fakeModel{}, MaxIterations: 2,
	})
	return NewHandler(registry, session.NewMemoryStore())
}

// TestChatStreamSSE 验证 SSE 端点端到端：响应头、信号事件与最终结果事件。
func TestChatStreamSSE(t *testing.T) {
	h := newTestHandler(t)

	req := httptest.NewRequest("POST", "/chat/stream",
		strings.NewReader(`{"scene":"echo","session":"s1","input":"hi"}`))
	rec := httptest.NewRecorder()
	h.ChatStream(rec, req)

	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type 应为 text/event-stream，实际 %q", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"event: signal",
		`"type":"turn_start"`,
		`"type":"llm_requesting"`,
		`"type":"llm_token"`,
		`"delta":"po"`,
		`"type":"turn_end"`,
		"event: result",
		"pong",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("SSE 流缺少 %q，实际:\n%s", want, body)
		}
	}
	// 结果事件必须在信号之后到达
	if strings.Index(body, "event: result") < strings.Index(body, "event: signal") {
		t.Error("result 事件应在 signal 事件之后")
	}
}

// TestChatStreamSceneNotFound 验证未知场景经 SSE 返回 error 事件而非 HTTP 错误码
// （响应头已发出，只能以事件形式报告失败）。
func TestChatStreamSceneNotFound(t *testing.T) {
	h := newTestHandler(t)

	req := httptest.NewRequest("POST", "/chat/stream",
		strings.NewReader(`{"scene":"nope","session":"s1","input":"hi"}`))
	rec := httptest.NewRecorder()
	h.ChatStream(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "event: error") || !strings.Contains(body, "scene not found") {
		t.Errorf("未知场景应返回 error 事件，实际:\n%s", body)
	}
	if !strings.Contains(body, `"status":404`) {
		t.Errorf("error 事件应携带 404 状态，实际:\n%s", body)
	}
}

// TestChatSyncStillWorks 验证同步端点行为不受流式接线影响（含 404 语义）。
func TestChatSyncStillWorks(t *testing.T) {
	h := newTestHandler(t)

	req := httptest.NewRequest("POST", "/chat",
		strings.NewReader(`{"scene":"echo","session":"s1","input":"hi"}`))
	rec := httptest.NewRecorder()
	h.Chat(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "pong") {
		t.Errorf("同步端点异常: code=%d body=%s", rec.Code, rec.Body.String())
	}

	req404 := httptest.NewRequest("POST", "/chat",
		strings.NewReader(`{"scene":"nope","session":"s1","input":"hi"}`))
	rec404 := httptest.NewRecorder()
	h.Chat(rec404, req404)
	if rec404.Code != 404 {
		t.Errorf("未知场景应返回 404，实际 %d", rec404.Code)
	}
}
