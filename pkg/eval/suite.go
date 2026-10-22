package eval

import (
	"context"
	"math"
	"math/rand"
	"sort"
	"sync"
	"time"
)

// Suite 是一个版本化的评测任务集。
type Suite struct {
	Name    string
	Version string
	Cases   []Case
}

// RunOptions 控制每个任务的重复次数与 suite 并发度。
// Parallelism 默认为 1。并行评测时应配置 Runner.ConfigFactory，
// 让每个 trial 获得独立的 model / tools / 环境对象。
type RunOptions struct {
	Repeat      int
	Parallelism int
}

// SummaryStats 是可直接用于看板、门禁和模型对比的汇总指标。
type SummaryStats struct {
	TaskCount        int        `json:"task_count"`
	RunCount         int        `json:"run_count"`
	PassedRuns       int        `json:"passed_runs"`
	FailedRuns       int        `json:"failed_runs"`
	SuccessRate      float64    `json:"success_rate"`
	SuccessRateCI95  [2]float64 `json:"success_rate_ci95"`
	PassAtK          float64    `json:"pass_at_k"`
	PassAtKCI95      [2]float64 `json:"pass_at_k_ci95"`
	PassK            float64    `json:"pass_k"`
	PassKCI95        [2]float64 `json:"pass_k_ci95"`
	AvgDurationMS    float64    `json:"avg_duration_ms"`
	AvgToolCalls     float64    `json:"avg_tool_calls"`
	ModelCalls       int        `json:"model_calls"`
	PromptTokens     int64      `json:"prompt_tokens"`
	CompletionTokens int64      `json:"completion_tokens"`
}

type runJob struct {
	caseIndex int
	attempt   int
	tc        Case
}

// RunSuite 对整个 Suite 运行评测。报告顺序固定为任务顺序、再按 attempt 排序，
// 与并行完成顺序无关。
func (r *Runner) RunSuite(ctx context.Context, suite Suite, options RunOptions) SuiteReport {
	if r == nil {
		r = &Runner{}
	}
	repeat := options.Repeat
	if repeat < 1 {
		repeat = 1
	}
	parallelism := options.Parallelism
	if parallelism < 1 {
		parallelism = 1
	}
	jobCount := len(suite.Cases) * repeat
	workers := parallelism
	if workers > jobCount {
		workers = jobCount
	}
	reports := make([]Report, jobCount)
	if jobCount > 0 {
		jobs := make(chan runJob)
		var wg sync.WaitGroup
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for job := range jobs {
					rep := r.run(ctx, job.tc, job.attempt)
					rep.CaseIndex = job.caseIndex
					rep.Tags = append([]string(nil), job.tc.Tags...)
					reports[job.caseIndex*repeat+job.attempt-1] = rep
				}
			}()
		}
		for caseIndex, tc := range suite.Cases {
			for attempt := 1; attempt <= repeat; attempt++ {
				jobs <- runJob{caseIndex: caseIndex, attempt: attempt, tc: tc}
			}
		}
		close(jobs)
		wg.Wait()
	}

	result := SuiteReport{
		Name:    suite.Name,
		Version: suite.Version,
		Repeat:  repeat,
		Reports: reports,
	}
	result.Stats = summarizeReports(reports, len(suite.Cases), repeat)
	result.ByTag = summarizeByTag(reports, repeat)
	return result
}

func summarizeByTag(reports []Report, repeat int) map[string]SummaryStats {
	grouped := make(map[string][]Report)
	for _, rep := range reports {
		seen := make(map[string]bool, len(rep.Tags))
		for _, tag := range rep.Tags {
			if tag == "" || seen[tag] {
				continue
			}
			seen[tag] = true
			grouped[tag] = append(grouped[tag], rep)
		}
	}
	out := make(map[string]SummaryStats, len(grouped))
	for tag, group := range grouped {
		caseIDs := map[int]bool{}
		for _, rep := range group {
			caseIDs[rep.CaseIndex] = true
		}
		out[tag] = summarizeReports(group, len(caseIDs), repeat)
	}
	return out
}

