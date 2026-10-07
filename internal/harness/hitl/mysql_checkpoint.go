package hitl

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	_ "github.com/go-sql-driver/mysql"
)

// MySQLCheckPointStore 是 CheckPointStore 的 MySQL 实现。
//
// 检查点内容是 eino 序列化后的完整图状态（审批中断现场），只按
// checkpoint_id（= sessionID）整存整取，框架不解析其内部结构，
// 因此用 BLOB 原样落库，不做 JSON 建模。
//
// 与内存版的差异：进程重启、多副本部署后 Resume 仍能命中检查点——
// 这是审批流跨进程恢复的前提。
type MySQLCheckPointStore struct {
	db *sql.DB
}

// 编译期断言：必须实现 eino 的检查点存取接口与可选的删除接口。
var (
	_ CheckPointStore   = (*MySQLCheckPointStore)(nil)
	_ CheckPointDeleter = (*MySQLCheckPointStore)(nil)
)

// NewMySQLCheckPointStore 注入已有的 *sql.DB，与 session.MySQLStore 共享连接池。
func NewMySQLCheckPointStore(db *sql.DB) *MySQLCheckPointStore {
	return &MySQLCheckPointStore{db: db}
}

// Get 按 checkpoint_id 取出图状态；不存在时返回 (nil, false, nil)。
func (s *MySQLCheckPointStore) Get(ctx context.Context, checkPointID string) ([]byte, bool, error) {
	var data []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT checkpoint FROM ai_checkpoint WHERE checkpoint_id = ?`,
		checkPointID,
	).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("get checkpoint: %w", err)
	}
	return data, true, nil
}

// Set 覆盖式写入：同一 checkpoint_id 重复中断时保留最新现场。
// eino 在图的各关键节点都会写检查点，最后一次写入才是可恢复点。
func (s *MySQLCheckPointStore) Set(ctx context.Context, checkPointID string, checkPoint []byte) error {
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO ai_checkpoint (checkpoint_id, checkpoint) VALUES (?, ?)
		 ON DUPLICATE KEY UPDATE checkpoint = VALUES(checkpoint)`,
		checkPointID, checkPoint,
	); err != nil {
		return fmt.Errorf("set checkpoint: %w", err)
	}
	return nil
}

// Delete 显式删除检查点。compose Graph 不会自动调用它，调用方须在运行终态
// 已持久化后清理残留现场。
func (s *MySQLCheckPointStore) Delete(ctx context.Context, checkPointID string) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM ai_checkpoint WHERE checkpoint_id = ?`,
		checkPointID,
	); err != nil {
		return fmt.Errorf("delete checkpoint: %w", err)
	}
	return nil
}
