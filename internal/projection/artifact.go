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
