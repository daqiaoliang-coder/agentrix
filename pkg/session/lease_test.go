package session

import (
	"context"
	"os"
	"testing"
	"time"
)

var (
	_ Leaser = (*MemoryLeaser)(nil)
	_ Leaser = (*MemoryStore)(nil)
	_ Leaser = (*MySQLStore)(nil)
)

func TestMemoryLeaserExclusionAndTakeover(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0)
	l := NewMemoryLeaser()
	l.now = func() time.Time { return now }

	if ok, _ := l.AcquireLease(ctx, "s", "a", time.Minute); !ok {
		t.Fatal("空闲租约应获取成功")
	}
	if ok, _ := l.AcquireLease(ctx, "s", "b", time.Minute); ok {
		t.Fatal("未过期租约不得被他人获取")
	}
	if ok, _ := l.AcquireLease(ctx, "other", "b", time.Minute); !ok {
		t.Fatal("不同会话的租约互不影响")
	}
	if ok, _ := l.RenewLease(ctx, "s", "b", time.Minute); ok {
		t.Fatal("非持有方不得续约")
	}

	now = now.Add(2 * time.Minute)
	if ok, _ := l.AcquireLease(ctx, "s", "b", time.Minute); !ok {
		t.Fatal("过期租约应可被接管")
	}
	if ok, _ := l.RenewLease(ctx, "s", "a", time.Minute); ok {
		t.Fatal("被接管后原持有方续约应失败")
	}
	_ = l.ReleaseLease(ctx, "s", "a")
	if ok, _ := l.AcquireLease(ctx, "s", "c", time.Minute); ok {
		t.Fatal("原持有方释放不得删除接管者的租约")
	}
	_ = l.ReleaseLease(ctx, "s", "b")
	if ok, _ := l.AcquireLease(ctx, "s", "c", time.Minute); !ok {
		t.Fatal("释放后租约应可再获取")
	}
}

// TestMySQLLease_RealDB 需要 AGENTRIX_TEST_MYSQL_DSN，且已执行 003_session_lease.sql。
func TestMySQLLease_RealDB(t *testing.T) {
	dsn := os.Getenv("AGENTRIX_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("AGENTRIX_TEST_MYSQL_DSN not set; skipping real-DB test")
	}
	store, err := NewMySQLStoreFromDSN(dsn)
	if err != nil {
		t.Fatalf("connect mysql: %v", err)
	}
	ctx := context.Background()
	const sid = "test_real_db_lease"
	cleanup := func() { _, _ = store.db.ExecContext(ctx, `DELETE FROM ai_session_lease WHERE session_id = ?`, sid) }
	cleanup()
	defer cleanup()

	if ok, err := store.AcquireLease(ctx, sid, "a", time.Minute); err != nil || !ok {
		t.Fatalf("acquire free lease: ok=%v err=%v", ok, err)
	}
	if ok, err := store.AcquireLease(ctx, sid, "b", time.Minute); err != nil || ok {
		t.Fatalf("acquire held lease: ok=%v err=%v", ok, err)
	}
	if ok, err := store.AcquireLease(ctx, sid, "a", time.Minute); err != nil || !ok {
		t.Fatalf("re-acquire by owner: ok=%v err=%v", ok, err)
	}
	if ok, err := store.RenewLease(ctx, sid, "b", time.Minute); err != nil || ok {
		t.Fatalf("renew by non-owner: ok=%v err=%v", ok, err)
	}

	if ok, err := store.AcquireLease(ctx, sid, "a", time.Millisecond); err != nil || !ok {
		t.Fatalf("shorten lease: ok=%v err=%v", ok, err)
	}
	time.Sleep(20 * time.Millisecond)
	if ok, err := store.AcquireLease(ctx, sid, "b", time.Minute); err != nil || !ok {
		t.Fatalf("take over expired lease: ok=%v err=%v", ok, err)
	}
	if ok, err := store.RenewLease(ctx, sid, "a", time.Minute); err != nil || ok {
		t.Fatalf("renew after takeover: ok=%v err=%v", ok, err)
	}
	if err := store.ReleaseLease(ctx, sid, "a"); err != nil {
		t.Fatalf("release by non-owner: %v", err)
	}
	if ok, _ := store.AcquireLease(ctx, sid, "c", time.Minute); ok {
		t.Fatal("non-owner release removed the lease")
	}
	if err := store.ReleaseLease(ctx, sid, "b"); err != nil {
		t.Fatalf("release by owner: %v", err)
	}
	if ok, err := store.AcquireLease(ctx, sid, "c", time.Minute); err != nil || !ok {
		t.Fatalf("acquire after release: ok=%v err=%v", ok, err)
	}
}
