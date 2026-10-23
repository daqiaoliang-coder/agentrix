# Agentrix Makefile
# 用法:
#   make run-echo                                  # 用 Keychain 中的方舟 Key 跑 echo demo
#   make run-echo ARK_MODEL=doubao-seed-2-1-pro-260628  # 覆盖模型
#   make build / make vet / make tidy / make clean

# 方舟接入配置（可用 make 参数或环境变量覆盖）
ARK_MODEL ?= doubao-seed-2-0-mini-260428
KEYCHAIN_SERVICE ?= harness-doubao-api-key

# 迁移目标使用的 DSN（默认读环境变量 AGENTRIX_MYSQL_DSN）
MYSQL_DSN ?=
# 集成测试专用 DSN（指向可写的测试库）
TEST_MYSQL_DSN ?=

.PHONY: run-echo run-scene build vet test test-integration tidy clean migrate

run-scene: ## 用 YAML 场景启动服务（需 ARK_API_KEY / ARK_MODEL）
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

migrate: ## 应用 migrations 下的 SQL（make migrate MYSQL_DSN=...）
	go run ./cmd/migrate -dsn "$(MYSQL_DSN)" -dir ./migrations
