# Agentrix Makefile
# 用法:
#   make run-echo                                  # 用 Keychain 中的方舟 Key 跑 echo demo
#   make run-echo ARK_MODEL=doubao-seed-2-1-pro-260628  # 覆盖模型
#   make build / make vet / make tidy / make clean

# 方舟接入配置（可用 make 参数或环境变量覆盖）
ARK_MODEL ?= doubao-seed-2-0-mini-260428
KEYCHAIN_SERVICE ?= harness-doubao-api-key

.PHONY: run-echo build vet test tidy clean

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
