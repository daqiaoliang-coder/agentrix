package session

import (
	"context"
	"os"
	"testing"

	"github.com/cloudwego/eino/schema"
)

// 编译期断言：MySQLStore 必须实现 Store 接口。
var _ Store = (*MySQLStore)(nil)

// TestMySQLStore_RealDB 在提供 AGENTRIX_TEST_MYSQL_DSN 时跑真实 MySQL 端到端；
// 未提供时跳过，避免本地无 DB 环境下 CI 失败。
//
// 运行方式：
//
//	AGENTRIX_TEST_MYSQL_DSN="root:pass@tcp(127.0.0.1:3306)/agentrix_test?parseTime=true&loc=Local" \
//	  go test ./internal/session/ -run TestMySQLStore_RealDB -v
func TestMySQLStore_RealDB(t *testing.T) {
	dsn := os.Getenv("AGENTRIX_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("AGENTRIX_TEST_MYSQL_DSN not set; skipping real-DB test")
	}

	store, err := NewMySQLStoreFromDSN(dsn)
	if err != nil {
		t.Fatalf("connect mysql: %v", err)
	}
	ctx := context.Background()
	const sid = "test_real_db_session"

	// 清理残留（测试用表允许 DELETE；生产 ai_raw_history 禁止）。
	cleanupTestData(t, store, sid)
	defer cleanupTestData(t, store, sid)

	// ---- RawHistory：append-only + 游标 ----
	first := []*schema.Message{
		schema.UserMessage("查发布状态"),
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{
			ID:       "call_1",
			Function: schema.FunctionCall{Name: "check_release", Arguments: `{"env":"prod"}`},
		}}},
		{Role: schema.Tool, ToolCallID: "call_1", Content: `{"status":"ok"}`},
		{Role: schema.Assistant, Content: "发布正常"},
	}
	last, err := store.AppendHistory(ctx, sid, first...)
	if err != nil {
		t.Fatalf("append first batch: %v", err)
	}
	if last != 3 {
		t.Fatalf("last seq = %d, want 3", last)
	}

	// 全量回读：tool_calls / tool_call_id 往返保真
	all, _ := store.LoadHistory(ctx, sid)
	if len(all) != 4 {
		t.Fatalf("load history len = %d, want 4", len(all))
	}
	if len(all[1].ToolCalls) != 1 || all[1].ToolCalls[0].ID != "call_1" || all[1].ToolCalls[0].Function.Name != "check_release" {
		t.Fatalf("tool_calls round-trip broken: %+v", all[1].ToolCalls)
	}
	if all[2].ToolCallID != "call_1" {
		t.Fatalf("tool_call_id round-trip broken: %+v", all[2])
	}

	// 游标读取：seq > 2 只返回最后一条
	inc, last2, _ := store.LoadHistoryFrom(ctx, sid, 2)
	if len(inc) != 1 || last2 != 3 || inc[0].Content != "发布正常" {
		t.Fatalf("LoadHistoryFrom(2) = %v msgs / last %d, want 1 / 3", len(inc), last2)
	}

	// 第二批追加：seq 连续
	last, _ = store.AppendHistory(ctx, sid, schema.UserMessage("再来一轮"))
	if last != 4 {
		t.Fatalf("second last seq = %d, want 4", last)
	}
	inc, _, _ = store.LoadHistoryFrom(ctx, sid, 3)
	if len(inc) != 1 {
		t.Fatalf("incremental after second append = %d, want 1", len(inc))
	}

	// 无新记录：返回空 + 0
	inc, last2, _ = store.LoadHistoryFrom(ctx, sid, 4)
	if len(inc) != 0 || last2 != 0 {
		t.Fatalf("no-new-case = %v / %d, want empty / 0", inc, last2)
	}

	// ---- SessionState：CAS 乐观锁 ----
	// 首次保存（行不存在）
	st := NewState(sid)
	st.Memory.Summary = "第一轮摘要"
	st.HistoryCursor = 4
	if err := store.SaveState(ctx, sid, st); err != nil {
		t.Fatalf("save state (insert): %v", err)
	}
	loaded, _ := store.LoadState(ctx, sid)
	if loaded.Memory.Summary != "第一轮摘要" || loaded.HistoryCursor != 4 {
		t.Fatalf("load state mismatch: %+v", loaded)
	}
	lockAfterInsert := loaded.VersionLock

	// CAS 更新成功：version_lock 自增
	loaded.Memory.Summary = "第二轮摘要"
	loaded.HistoryCursor = 5
	if err := store.SaveState(ctx, sid, loaded); err != nil {
		t.Fatalf("save state (update): %v", err)
	}
	if loaded.VersionLock != lockAfterInsert+1 {
		t.Fatalf("version_lock not incremented: got %d, want %d", loaded.VersionLock, lockAfterInsert+1)
	}

	// CAS 冲突：用旧版本号保存应返回 ErrCASConflict
	old := NewState(sid)
	old.VersionLock = lockAfterInsert // 过期版本
	old.Memory.Summary = "脏写"
	err = store.SaveState(ctx, sid, old)
	if err != ErrCASConflict {
		t.Fatalf("expected ErrCASConflict, got %v", err)
	}
	// 冲突后数据库值不应被污染
	verify, _ := store.LoadState(ctx, sid)
	if verify.Memory.Summary != "第二轮摘要" {
		t.Fatalf("state corrupted after CAS conflict: %s", verify.Memory.Summary)
	}
}

// cleanupTestData 清理测试会话残留（仅测试用，生产 ai_raw_history 禁止 DELETE）。
func cleanupTestData(t *testing.T, store *MySQLStore, sid string) {
	t.Helper()
	ctx := context.Background()
	_, _ = store.db.ExecContext(ctx, `DELETE FROM ai_raw_history WHERE session_id = ?`, sid)
	_, _ = store.db.ExecContext(ctx, `DELETE FROM ai_session_state WHERE session_id = ?`, sid)
}
