package projection

import (
	"fmt"
	"sync"
	"time"
)

// Emitter 是 Turn 级信号发射器：把运行事实翻译为 Signal 并推给 sink。
//
// sink 为 nil 时 Emit 是 no-op——Agent 的同步路径（Run/Resume）借此
// 与流式路径共用同一份代码，不为不可观测的执行付出发射成本。
//
// 线程安全：ToolsNode 内工具并发执行，回调可能来自多个 goroutine。
type Emitter struct {
	mu       sync.Mutex
	sink     func(Signal)
	threadID string
	roundID  string
	seq      int64
}

// NewEmitter 创建发射器。threadID 通常为 sessionID，roundID 为本次 Turn 标识。
func NewEmitter(threadID, roundID string, sink func(Signal)) *Emitter {
	return &Emitter{sink: sink, threadID: threadID, roundID: roundID}
}

// Emit 发射一条信号。payload 需可 JSON 序列化（SSE 传输）。
func (e *Emitter) Emit(typ SignalType, payload any) {
	e.mu.Lock()
	e.seq++
	sig := Signal{
		ID:       fmt.Sprintf("sig_%d_%d", time.Now().UnixNano(), e.seq),
		TS:       time.Now(),
		ThreadID: e.threadID,
		RoundID:  e.roundID,
		Type:     typ,
		Payload:  payload,
	}
	sink := e.sink
	e.mu.Unlock()
	if sink != nil {
		sink(sig)
	}
}