func summarizeReports(reports []Report, taskCount, repeat int) SummaryStats {
	stats := SummaryStats{TaskCount: taskCount, RunCount: len(reports)}
	if len(reports) == 0 {
		return stats
	}

	var duration time.Duration
	var toolCalls int
	trialsByCase := make(map[int][]Report, taskCount)
	for _, rep := range reports {
		trialsByCase[rep.CaseIndex] = append(trialsByCase[rep.CaseIndex], rep)
		duration += rep.Duration
		toolCalls += rep.Metrics.ToolCalls
		stats.ModelCalls += rep.Metrics.ModelCalls
		stats.PromptTokens += rep.Metrics.PromptTokens
		stats.CompletionTokens += rep.Metrics.CompletionTokens
		if rep.OK() {
			stats.PassedRuns++
		} else {
			stats.FailedRuns++
		}
	}
	stats.SuccessRate = float64(stats.PassedRuns) / float64(stats.RunCount)
	stats.AvgDurationMS = float64(duration) / float64(time.Millisecond) / float64(stats.RunCount)
	stats.AvgToolCalls = float64(toolCalls) / float64(stats.RunCount)

	if taskCount == 0 {
		return stats
	}
	var atLeastOne, allPass int
	taskSuccessRates := make([]float64, 0, len(trialsByCase))
	for _, trials := range trialsByCase {
		passed := 0
		for _, trial := range trials {
			if trial.OK() {
				passed++
			}
		}
		if passed > 0 {
			atLeastOne++
		}
		if len(trials) == repeat && passed == repeat {
			allPass++
		}
		if len(trials) > 0 {
			taskSuccessRates = append(taskSuccessRates, float64(passed)/float64(len(trials)))
		}
	}
	stats.PassAtK = float64(atLeastOne) / float64(taskCount)
	stats.PassK = float64(allPass) / float64(taskCount)
	stats.PassAtKCI95 = wilsonInterval(atLeastOne, taskCount)
	stats.PassKCI95 = wilsonInterval(allPass, taskCount)
	if repeat == 1 {
		stats.SuccessRateCI95 = wilsonInterval(stats.PassedRuns, stats.RunCount)
	} else {
		sort.Float64s(taskSuccessRates)
		stats.SuccessRateCI95 = clusterBootstrapCI95(taskSuccessRates)
	}
	return stats
}

// clusterBootstrapCI95 重采样任务而非单次运行，避免把同一任务的重复 trial
// 错当作独立样本而低估置信区间。固定 RNG 种子，保证报告可复现。
func clusterBootstrapCI95(taskRates []float64) [2]float64 {
	if len(taskRates) == 0 {
		return [2]float64{}
	}
	const samples = 10000
	rng := rand.New(rand.NewSource(1))
	distribution := make([]float64, samples)
	for i := range distribution {
		var sum float64
		for range taskRates {
			sum += taskRates[rng.Intn(len(taskRates))]
		}
		distribution[i] = sum / float64(len(taskRates))
	}
	sort.Float64s(distribution)
	return [2]float64{distribution[int(0.025*float64(samples))], distribution[int(0.975*float64(samples))]}
}

// wilsonInterval 返回二项比例的 95% Wilson 区间，避免小样本或 0/100% 时
// 使用正态近似得到越界或过窄区间。
func wilsonInterval(successes, total int) [2]float64 {
	if total <= 0 {
		return [2]float64{}
	}
	const z = 1.959963984540054
	n := float64(total)
	p := float64(successes) / n
	z2 := z * z
	denom := 1 + z2/n
	center := (p + z2/(2*n)) / denom
	radius := z * sqrtFloat(p*(1-p)/n+z2/(4*n*n)) / denom
	return [2]float64{maxFloat(0, center-radius), minFloat(1, center+radius)}
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func minFloat(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func sqrtFloat(v float64) float64 {
	return math.Sqrt(v)
}

func sortStrings(values []string) {
	sort.Strings(values)
}
