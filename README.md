# Agentrix

> A Go-native Agent Application Framework built on Eino Graph.

Agentrix 是一个产品级 Agent 应用框架，底层基于 [Eino Graph](https://github.com/cloudwego/eino)，
提供上下文管理、模型决策循环、工具执行、会话持久化、HITL、Signal/Artifact 等能力。

## 特性

- 基于 Eino Graph 的 ReAct 执行内核
- Scene + Config + Skill + Tool 业务接入
- Context Engine 上下文装配与四阶段压缩
- SessionState + RawHistory 双层记忆
- HITL 中断恢复
- Signal / Artifact 过程与产物投影

## 快速开始

```bash
go mod init github.com/daqiaoliang-coder/agentrix
go get github.com/cloudwego/eino
go run examples/echo_scene/main.go
```

License
MIT
