package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/components/model"

	"github.com/daqiaoliang-coder/agentrix/pkg/api"
	ctxengine "github.com/daqiaoliang-coder/agentrix/pkg/context"
	"github.com/daqiaoliang-coder/agentrix/pkg/harness/hitl"
	"github.com/daqiaoliang-coder/agentrix/pkg/identity"
	"github.com/daqiaoliang-coder/agentrix/pkg/projection"
	"github.com/daqiaoliang-coder/agentrix/pkg/scene"
	"github.com/daqiaoliang-coder/agentrix/pkg/session"
	"github.com/daqiaoliang-coder/agentrix/pkg/telemetry"
	agenttool "github.com/daqiaoliang-coder/agentrix/pkg/tool"
	"github.com/daqiaoliang-coder/agentrix/pkg/tool/builtin"
)

const (
	addr              = ":8080"
	readHeaderTimeout = 10 * time.Second
	idleTimeout       = 120 * time.Second
	shutdownTimeout   = 30 * time.Second

	defaultScenesDir  = "./scenes"
	defaultArkBaseURL = "https://ark.cn-beijing.volces.com/api/v3"
)

// 持久化选择策略：
//   - 设置了 AGENTRIX_MYSQL_DSN 时使用 MySQL 存储（双轨事件存储：
//     ai_raw_history append-only + ai_session_state CAS，以及
//     ai_checkpoint 审批检查点）；
//   - 否则回退到 MemoryStore（进程内，重启丢失，仅适合本地开发）。
//
// 可观测导出：设置了 AGENTRIX_OTEL_ENDPOINT 时经 OTLP HTTP 上报
// trace（Turn/LLM/工具三级 span）与 metric（调用次数/Turn 时延/token
// 用量）；未设置时 Default() 为 nil，执行路径零开销。
//
// 身份：设置了 AGENTRIX_API_KEYS（key:userID[:tenant],...）时要求
// 请求携带 X-Agentrix-Key；未设置时为匿名开发模式。
func main() {
	registry := scene.NewRegistry()

	store, db := buildStore()
	tel := setupTelemetry()

	// 业务工具与命令目录由宿主进程统一注册；场景 YAML 只按白名单引用
	toolReg := agenttool.NewRegistry()
	toolReg.Register("echo", &builtin.EchoTool{})
	catalog := agenttool.NewCatalog()

	// MySQL 模式下 artifact / spill 与会话存储共享同一连接池；
	// 未设置 DSN 时留空，由运行时回退到进程内实现。
	var artifactStore projection.ArtifactStore
	var spillStore ctxengine.SpillStore
	if db != nil {
		artifactStore = projection.NewMySQLArtifactStore(db)
		spillStore = ctxengine.NewMySQLSpillStore(db)
	}

	loadScenes(registry, scene.NewLoader(scene.LoaderDeps{
		ModelFactory:  buildModelFactory(),
		Tools:         toolReg.List(),
		Catalog:       catalog,
		ArtifactStore: artifactStore,
		SpillStore:    spillStore,
	}))

	auth, err := identity.FromEnv()
	if err != nil {
		log.Fatalf("[auth] invalid AGENTRIX_API_KEYS: %v", err)
	}
	if !auth.Enabled() {
		log.Println("[auth] AGENTRIX_API_KEYS not set, authentication disabled (anonymous dev mode)")
	}

	// TODO: 在此注册 Scene
	// registry.Register(&scene.SceneConfig{...})

	handler := api.NewHandler(registry, store, api.WithAuthenticator(auth))
	mux := http.NewServeMux()
	mux.HandleFunc("/chat", handler.Chat)
	mux.HandleFunc("/chat/stream", handler.ChatStream)                     // SSE：实时推送过程信号
	mux.HandleFunc("GET /sessions/{session}/artifacts", handler.Artifacts) // 会话 artifact 列表
	mux.HandleFunc("GET /healthz", handleHealthz)
	mux.HandleFunc("GET /readyz", handleReadyz(store))

	srv := &http.Server{
		Addr:    addr,
		Handler: mux,
		// 慢速请求头攻击防护；SSE 是长连接，WriteTimeout 必须保持 0，
		// 否则长 Turn 会在固定时长后被强制断开。
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		log.Printf("agentrix listening on %s", addr)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("[server] listen failed: %v", err)
			_ = tel.Shutdown(context.Background())
			os.Exit(1)
		}
	case <-ctx.Done():
		log.Println("[server] shutdown signal received")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	// 先停止接收新请求并等待在途请求结束，再冲刷遥测数据
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("[server] graceful shutdown failed: %v", err)
		_ = srv.Close()
	}
	if err := tel.Shutdown(shutdownCtx); err != nil {
		log.Printf("[telemetry] shutdown failed: %v", err)
	}
	log.Println("[server] stopped")
}

func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "ok\n")
}

// handleReadyz 检查依赖可用性。Store 实现可选提供 Ping（MySQL 连接池探测）；
// 内存实现无外部依赖，直接视为就绪。
func handleReadyz(store session.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if p, ok := store.(interface{ Ping(context.Context) error }); ok {
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			defer cancel()
			if err := p.Ping(ctx); err != nil {
				log.Printf("[readyz] store ping failed: %v", err)
				http.Error(w, "not ready", http.StatusServiceUnavailable)
				return
			}
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
	}
}

