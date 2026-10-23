package projection

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// MySQLArtifactStore 是 ArtifactStore 的持久化实现，表结构见
// migrations/004_artifact.sql。跨进程重启与多副本共享同一份产物。
//
// 写入语义与 MemoryArtifactStore 一致：同 ID 重复 Save 覆盖更新，
// updated_at 由本实现刷新。
type MySQLArtifactStore struct {
	db *sql.DB
}

func NewMySQLArtifactStore(db *sql.DB) *MySQLArtifactStore {
	return &MySQLArtifactStore{db: db}
}

func (s *MySQLArtifactStore) Save(ctx context.Context, a *Artifact) error {
	meta, err := marshalMetadata(a.Metadata)
	if err != nil {
		return fmt.Errorf("save artifact %s: %w", a.ID, err)
	}
	now := time.Now()
	if a.CreatedAt.IsZero() {
		a.CreatedAt = now
	}
	a.UpdatedAt = now

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO ai_artifact (id, session_id, turn_id, type, title, content, metadata, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			session_id = VALUES(session_id),
			turn_id    = VALUES(turn_id),
			type       = VALUES(type),
			title      = VALUES(title),
			content    = VALUES(content),
			metadata   = VALUES(metadata),
			updated_at = VALUES(updated_at)`,
		a.ID, a.SessionID, a.TurnID, string(a.Type), a.Title,
		[]byte(a.Content), meta, a.CreatedAt, a.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("save artifact %s: %w", a.ID, err)
	}
	return nil
}

func (s *MySQLArtifactStore) Get(ctx context.Context, id string) (*Artifact, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, session_id, turn_id, type, title, content, metadata, created_at, updated_at
		FROM ai_artifact WHERE id = ?`, id)
	a, err := scanArtifact(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("artifact %s not found", id)
		}
		return nil, fmt.Errorf("get artifact %s: %w", id, err)
	}
	return a, nil
}

func (s *MySQLArtifactStore) ListBySession(ctx context.Context, sessionID string) ([]*Artifact, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, session_id, turn_id, type, title, content, metadata, created_at, updated_at
		FROM ai_artifact WHERE session_id = ? ORDER BY created_at ASC`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("list artifacts for %s: %w", sessionID, err)
	}
	defer rows.Close()

	out := make([]*Artifact, 0)
	for rows.Next() {
		a, err := scanArtifact(rows)
		if err != nil {
			return nil, fmt.Errorf("list artifacts for %s: %w", sessionID, err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list artifacts for %s: %w", sessionID, err)
	}
	return out, nil
}

func (s *MySQLArtifactStore) Delete(ctx context.Context, id string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM ai_artifact WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete artifact %s: %w", id, err)
	}
	return nil
}

// rowScanner 抽象 *sql.Row / *sql.Rows 的 Scan，复用同一列读取逻辑。
type rowScanner interface {
	Scan(dest ...any) error
}

func scanArtifact(row rowScanner) (*Artifact, error) {
	var (
		a       Artifact
		typ     string
		content []byte
		meta    []byte
	)
	if err := row.Scan(&a.ID, &a.SessionID, &a.TurnID, &typ, &a.Title,
		&content, &meta, &a.CreatedAt, &a.UpdatedAt); err != nil {
		return nil, err
	}
	a.Type = ArtifactType(typ)
	a.Content = json.RawMessage(content)
	if len(meta) > 0 {
		if err := json.Unmarshal(meta, &a.Metadata); err != nil {
			return nil, fmt.Errorf("decode metadata: %w", err)
		}
	}
	return &a, nil
}

func marshalMetadata(meta map[string]any) (any, error) {
	if len(meta) == 0 {
		return nil, nil
	}
	raw, err := json.Marshal(meta)
	if err != nil {
		return nil, fmt.Errorf("encode metadata: %w", err)
	}
	return raw, nil
}
