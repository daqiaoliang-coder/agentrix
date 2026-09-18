package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cloudwego/eino/schema"
	_ "github.com/go-sql-driver/mysql"
)

// ErrCASConflict 表示 SaveState 时版本号不匹配。
// 触发场景：并发更新同一会话，或游标已被其他进程推进。
// 调用方应重新 LoadState 后重试，而非直接覆盖。
var ErrCASConflict = errors.New("session state CAS conflict: version_lock mismatch")

// MySQLStore 是 Store 的 MySQL 实现，承载双轨事件存储：
//   - ai_raw_history   ：append-only 审计事件流（只增不改不删）
//   - ai_session_state ：每会话单行工作记忆，version_lock 乐观锁
//
// 两表通过 state.history_cursor 关联：游标指向已消费的最大 raw seq。
type MySQLStore struct {
	db *sql.DB
}

// NewMySQLStore 注入已有的 *sql.DB（便于复用连接池 / 测试注入 mock）。
func NewMySQLStore(db *sql.DB) *MySQLStore {
	return &MySQLStore{db: db}
}

// NewMySQLStoreFromDSN 从 DSN 创建 MySQLStore，并完成连接池初始化与连通性探测。
// DSN 格式参考 go-sql-driver/mysql：
//
//	user:pass@tcp(127.0.0.1:3306)/dbname?parseTime=true&loc=Local
//
// parseTime=true 是必须的，否则 DATETIME 列无法扫描为 time.Time。
func NewMySQLStoreFromDSN(dsn string) (*MySQLStore, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("open mysql: %w", err)
	}
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping mysql: %w", err)
	}
	return &MySQLStore{db: db}, nil
}

// ---------------------------------------------------------------------------
// RawHistory：append-only 事件流
// ---------------------------------------------------------------------------

// AppendHistory 批量追加消息到 ai_raw_history，返回最后一条记录的 seq。
//
// 并发安全：在事务内用 `SELECT MAX(seq) ... FOR UPDATE` 锁定该会话的现有行
// （含间隙锁），使同一会话的并发追加串行化，保证 seq 单调连续。
//
// 幂等性：ai_raw_history 上有唯一键 (session_id, seq)，若出现重放同一段消息
// 的异常路径，INSERT 会因唯一键冲突失败而非产生重复行。
func (s *MySQLStore) AppendHistory(ctx context.Context, sessionID string, msgs ...*schema.Message) (int64, error) {
	if len(msgs) == 0 {
		return 0, nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 取当前最大 seq 并加锁；-1 表示尚无记录，下一条 seq 从 0 开始。
	var maxSeq sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(seq), -1) FROM ai_raw_history WHERE session_id = ? FOR UPDATE`,
		sessionID,
	).Scan(&maxSeq); err != nil {
		return 0, fmt.Errorf("get max seq: %w", err)
	}
	nextSeq := maxSeq.Int64 + 1

	records := messagesToRecords(sessionID, msgs, nextSeq, time.Now())
	if len(records) == 0 {
		_ = tx.Commit()
		return maxSeq.Int64, nil
	}

	placeholders := make([]string, len(records))
	args := make([]any, 0, len(records)*7)
	for i, r := range records {
		placeholders[i] = "(?, ?, ?, ?, ?, ?, ?)"
		args = append(args,
			r.SessionID, r.Seq, r.Role, r.Content,
			mustJSON(r.ToolCalls), nullableString(r.ToolCallID), mustJSON(r.Extra),
		)
	}

	query := `INSERT INTO ai_raw_history (session_id, seq, role, content, tool_calls, tool_call_id, extra) VALUES ` +
		strings.Join(placeholders, ",")

	if _, err := tx.ExecContext(ctx, query, args...); err != nil {
		return 0, fmt.Errorf("insert raw history: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return records[len(records)-1].Seq, nil
}

// LoadHistory 全量加载某会话的 RawHistory（按 seq 升序）。
func (s *MySQLStore) LoadHistory(ctx context.Context, sessionID string) ([]*schema.Message, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT role, content, tool_calls, tool_call_id FROM ai_raw_history WHERE session_id = ? ORDER BY seq ASC`,
		sessionID,
	)
	if err != nil {
		return nil, fmt.Errorf("load history: %w", err)
	}
	defer rows.Close()
	return scanHistoryRows(rows)
}

// LoadHistoryFrom 游标范围读取：返回 seq > afterSeq 的记录及其中最大的 seq。
// 无新记录时返回空列表与 0，调用方据此不推进游标。
func (s *MySQLStore) LoadHistoryFrom(ctx context.Context, sessionID string, afterSeq int64) ([]*schema.Message, int64, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT role, content, tool_calls, tool_call_id FROM ai_raw_history WHERE session_id = ? AND seq > ? ORDER BY seq ASC`,
		sessionID, afterSeq,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("load history from: %w", err)
	}
	defer rows.Close()

	msgs, err := scanHistoryRows(rows)
	if err != nil {
		return nil, 0, err
	}
	if len(msgs) == 0 {
		return nil, 0, nil
	}
	// 取最大 seq：rows 按 seq 升序，最后一条即为最大。
	// 重新查一次避免在 scan 时维护额外状态。
	var maxSeq int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT MAX(seq) FROM ai_raw_history WHERE session_id = ? AND seq > ?`,
		sessionID, afterSeq,
	).Scan(&maxSeq); err != nil {
		return nil, 0, fmt.Errorf("get max seq: %w", err)
	}
	return msgs, maxSeq, nil
}

