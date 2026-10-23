package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"sync"

	"github.com/cloudwego/eino/schema"

	"github.com/daqiaoliang-coder/agentrix/pkg/harness/core"
	"github.com/daqiaoliang-coder/agentrix/pkg/harness/hitl"
	"github.com/daqiaoliang-coder/agentrix/pkg/identity"
	"github.com/daqiaoliang-coder/agentrix/pkg/projection"
	"github.com/daqiaoliang-coder/agentrix/pkg/scene"
	"github.com/daqiaoliang-coder/agentrix/pkg/session"
)

// maxBodyBytes 是请求体上限。Turn 输入是文本，1MB 已远超合理范围，
// 限制后可在解码阶段挡住超大 body 导致的堆积。
const maxBodyBytes = 1 << 20

type Handler struct {
	registry *scene.Registry
	store    session.Store
	auth     *identity.Authenticator
	streams  *StreamTracker
}

// Option 覆盖 Handler 的默认依赖。
type Option func(*Handler)

// WithAuthenticator 注入 API Key 认证器。未注入时为匿名开发模式。
func WithAuthenticator(a *identity.Authenticator) Option {
	return func(h *Handler) { h.auth = a }
}

func NewHandler(registry *scene.Registry, store session.Store, opts ...Option) *Handler {
	h := &Handler{
		registry: registry,
		store:    store,
		auth:     identity.DevAuthenticator(),
		streams:  NewStreamTracker(),
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// DrainStreams 取消全部在途 SSE 流并拒绝新流。由 cmd 在优雅下线
// 宽限期结束后调用；可重复调用。
func (h *Handler) DrainStreams() {
	h.streams.Drain()
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
// 会话 ID 统一按认证主体加前缀，避免不同调用方互相读写对方会话。
func (h *Handler) runTurn(
	ctx context.Context,
	p identity.Principal,
	req *chatRequest,
	sink func(projection.Signal),
) (*schema.Message, error) {
	cfg, ok := h.registry.Get(req.Scene)
	if !ok {
		return nil, errSceneNotFound
	}
	agent, err := core.NewAgent(ctx, cfg, h.store)
	if err != nil {
		return nil, err
	}
	sessionID := p.Scope(req.Session)
	if req.Resume {
		if req.InterruptID == "" || req.Decision == nil {
			return nil, errResumeParams
		}
		// 操作者以认证身份为准，不信任请求体自报的 operator
		req.Decision.Operator = p.Operator()
		if sink != nil {
			return agent.ResumeStream(ctx, sessionID, req.InterruptID, req.Decision, sink)
		}
		return agent.Resume(ctx, sessionID, req.InterruptID, req.Decision)
	}
	if sink != nil {
		return agent.RunStream(ctx, sessionID, req.Input, sink)
	}
	return agent.Run(ctx, sessionID, req.Input)
}

// errSceneNotFound / errResumeParams 用哨兵错误区分 4xx 与 5xx。
var (
	errSceneNotFound = fmt.Errorf("scene not found")
	errResumeParams  = fmt.Errorf("resume requires interrupt_id and decision")
)

// decodeRequest 在 1MB 上限内解析请求体。
func decodeRequest(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	return json.NewDecoder(r.Body).Decode(dst)
}

// writeError 统一错误响应：4xx 透出具体原因便于调用方纠正；
// 5xx 只回通用文案，详情进服务端日志（带 request_id 便于串联），
// 避免泄露内部实现。
func writeError(ctx context.Context, w http.ResponseWriter, err error, status int) {
	if status >= http.StatusInternalServerError {
		log.Printf("[api] internal error (status %d, request_id=%s): %v", status, RequestID(ctx), err)
		http.Error(w, "internal server error", status)
		return
	}
	http.Error(w, err.Error(), status)
}

// errorMessage 与 writeError 同口径，供 SSE 事件使用。
func errorMessage(ctx context.Context, err error, status int) string {
	if status >= http.StatusInternalServerError {
		log.Printf("[api] internal error (status %d, request_id=%s): %v", status, RequestID(ctx), err)
		return "internal server error"
	}
	return err.Error()
}

// authenticate 校验请求身份；失败时已写出 401 响应。
func (h *Handler) authenticate(w http.ResponseWriter, r *http.Request) (identity.Principal, bool) {
	p, err := h.auth.Authenticate(r)
	if err != nil {
		writeError(r.Context(), w, err, http.StatusUnauthorized)
		return identity.Principal{}, false
	}
	return p, true
}

// statusOf 把执行错误映射为 HTTP 状态码：
//   - 401：缺少或无效的 API Key；
//   - 409：会话正被执行、审批已被认领、interrupt_id 不匹配、状态 CAS 冲突——
//     客户端应刷新会话状态后决定是否重试；
//   - 410：审批已终结或已放弃（检查点不存在）——不可再恢复，应发起新 Turn。
func statusOf(err error) int {
	switch {
	case errors.Is(err, identity.ErrUnauthorized):
		return http.StatusUnauthorized
	case errors.Is(err, errSceneNotFound):
		return http.StatusNotFound
	case errors.Is(err, errResumeParams):
		return http.StatusBadRequest
	case errors.Is(err, core.ErrSessionBusy),
		errors.Is(err, core.ErrSessionLeaseLost),
		errors.Is(err, core.ErrResumeInProgress),
		errors.Is(err, core.ErrInterruptMismatch),
		errors.Is(err, session.ErrCASConflict):
		return http.StatusConflict
	case errors.Is(err, core.ErrNoPendingApproval),
		errors.Is(err, core.ErrCheckpointNotFound):
		return http.StatusGone
	default:
		return http.StatusInternalServerError
	}
}

// statusOfDecode 把解码错误映射为状态码：超限返回 413，其余 400。
func statusOfDecode(err error) int {
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		return http.StatusRequestEntityTooLarge
	}
	return http.StatusBadRequest
}

func (h *Handler) Chat(w http.ResponseWriter, r *http.Request) {
	p, ok := h.authenticate(w, r)
	if !ok {
		return
	}
	var req chatRequest
	if err := decodeRequest(w, r, &req); err != nil {
		writeError(r.Context(), w, err, statusOfDecode(err))
		return
	}

	output, err := h.runTurn(r.Context(), p, &req, nil)
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
		writeError(r.Context(), w, err, statusOf(err))
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
	p, ok := h.authenticate(w, r)
	if !ok {
		return
	}
	var req chatRequest
	if err := decodeRequest(w, r, &req); err != nil {
		writeError(r.Context(), w, err, statusOfDecode(err))
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	// 登记在途流：优雅下线时经 ctx 取消，避免长连接把 Shutdown 拖到超时强断。
	streamCtx, release, err := h.streams.Track(r.Context())
	if err != nil {
		writeError(r.Context(), w, err, http.StatusServiceUnavailable)
		return
	}
	defer release()

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

	output, err := h.runTurn(streamCtx, p, &req, sink)
	if err != nil {
		if approval, isApproval := core.ExtractApprovalRequired(err); isApproval {
			writeEvent("approval_required", map[string]any{
				"interrupt_id": approval.InterruptID,
				"request":      approval.Request,
			})
			return
		}
		status := statusOf(err)
		writeEvent("error", map[string]any{
			"message": errorMessage(r.Context(), err, status),
			"status":  status,
		})
		return
	}
	writeEvent("result", map[string]string{"content": output.Content})
}

// Artifacts 处理 GET /sessions/{session}/artifacts：列举某会话产生的全部
// artifact。artifact 按 SessionID 归属，会话与场景无映射关系，故需跨
// 已注册场景的后端汇总。枚举范围以 Registry 中的场景为边界（而非包级
// 全局存储表），场景下线或未注册时其存储不会被无界持有。
// 输出按创建时间排序，保证分页 / diff 稳定。
func (h *Handler) Artifacts(w http.ResponseWriter, r *http.Request) {
	p, ok := h.authenticate(w, r)
	if !ok {
		return
	}
	sessionID := r.PathValue("session")
	if sessionID == "" {
		http.Error(w, "session is required", http.StatusBadRequest)
		return
	}
	sessionID = p.Scope(sessionID)
	out := make([]*projection.Artifact, 0)
	// 多个场景可能共享同一持久化存储实例，按指针去重避免重复枚举
	seen := make(map[projection.ArtifactStore]struct{})
	for _, cfg := range h.registry.List() {
		s := cfg.ArtifactStore
		if s == nil {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		arts, err := s.ListBySession(r.Context(), sessionID)
		if err != nil {
			log.Printf("[api] list artifacts for %s (scene %s): %v", sessionID, cfg.Key, err)
			continue
		}
		out = append(out, arts...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}
