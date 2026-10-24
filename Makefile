# Agentrix Makefile
# 用法:
#   make run-echo                                  # 用 Keychain 中的方舟 Key 跑 echo demo
#   make run-echo ARK_MODEL=doubao-seed-2-1-pro-260628  # 覆盖模型
#   make build / make vet / make tidy / make clean

# 方舟接入配置（可用 make 参数或环境变量覆盖）
ARK_MODEL ?= doubao-seed-2-0-mini-260428
KEYCHAIN_SERVICE ?= harness-doubao-api-key

# MySQL DSN：优先显式传入；否则在 recipe 执行时从 Keychain 读取，
# 避免 make -n 把密钥展开到终端。
MYSQL_KEYCHAIN_SERVICE ?= agentrix-mysql-dsn
MYSQL_DSN ?=
# 集成测试专用 DSN（必须指向可写的独立测试库，避免误删业务表）
TEST_MYSQL_DSN ?=

.PHONY: run-echo run-scene run-scene-mysql build vet test test-integration tidy clean migrate

run-scene: ## 用 YAML 场景启动服务（需 ARK_API_KEY / ARK_MODEL）
	AGENTRIX_SCENES_DIR=./examples/scenes go run ./cmd/agentrix

run-scene-mysql: ## 用 Keychain 中的 DSN 启动服务（MySQL 存储 + YAML 场景）
	@dsn="$(MYSQL_DSN)"; \
	if [ -z "$$dsn" ]; then dsn=$$(security find-generic-password -s "$(MYSQL_KEYCHAIN_SERVICE)" -w 2>/dev/null); fi; \
	AGENTRIX_MYSQL_DSN="$$dsn" \
	AGENTRIX_SCENES_DIR=./examples/scenes go run ./cmd/agentrix

test-integration: ## 运行 MySQL 集成测试（make test-integration TEST_MYSQL_DSN=...）
	AGENTRIX_TEST_MYSQL_DSN="$(TEST_MYSQL_DSN)" go test -tags=integration ./... -v

run-echo: ## 运行 echo_scene 示例（Key 从 macOS Keychain 动态读取，不落盘）
	ARK_API_KEY=$$(security find-generic-password -s "$(KEYCHAIN_SERVICE)" -w) \
	ARK_MODEL="$(ARK_MODEL)" \
	go run ./examples/echo_scene

build: ## 编译全部包
	go build ./...

vet: ## 静态检查
	go vet ./...

test: ## 运行测试
	go test ./...

tidy: ## 整理依赖
	go mod tidy

clean: ## 清理构建缓存
	go clean ./...

migrate: ## 应用 migrations 下的 SQL（默认读 Keychain DSN，可 make migrate MYSQL_DSN=...）
	@dsn="$(MYSQL_DSN)"; \
	if [ -z "$$dsn" ]; then dsn=$$(security find-generic-password -s "$(MYSQL_KEYCHAIN_SERVICE)" -w 2>/dev/null); fi; \
	AGENTRIX_MYSQL_DSN="$$dsn" go run ./cmd/migrate -dir ./migrations
