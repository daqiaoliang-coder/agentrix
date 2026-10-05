package eval

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"

	"github.com/daqiaoliang-coder/agentrix/internal/harness/core"
	"github.com/daqiaoliang-coder/agentrix/internal/projection"
	"github.com/daqiaoliang-coder/agentrix/internal/scene"
	"github.com/daqiaoliang-coder/agentrix/internal/session"
)

// maxResumeRounds 限制单用例自动恢复次数，防止脚本与决策配置失误造成死循环。
const maxResumeRounds = 8

// Oracle 检查 Agent 运行后的业务状态，适合验证数据库、文件或外部模拟环境。
type Oracle func(context.Context, Report) error

// AgentSetup 把一次 trial 的完整 Agent 配置与对应环境的状态 Oracle 绑定，
// 确保重复运行时每条结果都由自己的环境实例检查。
type AgentSetup struct {
	Config   *scene.SceneConfig
	Verify   Oracle
	Metadata map[string]string
}

// AgentConfigFactory 为每个 trial 创建业务 Agent 的完整配置与可选 Oracle。
// 重复或并行评测时应创建独立的模型、工具和环境实例，避免运行状态串扰。
type AgentConfigFactory func(context.Context, Case, int) (AgentSetup, error)

// Runner 执行评测用例。零值即可用。
type Runner struct {
	// Store 非 nil 时所有运行共享该存储，session ID 保证唯一；
	// nil 时每次运行使用独立 MemoryStore。
	Store session.Store

	// ConfigFactory 用于把数据集中的 SceneKey 映射到真实业务场景。
	// 非 nil 时每个 trial 调用一次；可据此构建自己的 prompts / skills / tools。
	ConfigFactory AgentConfigFactory

	runSeq atomic.Uint64
}

// RunMetrics 是从 Agentrix 的实际运行信号中汇总出的可观察用量。
type RunMetrics struct {
	ToolCalls        int   `json:"tool_calls"`
	ModelCalls       int   `json:"model_calls"`
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
}

// Report 是单条 trial 的运行结果、行为信号和断言结论。
type Report struct {
	RunID         string
	CaseIndex     int
	Attempt       int
	CaseName      string
	SceneKey      string
	Tags          []string
	AgentMetadata map[string]string
	StartedAt     time.Time
	Duration      time.Duration
	Metrics       RunMetrics
	Model         *ScriptModel `json:"-"`
	Output        *schema.Message
	Err           error `json:"-"`
	// RunError 是 Err 的可序列化副本；ApprovalRequired 的暂停错误也会记录。
	RunError    string
	FailureKind string
	Approval    *core.ErrApprovalRequired
	Signals     []projection.Signal
	Failures    []string
}

// OK 报告用例是否通过。预期审批中断可通过 Approval 断言判为成功。
func (r Report) OK() bool {
	if len(r.Failures) > 0 {
		return false
	}
	return r.Err == nil || r.Approval != nil
}

// signalRecorder 收集 RunStream/ResumeStream 推出的全部信号
// （图回调可能并发，须加锁）。
type signalRecorder struct {
	mu   sync.Mutex
	sigs []projection.Signal
}

func (r *signalRecorder) sink() func(projection.Signal) {
	return func(s projection.Signal) {
		r.mu.Lock()
		r.sigs = append(r.sigs, s)
		r.mu.Unlock()
	}
}

func (r *signalRecorder) snapshot() []projection.Signal {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]projection.Signal, len(r.sigs))
	copy(out, r.sigs)
	return out
}

// Run 执行单条用例，默认 attempt 为 1。
func (r *Runner) Run(ctx context.Context, tc Case) Report {
	return r.run(ctx, tc, 1)
}

