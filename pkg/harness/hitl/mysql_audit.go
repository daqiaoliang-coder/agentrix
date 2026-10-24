package hitl

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// MySQLApprovalAuditStore 是审批审计的持久化实现，表结构见
// migrations/006_approval_audit.sql。
//
// 与会话/检查点共享连接池；记录以 interrupt_id 为主键，同一审批重复写入
// 按覆盖处理，保证 at-most-once 恢复路径下审计不重复。
type MySQLApprovalAuditStore struct {
	db *sql.DB
}

var _ ApprovalAuditStore = (*MySQLApprovalAuditStore)(nil)

func NewMySQLApprovalAuditStore(db *sql.DB) *MySQLApprovalAuditStore {
	return &MySQLApprovalAuditStore{db: db}
}

func (s *MySQLApprovalAuditStore) RecordRequest(ctx context.Context, a *ApprovalAudit) error {
	if a == nil || a.InterruptID == "" {
		return ErrApprovalAuditMissingInterruptID
	}
	requestedAt := a.RequestedAt
	if requestedAt.IsZero() {
		requestedAt = time.Now()
	}
	status := a.Status
	if status == "" {
		status = ApprovalPending
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO ai_approval_audit
			(interrupt_id, scene, session_id, turn_id, tool_name, tool_call_id,
			 arguments, reason, status, operator, comment, requested_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, '', '', ?)
		ON DUPLICATE KEY UPDATE
			scene       = VALUES(scene),
			session_id  = VALUES(session_id),
			turn_id     = VALUES(turn_id),
			tool_name   = VALUES(tool_name),
			tool_call_id= VALUES(tool_call_id),
			arguments   = VALUES(arguments),
			reason      = VALUES(reason),
			status      = VALUES(status),
			requested_at= VALUES(requested_at)`,
		a.InterruptID, a.Scene, a.SessionID, a.TurnID, a.ToolName, a.ToolCallID,
		a.Arguments, a.Reason, status, requestedAt,
	)
	if err != nil {
		return fmt.Errorf("record approval request %s: %w", a.InterruptID, err)
	}
	return nil
}

func (s *MySQLApprovalAuditStore) RecordDecision(
	ctx context.Context,
	interruptID, operator, comment string,
	approved bool,
) error {
	if interruptID == "" {
		return ErrApprovalAuditMissingInterruptID
	}
	status := ApprovalRejected
	if approved {
		status = ApprovalApproved
	}
	// 升级场景下中断先于审计表存在：无 pending 行时补一条最小记录，
	// 保证决策本身仍可追溯，而不是让 Resume 失败。
	now := time.Now()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO ai_approval_audit
			(interrupt_id, status, operator, comment, requested_at, decided_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			status     = VALUES(status),
			operator   = VALUES(operator),
			comment    = VALUES(comment),
			decided_at = VALUES(decided_at)`,
		interruptID, status, operator, comment, now, now,
	)
	if err != nil {
		return fmt.Errorf("record approval decision %s: %w", interruptID, err)
	}
	return nil
}

func (s *MySQLApprovalAuditStore) MarkAbandoned(ctx context.Context, interruptID string) error {
	if interruptID == "" {
		return ErrApprovalAuditMissingInterruptID
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE ai_approval_audit
		SET status = ?, decided_at = ?
		WHERE interrupt_id = ? AND status = ?`,
		ApprovalAbandoned, time.Now(), interruptID, ApprovalPending,
	)
	if err != nil {
		return fmt.Errorf("mark approval abandoned %s: %w", interruptID, err)
	}
	return nil
}
