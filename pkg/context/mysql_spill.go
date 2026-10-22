package context

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// MySQLSpillStore 是 SpillStore 的持久化实现，表结构见
// migrations/005_spill.sql。作用域从「单次 Turn」扩展到进程外：
// 压缩淘汰的工具结果在后续 Turn 中仍可经 read_result 回读。
type MySQLSpillStore struct {
	db *sql.DB
}

func NewMySQLSpillStore(db *sql.DB) *MySQLSpillStore {
	return &MySQLSpillStore{db: db}
}

func (s *MySQLSpillStore) Spill(ctx context.Context, ref string, content string) error {
	if ref == "" {
		return fmt.Errorf("spill: empty ref")
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO ai_tool_spill (ref, content, created_at, updated_at)
		VALUES (?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE content = VALUES(content), updated_at = VALUES(updated_at)`,
		ref, content, time.Now(), time.Now(),
	)
	if err != nil {
		return fmt.Errorf("spill %s: %w", ref, err)
	}
	return nil
}

func (s *MySQLSpillStore) Read(ctx context.Context, ref string) (string, error) {
	var content string
	err := s.db.QueryRowContext(ctx, `SELECT content FROM ai_tool_spill WHERE ref = ?`, ref).Scan(&content)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("spill: ref %s not found", ref)
	}
	if err != nil {
		return "", fmt.Errorf("read spill %s: %w", ref, err)
	}
	return content, nil
}
