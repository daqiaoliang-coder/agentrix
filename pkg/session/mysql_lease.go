package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// MySQLStore 的租约实现基于 ai_session_lease（见 migrations/003_session_lease.sql）。
// 过期判定统一用数据库 NOW(3)，不依赖各副本本地时钟一致。

// AcquireLease 以单条 upsert 原子地完成「空闲插入 / 过期接管 / 本 owner 重入」，
// 再回读持有者判定结果（不依赖 RowsAffected，其语义受 clientFoundRows 影响）。
//
// ON DUPLICATE KEY UPDATE 的赋值从左到右求值，expire_time 的条件读到的是
// 已更新后的 owner：仅当本次成功接管或重入时才刷新过期时间。
func (s *MySQLStore) AcquireLease(ctx context.Context, sessionID, owner string, ttl time.Duration) (bool, error) {
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO ai_session_lease (session_id, owner, expire_time)
		 VALUES (?, ?, NOW(3) + INTERVAL ? MICROSECOND)
		 ON DUPLICATE KEY UPDATE
			owner = IF(expire_time <= NOW(3) OR owner = VALUES(owner), VALUES(owner), owner),
			expire_time = IF(owner = VALUES(owner), VALUES(expire_time), expire_time)`,
		sessionID, owner, ttl.Microseconds(),
	); err != nil {
		return false, fmt.Errorf("acquire lease: %w", err)
	}
	return s.leaseHeldBy(ctx, sessionID, owner)
}

func (s *MySQLStore) RenewLease(ctx context.Context, sessionID, owner string, ttl time.Duration) (bool, error) {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE ai_session_lease SET expire_time = NOW(3) + INTERVAL ? MICROSECOND
		 WHERE session_id = ? AND owner = ?`,
		ttl.Microseconds(), sessionID, owner,
	); err != nil {
		return false, fmt.Errorf("renew lease: %w", err)
	}
	return s.leaseHeldBy(ctx, sessionID, owner)
}

func (s *MySQLStore) ReleaseLease(ctx context.Context, sessionID, owner string) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM ai_session_lease WHERE session_id = ? AND owner = ?`,
		sessionID, owner,
	); err != nil {
		return fmt.Errorf("release lease: %w", err)
	}
	return nil
}

func (s *MySQLStore) leaseHeldBy(ctx context.Context, sessionID, owner string) (bool, error) {
	var current string
	err := s.db.QueryRowContext(ctx,
		`SELECT owner FROM ai_session_lease WHERE session_id = ? AND expire_time > NOW(3)`,
		sessionID,
	).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read lease: %w", err)
	}
	return current == owner, nil
}
