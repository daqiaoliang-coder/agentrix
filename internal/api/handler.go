package api

import (
	"encoding/json"
	"net/http"

	"github.com/cloudwego/eino/schema"

	"github.com/daqiaoliang-coder/agentrix/internal/harness/core"
	"github.com/daqiaoliang-coder/agentrix/internal/harness/hitl"
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

func (h *Handler) Chat(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Scene   string `json:"scene"`
		Session string `json:"session"`
		Input   string `json:"input"`
		// 审批恢复字段：Resume 为 true 时按 InterruptID + Decision 恢复被中断的 Turn
		Resume      bool                   `json:"resume,omitempty"`
		InterruptID string                 `json:"interrupt_id,omitempty"`
		Decision    *hitl.ApprovalDecision `json:"decision,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	cfg, ok := h.registry.Get(req.Scene)
	if !ok {
		http.Error(w, "scene not found", http.StatusNotFound)
		return
	}

	agent, err := core.NewAgent(r.Context(), cfg, h.store)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var output *schema.Message
	if req.Resume {
		if req.InterruptID == "" || req.Decision == nil {
			http.Error(w, "resume requires interrupt_id and decision", http.StatusBadRequest)
			return
		}
		output, err = agent.Resume(r.Context(), req.Session, req.InterruptID, req.Decision)
	} else {
		output, err = agent.Run(r.Context(), req.Session, req.Input)
	}
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
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"content": output.Content})
}