// scanHistoryRows 把 ai_raw_history 行还原为消息序列。
func scanHistoryRows(rows *sql.Rows) ([]*schema.Message, error) {
	var msgs []*schema.Message
	for rows.Next() {
		var role, content string
		var toolCallsJSON, toolCallID sql.NullString
		if err := rows.Scan(&role, &content, &toolCallsJSON, &toolCallID); err != nil {
			return nil, fmt.Errorf("scan history row: %w", err)
		}
		msg := &schema.Message{
			Role:    schema.RoleType(role),
			Content: content,
		}
		if toolCallsJSON.Valid && toolCallsJSON.String != "null" {
			var tcs []ToolCallRecord
			if err := json.Unmarshal([]byte(toolCallsJSON.String), &tcs); err == nil {
				for _, tc := range tcs {
					msg.ToolCalls = append(msg.ToolCalls, schema.ToolCall{
						ID:       tc.ID,
						Function: schema.FunctionCall{Name: tc.Name, Arguments: tc.Arguments},
					})
				}
			}
		}
		if toolCallID.Valid {
			msg.ToolCallID = toolCallID.String
		}
		msgs = append(msgs, msg)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return msgs, nil
}

// ---------------------------------------------------------------------------
// SessionState：单行 + CAS 乐观锁
// ---------------------------------------------------------------------------

// LoadState 读取会话工作记忆；无记录时返回带默认值的新状态。
func (s *MySQLStore) LoadState(ctx context.Context, sessionID string) (*State, error) {
	var version, memoryJSON, skillRuntimesJSON, todoJSON, reminderJSON, hitlStateJSON string
	var historyCursor, versionLock int64
	var updateTime time.Time

	err := s.db.QueryRowContext(ctx,
		`SELECT version, history_cursor, memory, skill_runtimes, todo, reminder, hitl_state, version_lock, update_time
		 FROM ai_session_state WHERE session_id = ?`,
		sessionID,
	).Scan(&version, &historyCursor, &memoryJSON, &skillRuntimesJSON, &todoJSON, &reminderJSON, &hitlStateJSON, &versionLock, &updateTime)
	if errors.Is(err, sql.ErrNoRows) {
		return NewState(sessionID), nil
	}
	if err != nil {
		return nil, fmt.Errorf("load state: %w", err)
	}

	state := &State{
		Version:       version,
		SessionID:     sessionID,
		HistoryCursor: int(historyCursor),
		VersionLock:   versionLock,
		UpdateTime:    updateTime,
	}
	// JSON 列反序列化失败不致命：该字段退化为零值，下一轮会被覆盖。
	_ = json.Unmarshal([]byte(memoryJSON), &state.Memory)
	_ = json.Unmarshal([]byte(skillRuntimesJSON), &state.SkillRuntimes)
	_ = json.Unmarshal([]byte(todoJSON), &state.Todo)
	_ = json.Unmarshal([]byte(reminderJSON), &state.Reminder)
	_ = json.Unmarshal([]byte(hitlStateJSON), &state.HitlState)

	if state.Version == "" {
		state.Version = SchemaVersionV1
	}
	return state, nil
}

// SaveState 以 CAS 乐观锁方式保存会话状态。
//
// 流程：
//  1. UPDATE ... WHERE version_lock = ?  —— 行存在且版本匹配时成功，affected=1；
//  2. affected=0 时尝试 INSERT ... ON DUPLICATE KEY UPDATE session_id=session_id：
//     - 行不存在 → 插入成功（affected=1）；
//     - 行已存在但版本不符 → no-op 更新（affected=0），返回 ErrCASConflict。
//
// 成功后本地 state.VersionLock 自增，与数据库保持一致。
func (s *MySQLStore) SaveState(ctx context.Context, sessionID string, state *State) error {
	state.SessionID = sessionID
	if state.Version == "" {
		state.Version = SchemaVersionV1
	}

	result, err := s.db.ExecContext(ctx,
		`UPDATE ai_session_state SET
			history_cursor = ?, memory = ?, skill_runtimes = ?, todo = ?, reminder = ?, hitl_state = ?,
			version_lock = version_lock + 1
		 WHERE session_id = ? AND version_lock = ?`,
		state.HistoryCursor,
		mustJSON(state.Memory),
		mustJSON(state.SkillRuntimes),
		mustJSON(state.Todo),
		mustJSON(state.Reminder),
		mustJSON(state.HitlState),
		sessionID,
		state.VersionLock,
	)
	if err != nil {
		return fmt.Errorf("update state: %w", err)
	}
	affected, _ := result.RowsAffected()
	if affected > 0 {
		state.VersionLock++
		return nil
	}

	// 行不存在或版本不符：尝试首次插入。
	result, err = s.db.ExecContext(ctx,
		`INSERT INTO ai_session_state
			(session_id, version, history_cursor, memory, skill_runtimes, todo, reminder, hitl_state, version_lock)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON DUPLICATE KEY UPDATE session_id = session_id`,
		sessionID,
		state.Version,
		state.HistoryCursor,
		mustJSON(state.Memory),
		mustJSON(state.SkillRuntimes),
		mustJSON(state.Todo),
		mustJSON(state.Reminder),
		mustJSON(state.HitlState),
		state.VersionLock,
	)
	if err != nil {
		return fmt.Errorf("insert state: %w", err)
	}
	affected, _ = result.RowsAffected()
	if affected == 0 {
		// 行已存在但 ON DUPLICATE 未更新 → 真实的版本冲突。
		return ErrCASConflict
	}
	return nil
}

// ---------------------------------------------------------------------------
// 序列化辅助
// ---------------------------------------------------------------------------

// mustJSON 将任意值序列化为 JSON 字符串；nil 输出 "null"。
// 用于 MySQL JSON 列：始终传入合法 JSON，避免 SQL 层校验失败。
func mustJSON(v any) any {
	if v == nil {
		return "null"
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "null"
	}
	return string(b)
}

// nullableString 把空字符串转为 SQL NULL，避免空 tool_call_id 被存为 ""。
func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
