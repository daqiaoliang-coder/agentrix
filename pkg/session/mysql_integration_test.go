//go:build integration

// MySQL 集成测试：验证 migrations 能完整落库，且会话 / artifact / spill
// 三类持久化实现在真实 MySQL 上读写一致。
//
// 运行方式（DSN 必须指向专用测试库，测试会写入并清理固定前缀的数据）：
//
//	AGENTRIX_TEST_MYSQL_DSN='user:pass@tcp(127.0.0.1:3306)/agentrix_test?parseTime=true' \
//	  go test -tags=integration ./pkg/session -run TestMySQL -v
//
// 未设置 DSN 时整组测试 skip，因此普通 `go test ./...` 不依赖 Docker/MySQL。
package session_test

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"

	ctxengine "github.com/daqiaoliang-coder/agentrix/pkg/context"
	"github.com/daqiaoliang-coder/agentrix/pkg/migrate"
	"github.com/daqiaoliang-coder/agentrix/pkg/projection"
	"github.com/daqiaoliang-coder/agentrix/pkg/session"

	"github.com/cloudwego/eino/schema"
)

// openTestDB 打开测试库；未设置 AGENTRIX_TEST_MYSQL_DSN 时跳过整组测试。
func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("AGENTRIX_TEST_MYSQL_DSN"))
	if dsn == "" {
		t.Skip("AGENTRIX_TEST_MYSQL_DSN not set, skipping MySQL integration test")
	}
	db, err := session.OpenMySQLDB(dsn)
	if err != nil {
		t.Fatalf("open mysql: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// TestMySQLMigrationsApply 验证全部迁移可重复执行且幂等。
func TestMySQLMigrationsApply(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	applied, err := migrate.Apply(ctx, db, "../../migrations")
	if err != nil {
		t.Fatalf("migrate apply: %v", err)
	}
	// 第二次必须全部跳过
	again, err := migrate.Apply(ctx, db, "../../migrations")
	if err != nil {
		t.Fatalf("migrate re-apply: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("重复执行应为空，实际应用 %v", again)
	}
	t.Logf("本次新应用迁移: %v", applied)
}

// TestMySQLSessionStore 覆盖状态 CAS 与 append-only 历史游标读取。
func TestMySQLSessionStore(t *testing.T) {
	db := openTestDB(t)
	store := session.NewMySQLStore(db)
	ctx := context.Background()
	sid := "itest-session-" + t.Name()

	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, "DELETE FROM ai_raw_history WHERE session_id = ?", sid)
		_, _ = db.ExecContext(ctx, "DELETE FROM ai_session_state WHERE session_id = ?", sid)
	})

	st, err := store.LoadState(ctx, sid)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	st.Memory.Summary = "集成测试记忆"
	if err := store.SaveState(ctx, sid, st); err != nil {
		t.Fatalf("save state: %v", err)
	}

	got, err := store.LoadState(ctx, sid)
	if err != nil {
		t.Fatalf("reload state: %v", err)
	}
	if got.Memory.Summary != "集成测试记忆" {
		t.Errorf("状态未持久化: %+v", got.Memory)
	}

	// 携带过期 version_lock 的写入应被 CAS 拒绝
	stale := *got
	stale.VersionLock = got.VersionLock - 1
	if err := store.SaveState(ctx, sid, &stale); err == nil {
		t.Error("过期 version_lock 应触发 CAS 冲突")
	}

	seq, err := store.AppendHistory(ctx, sid,
		schema.UserMessage("第一条"), schema.AssistantMessage("回复", nil))
	if err != nil {
		t.Fatalf("append history: %v", err)
	}
	if seq <= 0 {
		t.Fatalf("append 应返回正 seq，实际 %d", seq)
	}

	msgs, _, err := store.LoadHistoryFrom(ctx, sid, 0)
	if err != nil {
		t.Fatalf("load history from 0: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("应读到 2 条历史，实际 %d", len(msgs))
	}

	tail, last, err := store.LoadHistoryFrom(ctx, sid, seq)
	if err != nil {
		t.Fatalf("load history tail: %v", err)
	}
	if len(tail) != 0 || last != 0 {
		t.Errorf("游标已到末尾时应无新记录，实际 %d 条 last=%d", len(tail), last)
	}
}

// TestMySQLArtifactStore 验证 artifact 跨实例读写与按会话列举。
func TestMySQLArtifactStore(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	sid := "itest-art-" + t.Name()
	store := projection.NewMySQLArtifactStore(db)

	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, "DELETE FROM ai_artifact WHERE session_id = ?", sid)
	})

	art, err := projection.NewArtifact(sid, "turn_itest", projection.ArtifactPlan, "集成测试计划",
		map[string]any{"steps": []string{"构建", "回归"}})
	if err != nil {
		t.Fatalf("new artifact: %v", err)
	}
	if err := store.Save(ctx, art); err != nil {
		t.Fatalf("save artifact: %v", err)
	}

	// 新建实例读取，验证落库而非进程内缓存
	got, err := projection.NewMySQLArtifactStore(db).Get(ctx, art.ID)
	if err != nil {
		t.Fatalf("get artifact: %v", err)
	}
	if got.Title != "集成测试计划" || got.SessionID != sid {
		t.Errorf("artifact 元数据不一致: %+v", got)
	}

	arts, err := store.ListBySession(ctx, sid)
	if err != nil {
		t.Fatalf("list artifacts: %v", err)
	}
	if len(arts) != 1 {
		t.Fatalf("应列出 1 个 artifact，实际 %d", len(arts))
	}

	if err := store.Delete(ctx, art.ID); err != nil {
		t.Fatalf("delete artifact: %v", err)
	}
	if _, err := store.Get(ctx, art.ID); err == nil {
		t.Error("删除后不应再读到 artifact")
	}
}

// TestMySQLSpillStore 验证工具结果溢出存储的写入、覆盖与跨实例回读。
func TestMySQLSpillStore(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ref := "toolresult:itest-" + t.Name()
	store := ctxengine.NewMySQLSpillStore(db)

	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, "DELETE FROM ai_tool_spill WHERE ref = ?", ref)
	})

	if err := store.Spill(ctx, ref, "原始结果"); err != nil {
		t.Fatalf("spill: %v", err)
	}
	if err := store.Spill(ctx, ref, "覆盖结果"); err != nil {
		t.Fatalf("spill overwrite: %v", err)
	}

	got, err := ctxengine.NewMySQLSpillStore(db).Read(ctx, ref)
	if err != nil {
		t.Fatalf("read spill: %v", err)
	}
	if got != "覆盖结果" {
		t.Errorf("同 ref 重复写入应覆盖，实际 %q", got)
	}

	if _, err := store.Read(ctx, ref+"-missing"); err == nil {
		t.Error("不存在的 ref 应返回错误")
	}
}
