# Agent 评测系统

Agentrix 的 `internal/eval` 包提供两类离线评测：

1. **确定性框架回归**：用 `ScriptModel` 固定模型每轮输出，检查工具调用、HITL 审批、白名单、信号时序等。
2. **业务 Agent 评测**：用 `Runner.ConfigFactory` 为任务装配真实的 prompts / skills / tools / model，并用断言和业务状态 Oracle 检查结果。

两种模式共享任务、报告、重复运行统计和 JSONL 导出。业务侧只需要维护一个任务集文件，以及一个把 `scene` 映射到自己业务配置的工厂。

## 任务集格式

任务集是一个版本化 JSON 文件。提示词、模型密钥和工具实现不放入数据文件；它们由应用代码中的 `AgentConfigFactory` 装配。

```json
{
  "name": "echo-agent",
  "version": "1",
  "description": "检查 Echo Agent 的工具调用和用户回复",
  "cases": [
    {
      "id": "echo-hello",
      "scene": "echo_scene",
      "input": "请用 echo 工具回显：Hello Agentrix",
      "tags": ["tool-use", "smoke"],
      "checks": [
        { "type": "no_error" },
        { "type": "tool_called", "tool": "echo" },
        { "type": "final_contains", "value": "Hello Agentrix" },
        { "type": "max_tool_calls", "limit": 2 }
      ]
    }
  ]
}
```

可用声明式检查：

| 类型 | 参数 | 检查内容 |
|---|---|---|
| `no_error` | 无 | Agent 执行没有错误 |
| `final_equals` | `value` | 最终文本完全一致 |
| `final_contains` | `value` | 最终文本包含片段 |
| `final_not_contains` | `value` | 最终文本不包含片段 |
| `tool_called` / `tool_not_called` | `tool` | 指定工具有/没有被调用 |
| `tool_call_count` | `tool`, `limit` | 指定工具恰好调用 N 次 |
| `tool_args_contains` | `tool`, `value` | 工具参数包含片段 |
| `tool_result_contains` | `tool`, `value` | 工具结果包含片段 |
| `approval_required` | 可选 `tool` | Agent 停在审批中断 |
| `max_tool_calls` / `min_tool_calls` | `limit` | 全部工具调用次数上限/下限 |

解析器会拒绝未知 JSON 字段、重复任务 ID 和不完整检查，避免数据文件的小错误静默改变评测含义。

## 接入真实业务 Agent

业务应用已有的 Scene 构造逻辑应由 `ConfigFactory` 复用。工厂每个 trial 调用一次，所以可以为每次评测新建模拟数据库、工具实例和模型客户端，并把 Oracle 绑定到该 trial 的环境。

```go
datasetFile, err := os.Open("eval/echo.json")
if err != nil {
    return err
}
defer datasetFile.Close()

dataset, err := eval.LoadDataset(datasetFile)
if err != nil {
    return err
}
suite, err := dataset.ToSuite()
if err != nil {
    return err
}

runner := &eval.Runner{
    ConfigFactory: func(ctx context.Context, tc eval.Case, attempt int) (eval.AgentSetup, error) {
        // 此函数由业务应用实现：按 tc.SceneKey 选择自己的场景定义，
        // 并用同一份 prompt、skills、tools 构建 SceneConfig。
        // 多次运行时应构造独立工具与模拟环境，避免 trial 间状态串扰。
        return buildEvalAgent(ctx, tc.SceneKey, attempt)
    },
}

result := runner.RunSuite(ctx, suite, eval.RunOptions{
    Repeat:      3,
    Parallelism: 1,
})
fmt.Print(result.Summary())

if err := result.WriteJSONL(os.Stdout); err != nil {
    return err
}
if !result.OK() {
    return errors.New("Agent evaluation failed")
}
```

`buildEvalAgent` 由宿主应用实现，并返回本次 trial 使用的 Agentrix 配置及可选业务状态 Oracle：

```go
func buildEvalAgent(ctx context.Context, sceneKey string, attempt int) (eval.AgentSetup, error) {
    env := newIsolatedTestEnvironment()
    cfg, err := buildBusinessScene(ctx, sceneKey, env) // 复用业务的 prompt / skill / tool 定义
    if err != nil {
        return eval.AgentSetup{}, err
    }

    return eval.AgentSetup{
        Config: cfg,
        Metadata: map[string]string{
            "model": "provider/model@version",
            "prompt": "git:abc123",
            "skills": "v3",
            "tools": "2026-10-05",
        },
        Verify: func(ctx context.Context, report eval.Report) error {
            // 检查数据库或模拟环境最终状态，而不只看模型的回复。
            return env.AssertGoalState(ctx)
        },
    }, nil
}
```

如果状态是只读的，也可以直接用 `Case.Oracle`；需要按 trial 新建环境时，把 `Verify` 放入 `AgentSetup`，这样最终状态检查会和对应的独立环境绑定。

## 运行统计和产物

`RunOptions` 默认顺序执行、每项运行一次。提高 `Repeat` 可评估随机模型的稳定性；`Parallelism` 可加速执行。并行或重复评测时，推荐由 `ConfigFactory` 创建全新的工具和环境实例。静态 `AgentConfig` 会浅拷贝配置结构，但不会克隆模型或工具对象。

`SuiteReport.Stats` 提供：

- 运行成功率及 95% 置信区间：单次运行使用 Wilson 区间；重复运行按任务做 cluster bootstrap，避免把同一任务的多次运行误当成独立样本；
- `pass@k`：每个任务多次尝试中至少成功一次的任务占比；
- `pass^k`：每次尝试都成功的任务占比；
- 按任务 `tags` 拆分的结果；
- 平均运行时间、工具调用数，以及模型调用和 token 汇总。

`SuiteReport.WriteJSONL` 为每次运行写一行 JSON，可用于 CI 和结果仓库。默认导出信号类型而不导出工具参数或完整结果，降低敏感信息意外落盘的风险；Agent 最终回答仍可能包含业务内容，写入共享目录或制品库前应按业务要求脱敏。完整信号只留在内存中的 `Report.Signals`。

应在 `AgentSetup.Metadata` 中记录模型快照、prompt/skills/tools 版本或哈希；这些标识会和每次运行结果一起导出。不要把密钥或 prompt 正文放入 metadata。

建议 CI 在改动 prompts / skills / tools 后运行快速回归集，在发布前运行更完整的分层任务与多次重复评测。评测门禁应单独关注高风险任务与约束违反，不能仅用一个全量平均分代替。
