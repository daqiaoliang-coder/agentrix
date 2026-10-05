package eval

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/daqiaoliang-coder/agentrix/internal/projection"
)

// ResultRecord 是 JSONL 导出的单次运行记录。
// 为避免默认把工具参数与结果中的敏感数据写入文件，仅导出信号类型，
// 完整 Signals 仍保留在进程内 Report 中供授权代码检查。
type ResultRecord struct {
	Dataset        string                  `json:"dataset"`
	DatasetVersion string                  `json:"dataset_version"`
	RunID          string                  `json:"run_id"`
	CaseID         string                  `json:"case_id"`
	SceneKey       string                  `json:"scene_key,omitempty"`
	AgentMetadata  map[string]string       `json:"agent_metadata,omitempty"`
	CaseIndex      int                     `json:"case_index"`
	Attempt        int                     `json:"attempt"`
	Tags           []string                `json:"tags,omitempty"`
	StartedAt      string                  `json:"started_at"`
	DurationMS     float64                 `json:"duration_ms"`
	Passed         bool                    `json:"passed"`
	FailureKind    string                  `json:"failure_kind,omitempty"`
	Error          string                  `json:"error,omitempty"`
	Output         string                  `json:"output,omitempty"`
	Metrics        RunMetrics              `json:"metrics"`
	SignalTypes    []projection.SignalType `json:"signal_types,omitempty"`
	Failures       []string                `json:"failures,omitempty"`
}

// WriteJSONL 把 suite 逐次运行结果写成 JSON Lines，可供 CI、数据仓库或看板消费。
func (s SuiteReport) WriteJSONL(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	for _, rep := range s.Reports {
		record := ResultRecord{
			Dataset:        s.Name,
			DatasetVersion: s.Version,
			RunID:          rep.RunID,
			CaseID:         rep.CaseName,
			SceneKey:       rep.SceneKey,
			AgentMetadata:  cloneMetadata(rep.AgentMetadata),
			CaseIndex:      rep.CaseIndex,
			Attempt:        rep.Attempt,
			Tags:           append([]string(nil), rep.Tags...),
			StartedAt:      rep.StartedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
			DurationMS:     float64(rep.Duration) / 1e6,
			Passed:         rep.OK(),
			FailureKind:    rep.FailureKind,
			Error:          rep.RunError,
			Metrics:        rep.Metrics,
			Failures:       append([]string(nil), rep.Failures...),
			SignalTypes:    make([]projection.SignalType, 0, len(rep.Signals)),
		}
		if rep.Output != nil {
			record.Output = rep.Output.Content
		}
		for _, sig := range rep.Signals {
			record.SignalTypes = append(record.SignalTypes, sig.Type)
		}
		if err := enc.Encode(record); err != nil {
			return fmt.Errorf("encode eval result %s: %w", rep.RunID, err)
		}
	}
	return nil
}

// WriteSummaryJSON 写出机器可读的 suite 汇总指标。
func (s SuiteReport) WriteSummaryJSON(w io.Writer) error {
	payload := struct {
		Name    string                  `json:"name"`
		Version string                  `json:"version"`
		Repeat  int                     `json:"repeat"`
		Stats   SummaryStats            `json:"stats"`
		ByTag   map[string]SummaryStats `json:"by_tag,omitempty"`
		Passed  bool                    `json:"passed"`
	}{
		Name: s.Name, Version: s.Version, Repeat: s.Repeat,
		Stats: s.Stats, ByTag: s.ByTag, Passed: s.OK(),
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(payload); err != nil {
		return fmt.Errorf("encode eval summary: %w", err)
	}
	return nil
}
