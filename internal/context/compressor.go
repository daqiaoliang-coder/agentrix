package context

import (
	"context"

	"github.com/cloudwego/eino/schema"
)

type Compressor struct {
	Engine *Engine
}

func NewCompressor(engine *Engine) *Compressor {
	return &Compressor{Engine: engine}
}

func (c *Compressor) Compress(
	ctx context.Context,
	messages []*schema.Message,
	prevSummary string,
) (result []*schema.Message, summary string, err error) {

	trimmed := trimToolOutputs(messages, trimKeepRecentTools)
	head, middle, tail := splitByBoundary(trimmed, c.Engine.TailTokenBudget)
	if len(middle) == 0 {
		return fixToolCallPairs(trimmed), prevSummary, nil
	}

	summary, err = c.Engine.summarize(ctx, middle, prevSummary)
	if err != nil {
		return nil, prevSummary, err
	}
	summary = c.Engine.truncateSummary(summary, middle)

	merged := make([]*schema.Message, 0, len(head)+1+len(tail))
	merged = append(merged, head...)
	merged = append(merged, schema.SystemMessage(summary))
	merged = append(merged, tail...)

	return fixToolCallPairs(merged), summary, nil
}