func (r *Runner) run(ctx context.Context, tc Case, attempt int) Report {
	start := time.Now()
	seq := r.runSeq.Add(1)
	rep := Report{
		RunID:     fmt.Sprintf("eval-%d", seq),
		Attempt:   attempt,
		CaseName:  tc.Name,
		SceneKey:  tc.SceneKey,
		Tags:      append([]string(nil), tc.Tags...),
		StartedAt: start,
	}

	cfg, scriptModel, oracle, metadata, err := r.sceneConfig(ctx, tc, attempt)
	rep.Model = scriptModel
	rep.AgentMetadata = metadata
	if err != nil {
		rep.Err = fmt.Errorf("build scene config: %w", err)
		rep.RunError = rep.Err.Error()
		rep.FailureKind = "setup_error"
		rep.Duration = time.Since(start)
		r.applyExpects(tc, &rep)
		return rep
	}
	rep.SceneKey = cfg.Key

	store := r.Store
	if store == nil {
		store = session.NewMemoryStore()
	}
	// 默认 ArtifactStore 原本按 SceneKey 全局共享。评测 trial 必须隔离，
	// 以免上一条任务生成的 artifact 影响后续结果。
	if cfg.ArtifactStore == nil {
		cfg.ArtifactStore = projection.NewMemoryArtifactStore()
	}

	agent, err := core.NewAgent(ctx, cfg, store)
	if err != nil {
		rep.Err = fmt.Errorf("new agent: %w", err)
		rep.RunError = rep.Err.Error()
		rep.FailureKind = "setup_error"
		rep.Duration = time.Since(start)
		r.applyExpects(tc, &rep)
		return rep
	}

	sessionID := rep.RunID
	rec := &signalRecorder{}

	out, runErr := agent.RunStream(ctx, sessionID, tc.Input, rec.sink())
	rep.Err = runErr
	rep.Output = out

	// 自动恢复：每轮中断按固定决策 Resume，直到无中断或达到保护上限。
	if tc.AutoResume != nil {
		for i := 0; i < maxResumeRounds; i++ {
			approval, ok := core.ExtractApprovalRequired(runErr)
			if !ok {
				break
			}
			out, runErr = agent.ResumeStream(ctx, sessionID, approval.InterruptID, tc.AutoResume, rec.sink())
			rep.Output = out
			rep.Err = runErr
		}
	}

	if approval, ok := core.ExtractApprovalRequired(runErr); ok {
		rep.Approval = approval
	}
	if runErr != nil {
		rep.RunError = runErr.Error()
		if rep.Approval == nil {
			rep.FailureKind = "agent_error"
		}
	}

	rep.Signals = rec.snapshot()
	rep.Metrics = metricsFromSignals(rep.Signals)
	rep.Duration = time.Since(start)
	r.applyExpects(tc, &rep)
	if oracle == nil {
		oracle = tc.Oracle
	}
	if oracle != nil {
		if err := oracle(ctx, rep); err != nil {
			rep.Failures = append(rep.Failures, "业务状态 Oracle 失败: "+err.Error())
			if rep.FailureKind == "" || rep.FailureKind == "assertion" {
				rep.FailureKind = "oracle_error"
			}
		}
	}
	return rep
}

func (r *Runner) sceneConfig(ctx context.Context, tc Case, attempt int) (*scene.SceneConfig, *ScriptModel, Oracle, map[string]string, error) {
	var cfg *scene.SceneConfig
	var oracle Oracle
	metadata := cloneMetadata(tc.AgentMetadata)
	var err error

	switch {
	case r.ConfigFactory != nil:
		var setup AgentSetup
		setup, err = r.ConfigFactory(ctx, tc, attempt)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		cfg = setup.Config
		oracle = setup.Verify
		metadata = mergeMetadata(metadata, setup.Metadata)
		if cfg == nil {
			return nil, nil, nil, nil, fmt.Errorf("ConfigFactory returned a nil SceneConfig")
		}
	case tc.AgentConfig != nil:
		cfg = tc.AgentConfig
	}

	if cfg == nil {
		model := NewScriptModel(tc.Replies...)
		key := tc.SceneKey
		if key == "" {
			key = "eval"
		}
		return &scene.SceneConfig{
			Key:             key,
			SystemPrompt:    tc.SystemPrompt,
			Model:           model,
			Tools:           tc.Tools,
			Catalog:         tc.Catalog,
			AllowedCommands: append([]string(nil), tc.AllowedCommands...),
			MaxIterations:   defaultMaxIterations(tc),
		}, model, oracle, metadata, nil
	}

	// 不修改调用方持有的配置；调用方如果需要隔离有状态组件，应通过
	// ConfigFactory 在每次 trial 构造新的 Model 与 Tool 实例。
	copied := *cfg
	if tc.SceneKey != "" {
		copied.Key = tc.SceneKey
	}
	if copied.Key == "" {
		copied.Key = "eval"
	}
	if tc.SystemPrompt != "" {
		copied.SystemPrompt = tc.SystemPrompt
	}
	if tc.Tools != nil {
		copied.Tools = append(copied.Tools[:0:0], tc.Tools...)
	}
	if tc.Catalog != nil {
		copied.Catalog = tc.Catalog
	}
	if tc.AllowedCommands != nil {
		copied.AllowedCommands = append([]string(nil), tc.AllowedCommands...)
	}
	if tc.MaxIterations > 0 {
		copied.MaxIterations = tc.MaxIterations
	}

	var scriptModel *ScriptModel
	if tc.Replies != nil || copied.Model == nil {
		scriptModel = NewScriptModel(tc.Replies...)
		copied.Model = scriptModel
	}
	if copied.MaxIterations <= 0 {
		copied.MaxIterations = defaultMaxIterations(tc)
	}
	return &copied, scriptModel, oracle, metadata, nil
}

func cloneMetadata(metadata map[string]string) map[string]string {
	if len(metadata) == 0 {
		return nil
	}
	cloned := make(map[string]string, len(metadata))
	for key, value := range metadata {
		cloned[key] = value
	}
	return cloned
}

func mergeMetadata(base, overlay map[string]string) map[string]string {
	merged := cloneMetadata(base)
	for key, value := range overlay {
		if merged == nil {
			merged = make(map[string]string)
		}
		merged[key] = value
	}
	return merged
}

