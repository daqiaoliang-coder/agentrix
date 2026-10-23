package api

import (
	"context"
	"errors"
	"sync"
)

// ErrStreamsClosed 表示服务正在排空，不再接受新的流式请求。
var ErrStreamsClosed = errors.New("server is draining")

// StreamTracker 追踪进行中的 SSE 连接，用于优雅下线。
//
// 背景：http.Server.Shutdown 会等待在途 handler 返回，但 SSE 是长连接，
// Turn 未结束前不会返回；若只靠 Shutdown 的 30s 上限，到点会被强制断开，
// 客户端既收不到结果也收不到可解释的错误。
//
// 排空流程：停止接收新连接后给予短暂宽限期（让短 Turn 自然完成），
// 随后 Drain 取消所有在途流的 ctx。取消会经 Agent.run 透传到图与模型调用，
// handler 随即返回，Shutdown 得以在剩余时间内正常收尾。
type StreamTracker struct {
	mu      sync.Mutex
	closed  bool
	nextID  uint64
	cancels map[uint64]context.CancelFunc
}

func NewStreamTracker() *StreamTracker {
	return &StreamTracker{cancels: make(map[uint64]context.CancelFunc)}
}

// Track 派生一个可被排空取消的 ctx，release 必须在流结束时调用。
// 服务已进入排空状态时返回 ErrStreamsClosed。
func (t *StreamTracker) Track(parent context.Context) (context.Context, func(), error) {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil, nil, ErrStreamsClosed
	}
	ctx, cancel := context.WithCancel(parent)
	id := t.nextID
	t.nextID++
	t.cancels[id] = cancel
	t.mu.Unlock()

	release := func() {
		t.mu.Lock()
		delete(t.cancels, id)
		t.mu.Unlock()
		cancel()
	}
	return ctx, release, nil
}

// Drain 标记服务为排空并取消全部在途流。可重复调用。
func (t *StreamTracker) Drain() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed = true
	for _, cancel := range t.cancels {
		cancel()
	}
}

// Active 返回当前在途流数量，供健康检查与排空日志使用。
func (t *StreamTracker) Active() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.cancels)
}
