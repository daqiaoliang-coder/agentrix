package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	"github.com/cloudwego/eino/schema"

	"github.com/daqiaoliang-coder/agentrix/internal/harness/core"
	"github.com/daqiaoliang-coder/agentrix/internal/harness/hitl"
	"github.com/daqiaoliang-coder/agentrix/internal/projection"
	"github.com/daqiaoliang-coder/agentrix/internal/scene"
	"github.com/daqiaoliang-coder/agentrix/internal/session"
)

type Handler struct {
	registry *scene.Registry
	store    session.Store
}

func NewHandler(registry *scene.Registry, store session.Store) *Handler {
	return &Handler{registry: registry, store: store}
}

// chatRequest 是 /chat 与 /chat/stream 共用的请求体。
type chatRequest struct {
	Scene   string `json:"scene"`
	Session string `json:"session"`
	Input   string `json:"input"`
	// 审批恢复字段：Resume 为 true 时按 InterruptID + Decision 恢复被中断的 Turn
	Resume      bool                   `json:"resume,omitempty"`
	InterruptID string                 `json:"interrupt_id,omitempty"`
	Decision    *hitl.ApprovalDecision `json:"decision,omitempty"`
}

// runTurn 按请求执行或恢复一个 Turn；sink 非 nil 时走流式变体推出过程信号。
func (h *Handler) runTurn(
	r *http.Request,
	req *chatRequest,
	sink func(projection.Signal),
) (*schema.Message, error) {
	cfg, ok := h.registry.Get(req.Scene)
	if !ok {
		return nil, errSceneNotFound
	}
	agent, err := core.NewAgent(r.Context(), cfg, h.store)
	if err != nil {
		return nil, err
	}
	if req.Resume {
		if req.InterruptID == "" || req.Decision == nil {
			return nil, errResumeParams
		}
		if sink != nil {
			return agent.ResumeStream(r.Context(), req.Session, req.InterruptID, req.Decision, sink)
		}
		return agent.Resume(r.Context(), req.Session, req.InterruptID, req.Decision)
	}
	if sink != nil {
		return agent.RunStream(r.Context(), req.Session, req.Input, sink)
	}
	return agent.Run(r.Context(), req.Session, req.Input)
}

// errSceneNotFound / errResumeParams 用哨兵错误区分 4xx 与 5xx。
var (
	errSceneNotFound = fmt.Errorf("scene not found")
	errResumeParams  = fmt.Errorf("resume requires interrupt_id and decision")
)

func statusOf(err error) int {
	switch err {
	case errSceneNotFound:
		return http.StatusNotFound
	case errResumeParams:
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

func (h *Handler) Chat(w http.ResponseWriter, r *http.Request) {
	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	output, err := h.runTurn(r, &req, nil)
	if err != nil {
		// 审批中断不是失败：返回 202 + 待审批信息，由前端发起人工授权后恢复
		if approval, isApproval := core.ExtractApprovalRequired(err); isApproval {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status":       "approval_required",
				"interrupt_id": approval.InterruptID,
				"request":      approval.Request,
			})
			return
		}
		http.Error(w, err.Error(), statusOf(err))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"content": output.Content})
}

// ChatStream 是 /chat 的 SSE 流式版本：Turn 执行期间实时推送事件。
//
// 事件序列：
//
//	event: signal          过程信号（turn_start / llm_requesting / tool_start / ...）
//	event: approval_required  写操作等待人工授权（随后客户端带 resume 字段重连恢复）
//	event: result          最终回复
//	event: error           失败
func (h *Handler) ChatStream(w http.ResponseWriter, r *http.Request) {
	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// 信号可能来自 ToolsNode 的并发工具回调，写响应须串行化
	var wmu sync.Mutex
	writeEvent := func(event string, v any) {
		data, err := json.Marshal(v)
		if err != nil {
			return
		}
		wmu.Lock()
		defer wmu.Unlock()
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
		flusher.Flush()
	}

	sink := func(sig projection.Signal) { writeEvent("signal", sig) }

	output, err := h.runTurn(r, &req, sink)
	if err != nil {
		if approval, isApproval := core.ExtractApprovalRequired(err); isApproval {
			writeEvent("approval_required", map[string]any{
				"interrupt_id": approval.InterruptID,
				"request":      approval.Request,
			})
			return
		}
		writeEvent("error", map[string]any{
			"message": err.Error(),
			"status":  statusOf(err),
		})
		return
	}
	writeEvent("result", map[string]string{"content": output.Content})
}
