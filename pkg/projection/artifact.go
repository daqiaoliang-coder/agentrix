package projection

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

type ArtifactType string

const (
	ArtifactText   ArtifactType = "text"
	ArtifactTable  ArtifactType = "table"
	ArtifactJSON   ArtifactType = "json"
	ArtifactPlan   ArtifactType = "plan"
	ArtifactChart  ArtifactType = "chart"
	ArtifactCustom ArtifactType = "custom"
)

type Artifact struct {
	ID        string          `json:"id"`
	SessionID string          `json:"session_id"`
	TurnID    string          `json:"turn_id"`
	Type      ArtifactType    `json:"type"`
	Title     string          `json:"title"`
	Content   json.RawMessage `json:"content"`
	Metadata  map[string]any  `json:"metadata,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

func NewArtifact(sessionID, turnID string, typ ArtifactType, title string, content any) (*Artifact, error) {
	raw, err := json.Marshal(content)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	return &Artifact{
		ID:        generateID(),
		SessionID: sessionID,
		TurnID:    turnID,
		Type:      typ,
		Title:     title,
		Content:   raw,
		CreatedAt: now,
		UpdatedAt: now,
	}, nil
}

func (a *Artifact) Unmarshal(v any) error {
	return json.Unmarshal(a.Content, v)
}

type ArtifactStore interface {
	Save(ctx context.Context, artifact *Artifact) error
	Get(ctx context.Context, id string) (*Artifact, error)
	ListBySession(ctx context.Context, sessionID string) ([]*Artifact, error)
	Delete(ctx context.Context, id string) error
}

type MemoryArtifactStore struct {
	mu        sync.RWMutex
	artifacts map[string]*Artifact
}

func NewMemoryArtifactStore() *MemoryArtifactStore {
	return &MemoryArtifactStore{artifacts: make(map[string]*Artifact)}
}

func (s *MemoryArtifactStore) Save(_ context.Context, artifact *Artifact) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	artifact.UpdatedAt = time.Now()
	s.artifacts[artifact.ID] = artifact
	return nil
}

func (s *MemoryArtifactStore) Get(_ context.Context, id string) (*Artifact, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.artifacts[id]
	if !ok {
		return nil, fmt.Errorf("artifact %s not found", id)
	}
	return a, nil
}

func (s *MemoryArtifactStore) ListBySession(_ context.Context, sessionID string) ([]*Artifact, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []*Artifact
	for _, a := range s.artifacts {
		if a.SessionID == sessionID {
			result = append(result, a)
		}
	}
	return result, nil
}

func (s *MemoryArtifactStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.artifacts, id)
	return nil
}

var idCounter int64

func generateID() string {
	return fmt.Sprintf("art_%d_%d", time.Now().UnixNano(), atomic.AddInt64(&idCounter, 1))
}

// turnScopeKey 是 TurnScope 的 context key，未导出以避免业务侧误注入。
type turnScopeKey struct{}

// TurnScope 携带本次 Turn 的归属信息，供框架工具（write_artifact / spawn_*）
// 在回调深处取回会话与轮次标识。由 Agent.run 注入，业务代码不应直接写入。
type TurnScope struct {
	SessionID string
	TurnID    string
}

// WithTurnScope 把 Turn 归属信息注入 ctx。工具经图回调拿到的 ctx 继承自
// Runnable 的调用 ctx，故此处注入对图内全部工具可见。
func WithTurnScope(ctx context.Context, sessionID, turnID string) context.Context {
	return context.WithValue(ctx, turnScopeKey{}, TurnScope{SessionID: sessionID, TurnID: turnID})
}

// TurnScopeFrom 取出 Turn 归属信息；未注入（如工具脱离 Agent 直接调用）时 ok=false。
func TurnScopeFrom(ctx context.Context) (TurnScope, bool) {
	scope, ok := ctx.Value(turnScopeKey{}).(TurnScope)
	return scope, ok
}
