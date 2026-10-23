# Agentrix

A Go-native Agent Application Framework built on Eino.

Agentrix 是一个产品级 Agent 应用框架，底层基于 Eino，
提供上下文管理、ReAct、工具执行、会话持久化、HITL等能力。

## 特性

- 基于 Eino 的 ReAct 执行内核
- Scene + Config + Skill + Tool 业务接入
- Context Engine 上下文装配与四阶段压缩
- SessionState + RawHistory 双层记忆
- HITL 中断恢复
- Signal / Artifact 过程与产物投影
- YAML 场景声明 + 技能目录加载（`SKILL.md` + `references/`）
- API Key 身份隔离、请求级 `X-Request-Id`、优雅下线（SSE 排空）
- MySQL 持久化（会话双轨 / 审批检查点 / artifact / spill）与幂等迁移工具
- 离线 Agent 评测：真实 SceneConfig 接入、版本化任务集、多次运行统计与 JSONL 报告（[使用指南](docs/evaluation.md)）

## 快速开始

```bash
# 零配置本地启动：内存存储 + 匿名模式，健康检查可用
go run ./cmd/agentrix

# 带 YAML 场景启动（echo 示例，需 ARK_API_KEY / ARK_MODEL）
export ARK_API_KEY=... ARK_MODEL=...
make run-scene

# 完整链路：MySQL + 身份 + 场景
export AGENTRIX_MYSQL_DSN='user:pass@tcp(127.0.0.1:3306)/agentrix?parseTime=true&loc=Local'
export AGENTRIX_API_KEYS='secret:alice:tenant-a'
make migrate MYSQL_DSN="$AGENTRIX_MYSQL_DSN"
AGENTRIX_SCENES_DIR=./examples/scenes go run ./cmd/agentrix
```

主要环境变量：

| 变量 | 说明 |
|---|---|
| `AGENTRIX_MYSQL_DSN` | MySQL DSN；未设置时回退内存存储 |
| `AGENTRIX_API_KEYS` | `key:userID[:tenant],...`；未设置时为匿名开发模式 |
| `AGENTRIX_SCENES_DIR` | YAML 场景目录；显式指定但不存在时启动失败 |
| `ARK_API_KEY` / `ARK_MODEL` / `ARK_BASE_URL` | 模型凭据与接入点 |
| `AGENTRIX_OTEL_ENDPOINT` | OTLP HTTP 端点，启用 trace/metric 导出 |

测试：

```bash
make test              # 单元测试（不依赖外部服务）
make test-integration TEST_MYSQL_DSN='...'   # MySQL 集成测试
```

## License

MIT