func defaultMaxIterations(tc Case) int {
	if tc.MaxIterations > 0 {
		return tc.MaxIterations
	}
	max := len(tc.Replies) + 2
	if max < 1 {
		return 1
	}
	return max
}

func metricsFromSignals(signals []projection.Signal) RunMetrics {
	var metrics RunMetrics
	for _, sig := range signals {
		switch sig.Type {
		case projection.ToolStart:
			metrics.ToolCalls++
		case projection.LLMEnd:
			metrics.ModelCalls++
			payload, _ := sig.Payload.(map[string]any)
			usage, _ := payload["usage"].(*schema.TokenUsage)
			if usage != nil {
				metrics.PromptTokens += int64(usage.PromptTokens)
				metrics.CompletionTokens += int64(usage.CompletionTokens)
			}
		}
	}
	return metrics
}

// applyExpects 逐条套用断言，聚合失败信息，不在首个失败处短路。
func (r *Runner) applyExpects(tc Case, rep *Report) {
	for _, expect := range tc.Expect {
		if expect == nil {
			continue
		}
		if err := expect(*rep); err != nil {
			rep.Failures = append(rep.Failures, err.Error())
		}
	}
	if len(rep.Failures) > 0 && rep.FailureKind == "" {
		rep.FailureKind = "assertion"
	}
}

// SuiteReport 是一组任务及其重复 trial 的汇总报告。
type SuiteReport struct {
	Name    string
	Version string
	Repeat  int
	Reports []Report
	Stats   SummaryStats
	ByTag   map[string]SummaryStats
}

// OK 报告是否全部运行通过。
func (s SuiteReport) OK() bool {
	for _, r := range s.Reports {
		if !r.OK() {
			return false
		}
	}
	return true
}

// RunAll 顺序执行全部用例一次并汇总，保留旧 API。
func (r *Runner) RunAll(ctx context.Context, cases []Case) SuiteReport {
	return r.RunSuite(ctx, Suite{Name: "agentrix", Version: "1", Cases: cases}, RunOptions{})
}

// Summary 输出逐用例结论和可靠性、效率汇总，供 CI 日志与终端查看。
func (s SuiteReport) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "评测集 %s@%s：%d 个任务，重复 %d 次\n",
		s.Name, s.Version, s.Stats.TaskCount, s.Repeat)
	fmt.Fprintf(&b, "成功率 %.1f%%（%d/%d），95%% CI [%.1f%%, %.1f%%]，pass@k %.1f%% [%.1f%%, %.1f%%]，pass^k %.1f%% [%.1f%%, %.1f%%]\n",
		s.Stats.SuccessRate*100, s.Stats.PassedRuns, s.Stats.RunCount,
		s.Stats.SuccessRateCI95[0]*100, s.Stats.SuccessRateCI95[1]*100,
		s.Stats.PassAtK*100, s.Stats.PassAtKCI95[0]*100, s.Stats.PassAtKCI95[1]*100,
		s.Stats.PassK*100, s.Stats.PassKCI95[0]*100, s.Stats.PassKCI95[1]*100)
	fmt.Fprintf(&b, "平均耗时 %.0fms，平均工具调用 %.2f，模型调用 %d，prompt/completion tokens %d/%d\n",
		s.Stats.AvgDurationMS, s.Stats.AvgToolCalls, s.Stats.ModelCalls,
		s.Stats.PromptTokens, s.Stats.CompletionTokens)

	if len(s.ByTag) > 0 {
		tags := make([]string, 0, len(s.ByTag))
		for tag := range s.ByTag {
			tags = append(tags, tag)
		}
		sortStrings(tags)
		b.WriteString("\n按标签汇总：\n")
		for _, tag := range tags {
			stats := s.ByTag[tag]
			fmt.Fprintf(&b, "  %-20s %.1f%% (%d/%d), pass@k %.1f%%, pass^k %.1f%%\n",
				tag, stats.SuccessRate*100, stats.PassedRuns, stats.RunCount,
				stats.PassAtK*100, stats.PassK*100)
		}
	}

	b.WriteString("\n逐次运行：\n")
	for _, r := range s.Reports {
		status := "PASS"
		if !r.OK() {
			status = "FAIL"
		}
		fmt.Fprintf(&b, "%-4s %s attempt=%d duration=%s tools=%d models=%d\n",
			status, r.CaseName, r.Attempt, r.Duration.Round(time.Millisecond),
			r.Metrics.ToolCalls, r.Metrics.ModelCalls)
		for _, f := range r.Failures {
			fmt.Fprintf(&b, "       - %s\n", f)
		}
		if r.RunError != "" && len(r.Failures) == 0 {
			fmt.Fprintf(&b, "       - %s\n", r.RunError)
		}
	}
	return b.String()
}

// Assert 在测试或 CI 入口中断言整组任务通过。
func (s SuiteReport) Assert(t *testing.T) {
	t.Helper()
	if !s.OK() {
		t.Fatalf("评测用例存在失败:\n%s", s.Summary())
	}
}
