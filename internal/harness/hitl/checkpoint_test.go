package hitl

import (
	"context"
	"os"
	"testing"

	"github.com/daqiaoliang-coder/agentrix/internal/session"
)

// TestDefaultCheckPointStore_Injection 验证进程级共享存储的注入与回退：
// 注入持久化实现后 DefaultCheckPointStore 返回注入实例；未注入时回退内存实现。
func TestDefaultCheckPointStore_Injection(t *testing.T) {
	// 注入自定义实现
	custom := NewMemoryCheckPointStore()
	SetDefaultCheckPointStore(custom)
	if got := DefaultCheckPointStore(); got != CheckPointStore(custom) {
		t.Fatalf("注入后应返回注入实例，实际 %T", got)
	}

	// 注入实例真实可写读（进程级共享语义的直接体现）
	ctx := context.Background()
	if err := DefaultCheckPointStore().Set(ctx, "cp-1", []byte("state")); err != nil {
		t.Fatalf("set: %v", err)
	}
	data, ok, err := custom.Get(ctx, "cp-1")
	if err != nil || !ok || string(data) != "state" {
		t.Fatalf("经默认句柄写入的数据应能从注入实例读出: ok=%v data=%q err=%v", ok, data, err)
	}

	// 清理全局状态，避免影响同进程其他测试
	SetDefaultCheckPointStore(nil)
}

// TestMySQLCheckPointStore_RealDB 在提供 AGENTRIX_TEST_MYSQL_DSN 时跑真实 MySQL
// 端到端；未提供时跳过。前置：已执行 migrations/002_checkpoint.sql 建表。
//
// 运行方式：
//
//	AGENTRIX_TEST_MYSQL_DSN="root:pass@tcp(127.0.0.1:3306)/agentrix_test?parseTime=true&loc=Local" \
//	  go test ./internal/harness/hitl/ -run TestMySQLCheckPointStore_RealDB -v
func TestMySQLCheckPointStore_RealDB(t *testing.T) {
	dsn := os.Getenv("AGENTRIX_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("AGENTRIX_TEST_MYSQL_DSN not set; skipping real-DB test")
	}

	db, err := session.OpenMySQLDB(dsn)
	if err != nil {
		t.Fatalf("connect mysql: %v", err)
	}
	store := NewMySQLCheckPointStore(db)
	ctx := context.Background()
	const cpID = "test_real_db_checkpoint"

	cleanup := func() {
		if _, err := db.ExecContext(ctx, `DELETE FROM ai_checkpoint WHERE checkpoint_id = ?`, cpID); err != nil {
			t.Logf("cleanup: %v", err)
		}
	}
	cleanup()
	defer cleanup()

	// 未写入时 Get 返回 not-found
	if _, ok, err := store.Get(ctx, cpID); err != nil || ok {
		t.Fatalf("空表 Get 应返回 (nil,false,nil): ok=%v err=%v", ok, err)
	}

	// 写入 → 读取
	payload := []byte(`{"graph":"state-v1"}`)
	if err := store.Set(ctx, cpID, payload); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, ok, err := store.Get(ctx, cpID)
	if err != nil || !ok || string(got) != string(payload) {
		t.Fatalf("get 应返回写入内容: ok=%v got=%q err=%v", ok, got, err)
	}

	// 覆盖写 → 读取最新（同一会话重复中断只保留最新现场）
	payloadV2 := []byte(`{"graph":"state-v2"}`)
	if err := store.Set(ctx, cpID, payloadV2); err != nil {
		t.Fatalf("set v2: %v", err)
	}
	got, ok, err = store.Get(ctx, cpID)
	if err != nil || !ok || string(got) != string(payloadV2) {
		t.Fatalf("覆盖写后应读到最新内容: ok=%v got=%q err=%v", ok, got, err)
	}

	// Delete → 不可再读
	if err := store.Delete(ctx, cpID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, ok, err := store.Get(ctx, cpID); err != nil || ok {
		t.Fatalf("删除后 Get 应返回 not-found: ok=%v err=%v", ok, err)
	}
}
