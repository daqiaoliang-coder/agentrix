package main

import (
	"log"
	"net/http"
	"os"

	"github.com/daqiaoliang-coder/agentrix/internal/api"
	"github.com/daqiaoliang-coder/agentrix/internal/scene"
	"github.com/daqiaoliang-coder/agentrix/internal/session"
)

// 持久化选择策略：
//   - 设置了 AGENTRIX_MYSQL_DSN 时使用 MySQLStore（双轨事件存储：
//     ai_raw_history append-only + ai_session_state CAS）；
//   - 否则回退到 MemoryStore（进程内，重启丢失，仅适合本地开发）。
func main() {
	registry := scene.NewRegistry()

	store := buildStore()

	// TODO: 在此注册 Scene
	// registry.Register(&scene.SceneConfig{...})

	handler := api.NewHandler(registry, store)
	mux := http.NewServeMux()
	mux.HandleFunc("/chat", handler.Chat)

	log.Println("agentrix listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", mux))
}

// buildStore 根据环境变量选择 Store 实现。
// DSN 必须带 parseTime=true，否则 DATETIME 列无法扫描为 time.Time。
func buildStore() session.Store {
	dsn := os.Getenv("AGENTRIX_MYSQL_DSN")
	if dsn == "" {
		log.Println("[store] AGENTRIX_MYSQL_DSN not set, using in-memory store (data lost on restart)")
		return session.NewMemoryStore()
	}
	ms, err := session.NewMySQLStoreFromDSN(dsn)
	if err != nil {
		log.Fatalf("[store] connect mysql failed: %v", err)
	}
	log.Println("[store] using MySQL store (dual-track: raw_history + session_state)")
	return ms
}
