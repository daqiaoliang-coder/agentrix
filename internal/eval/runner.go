package eval

import (
	"context"
	"fmt"
	"strings"
	"sync"
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

// Runner 执行评测用例。零值即可用。
type Runner struct {
	// Store 非 nil 时所有用例共享同一存储（评测跨会话场景时使用）；
	// 为 nil 时每个用例使用独立 MemoryStore，互不串扰。
	Store session.Store
}

// Report 是单条用例的运行结果 + 断言结论。
type Report struct {
	CaseName string
	Model    *ScriptModel
	Output   *schema.Message
	Err      error
	// Approval 为最终一次未消化的审批中断（未配置 AutoResume 时即首次中断）。
	Approval *core.ErrApprovalRequired
	Signals  []projection.Signal
	Duration time.Duration
	Failures []string
}

// OK 报告用例是否全部断言通过。
func (r Report) OK() bool { return len(r.Failures) == 0 }

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

// Run 执行单条用例：装配场景 → 流式运行（收集信号）→ 按需自动恢复
// → 套用断言。装配/运行本身的错误进入 Report.Err 与断言结论，不 panic。
func (r *Runner) Run(ctx context.Context, tc Case) Report {
	start := time.Now()
	rep := Report{CaseName: tc.Name}

	m := NewScriptModel(tc.Replies...)
	rep.Model = m

	sceneKey := tc.SceneKey
	if sceneKey == "" {
		sceneKey = "eval"
	}
	maxIter := tc.MaxIterations
	if maxIter <= 0 {
		maxIter = len(tc.Replies) + 2
		if maxIter < 1 {
			maxIter = 1
		}
	}
	cfg := &scene.SceneConfig{
		Key:             sceneKey,
		SystemPrompt:    tc.SystemPrompt,
		Model:           m,
		Tools:           tc.Tools,
		Catalog:         tc.Catalog,
		AllowedCommands: tc.AllowedCommands,
		MaxIterations:   maxIter,
	}

	store := r.Store
	if store == nil {
		store = session.NewMemoryStore()
	}
	agent, err := core.NewAgent(ctx, cfg, store)
	if err != nil {
		rep.Err = fmt.Errorf("new agent: %w", err)
		rep.Duration = time.Since(start)
		r.applyExpects(tc, &rep)
		return rep
	}

	sessionID := "eval-" + tc.Name
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

	// 记录最终未消化的中断供审批类断言（配置了自动恢复但超出保护上限
	// 仍停留中断时也能看到）。rep.Err 已持有 runErr，含审批中断包装错误。
	if approval, ok := core.ExtractApprovalRequired(runErr); ok {
		rep.Approval = approval
	}

	rep.Signals = rec.snapshot()
	rep.Duration = time.Since(start)
	r.applyExpects(tc, &rep)
	return rep
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
}

// SuiteReport 是一组用例的汇总报告。
type SuiteReport struct {
	Reports []Report
}

// RunAll 顺序执行全部用例并汇总。
func (r *Runner) RunAll(ctx context.Context, cases []Case) SuiteReport {
	sr := SuiteReport{Reports: make([]Report, 0, len(cases))}
	for _, tc := range cases {
		sr.Reports = append(sr.Reports, r.Run(ctx, tc))
	}
	return sr
}

// OK 报告是否全部用例通过。
func (s SuiteReport) OK() bool {
	for _, r := range s.Reports {
		if !r.OK() {
			return false
		}
	}
	return true
}

// Summary 输出人类可读的逐用例结论，供 CI 日志/终端查看。
func (s SuiteReport) Summary() string {
	var b strings.Builder
	passed := 0
	for _, r := range s.Reports {
		if r.OK() {
			passed++
			fmt.Fprintf(&b, "PASS  %s  (%s)\n", r.CaseName, r.Duration.Round(time.Millisecond))
			continue
		}
		fmt.Fprintf(&b, "FAIL  %s  (%s)\n", r.CaseName, r.Duration.Round(time.Millisecond))
		for _, f := range r.Failures {
			fmt.Fprintf(&b, "        - %s\n", f)
		}
	}
	fmt.Fprintf(&b, "\n%d/%d 用例通过\n", passed, len(s.Reports))
	return b.String()
}

// Assert 在测试中断言整组用例通过，失败时一次性打印全部失败明细。
func (s SuiteReport) Assert(t *testing.T) {
	t.Helper()
	if !s.OK() {
		t.Fatalf("评测用例存在失败:\n%s", s.Summary())
	}
}
