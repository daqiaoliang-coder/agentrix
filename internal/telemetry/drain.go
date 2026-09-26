package telemetry

import (
	"io"

	"github.com/cloudwego/eino/callbacks"
	"github.com/cloudwego/eino/schema"
)

// 本文件的 drain/truncate 助手与 internal/harness/core/observe.go 中的
// 未导出实现同构。core 依赖本包（telemetry 不能反向 import core），
// 故保留一份最小拷贝，而非为两个函数抽共享包。

// drainInputMessages 读空回调输入流并拼接为单条消息。必须读到 EOF 并
// Close：流式模式下回调拿到的是私有副本，不读空会泄漏 goroutine。
func drainInputMessages(sr *schema.StreamReader[callbacks.CallbackInput]) callbacks.CallbackInput {
	defer sr.Close()
	var frames []*schema.Message
	for {
		frame, err := sr.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil
		}
		if m, ok := frame.(*schema.Message); ok && m != nil {
			frames = append(frames, m)
		}
	}
	if len(frames) == 0 {
		return nil
	}
	out, err := schema.ConcatMessages(frames)
	if err != nil {
		return frames[len(frames)-1]
	}
	return out
}

// drainOutputMessages 读空回调输出流并还原节点输出。
// ToolsNode 输出为 []*schema.Message，ChatModel 输出为 *schema.Message
// 增量帧，按帧类型归并。
func drainOutputMessages(sr *schema.StreamReader[callbacks.CallbackOutput]) callbacks.CallbackOutput {
	defer sr.Close()
	var list []*schema.Message
	var frames []*schema.Message
	isList := false
	for {
		frame, err := sr.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil
		}
		switch v := frame.(type) {
		case []*schema.Message:
			isList = true
			list = append(list, v...)
		case *schema.Message:
			if v != nil {
				frames = append(frames, v)
			}
		}
	}
	if isList {
		return list
	}
	if len(frames) == 0 {
		return nil
	}
	out, err := schema.ConcatMessages(frames)
	if err != nil {
		return frames[len(frames)-1]
	}
	return out
}

// truncateRunes 截断超长文本：span 事件与指标属性是观测摘要，
// 原文已在 RawHistory，全量入 trace 只会撑爆后端。
func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}