// setupTelemetry 按环境变量装配 OTel 导出。装配失败降级为禁用并记日志，
// 可观测性故障不应阻断服务启动。
func setupTelemetry() *telemetry.Provider {
	tel, err := telemetry.SetupFromEnv(context.Background())
	if err != nil {
		log.Printf("[telemetry] setup failed, observability disabled: %v", err)
		return nil
	}
	if !tel.Enabled() {
		return nil
	}
	telemetry.SetDefault(tel)
	log.Println("[telemetry] OTLP export enabled (AGENTRIX_OTEL_ENDPOINT)")
	return tel
}

// buildStore 根据环境变量选择 Store 实现。
// DSN 必须带 parseTime=true，否则 DATETIME 列无法扫描为 time.Time。
//
// MySQL 模式下同时把检查点存储切换为持久化实现：审批中断的现场落库，
// 进程重启或多副本部署后 Resume 仍能恢复；未设置 DSN 时保持进程内
// 默认（hitl.DefaultCheckPointStore 的内存回退）。
//
// 会话执行租约由 Store 自身提供（MySQLStore 依赖 ai_session_lease，
// 见 migrations/003_session_lease.sql），多副本下 Run/Resume 同会话互斥。
func buildStore() (session.Store, *sql.DB) {
	dsn := os.Getenv("AGENTRIX_MYSQL_DSN")
	if dsn == "" {
		log.Println("[store] AGENTRIX_MYSQL_DSN not set, using in-memory store (data lost on restart)")
		return session.NewMemoryStore(), nil
	}
	db, err := session.OpenMySQLDB(dsn)
	if err != nil {
		log.Fatalf("[store] connect mysql failed: %v", err)
	}
	// 与会话存储共享同一连接池，避免多份池开销
	hitl.SetDefaultCheckPointStore(hitl.NewMySQLCheckPointStore(db))
	log.Println("[store] using MySQL store (dual-track: raw_history + session_state; checkpoint: ai_checkpoint)")
	return session.NewMySQLStore(db), db
}

// loadScenes 从 AGENTRIX_SCENES_DIR（默认 ./scenes）加载 YAML 场景。
// 目录不存在视为「未配置场景」，服务仍可启动（健康检查可用）；
// 文件内容错误则启动失败，避免带病上线。
func loadScenes(registry *scene.Registry, loader *scene.Loader) {
	dir := os.Getenv("AGENTRIX_SCENES_DIR")
	if dir == "" {
		dir = defaultScenesDir
	}
	if _, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			log.Printf("[scene] scenes dir %s not found, no scenes registered", dir)
			return
		}
		log.Fatalf("[scene] stat scenes dir %s: %v", dir, err)
	}
	cfgs, err := loader.LoadDir(context.Background(), dir)
	if err != nil {
		log.Fatalf("[scene] load scenes: %v", err)
	}
	for _, cfg := range cfgs {
		registry.Register(cfg)
		log.Printf("[scene] registered %s (%s)", cfg.Key, cfg.Name)
	}
}

// buildModelFactory 构造模型实例。凭据来源优先级：
// 场景声明的 api_key_env → ARK_API_KEY → OPENAI_API_KEY。
// 模型名优先取场景声明，缺省时回退 ARK_MODEL。
func buildModelFactory() scene.ModelFactory {
	return func(ctx context.Context, spec scene.ModelSpec) (model.ToolCallingChatModel, error) {
		provider := strings.ToLower(strings.TrimSpace(spec.Provider))
		if provider == "" {
			provider = "ark"
		}

		keyEnv := strings.TrimSpace(spec.APIKeyEnv)
		if keyEnv == "" {
			if provider == "openai" {
				keyEnv = "OPENAI_API_KEY"
			} else {
				keyEnv = "ARK_API_KEY"
			}
		}
		apiKey := os.Getenv(keyEnv)
		if apiKey == "" && keyEnv != "OPENAI_API_KEY" {
			apiKey = os.Getenv("OPENAI_API_KEY")
		}
		if apiKey == "" {
			return nil, fmt.Errorf("%s is not set", keyEnv)
		}

		name := strings.TrimSpace(spec.Name)
		if name == "" {
			name = os.Getenv("ARK_MODEL")
		}
		if name == "" {
			return nil, fmt.Errorf("model.name is required (or set ARK_MODEL)")
		}

		baseURL := strings.TrimSpace(spec.BaseURL)
		if baseURL == "" {
			if provider == "openai" {
				baseURL = os.Getenv("OPENAI_BASE_URL")
			} else {
				baseURL = os.Getenv("ARK_BASE_URL")
				if baseURL == "" {
					baseURL = defaultArkBaseURL
				}
			}
		}

		return openai.NewChatModel(ctx, &openai.ChatModelConfig{
			Model:   name,
			APIKey:  apiKey,
			BaseURL: baseURL,
		})
	}
}
