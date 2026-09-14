package api

import (
	"encoding/json"
	"net/http"

	"github.com/daqiaoliang-coder/agentrix/internal/harness/core"
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

	output, err := agent.Run(r.Context(), req.Session, req.Input)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"content": output.Content})
}
