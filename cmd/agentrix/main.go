package main

import (
	"log"
	"net/http"
	"os"

	"github.com/daqiaoliang-coder/agentrix/internal/api"
	"github.com/daqiaoliang-coder/agentrix/internal/harness/hitl"
	"github.com/daqiaoliang-coder/agentrix/internal/scene"
	"github.com/daqiaoliang-coder/agentrix/internal/session"
)

// 持久化选择策略：
//   - 设置了 AGENTRIX_MYSQL_DSN 时使用 MySQL 存储（双轨事件存储：
//     ai_raw_history append-only + ai_session_state CAS，以及
//     ai_checkpoint 审批检查点）；
//   - 否则回退到 MemoryStore（进程内，重启丢失，仅适合本地开发）。
func main() {
	registry := scene.NewRegistry()

	store := buildStore()

	// TODO: 在此注册 Scene
	// registry.Register(&scene.SceneConfig{...})

	handler := api.NewHandler(registry, store)
	mux := http.NewServeMux()
	mux.HandleFunc("/chat", handler.Chat)
	mux.HandleFunc("/chat/stream", handler.ChatStream)                     // SSE：实时推送过程信号
	mux.HandleFunc("GET /sessions/{session}/artifacts", handler.Artifacts) // 会话 artifact 列表

	log.Println("agentrix listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", mux))
}

// buildStore 根据环境变量选择 Store 实现。
// DSN 必须带 parseTime=true，否则 DATETIME 列无法扫描为 time.Time。
//
// MySQL 模式下同时把检查点存储切换为持久化实现：审批中断的现场落库，
// 进程重启或多副本部署后 Resume 仍能恢复；未设置 DSN 时保持进程内
// 默认（hitl.DefaultCheckPointStore 的内存回退）。
func buildStore() session.Store {
	dsn := os.Getenv("AGENTRIX_MYSQL_DSN")
	if dsn == "" {
		log.Println("[store] AGENTRIX_MYSQL_DSN not set, using in-memory store (data lost on restart)")
		return session.NewMemoryStore()
	}
	db, err := session.OpenMySQLDB(dsn)
	if err != nil {
		log.Fatalf("[store] connect mysql failed: %v", err)
	}
	// 与会话存储共享同一连接池，避免多份池开销
	hitl.SetDefaultCheckPointStore(hitl.NewMySQLCheckPointStore(db))
	log.Println("[store] using MySQL store (dual-track: raw_history + session_state; checkpoint: ai_checkpoint)")
	return session.NewMySQLStore(db)
}
