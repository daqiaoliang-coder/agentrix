package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
)

// requestIDKey 是请求 ID 的 context key，未导出以避免业务侧伪造。
type requestIDKey struct{}

const (
	// headerRequestID 是请求/响应共用的链路 ID 头。
	headerRequestID = "X-Request-Id"
	maxRequestIDLen = 64
)

// WithRequestID 把请求 ID 注入 ctx，供日志与下游组件关联同一次调用。
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// RequestID 取出请求 ID；未经中间件注入时返回空串。
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// RequestIDMiddleware 为每个请求生成（或沿用上游传入的）请求 ID：
//   - 响应头回写 X-Request-Id，便于客户端排障时直接提供；
//   - 注入 context，5xx 日志据此把同一次请求的多条记录串起来。
//
// 上游 ID 超长或含换行等非法字符时重新生成，避免日志注入。
func RequestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSpace(r.Header.Get(headerRequestID))
		if !validRequestID(id) {
			id = newRequestID()
		}
		w.Header().Set(headerRequestID, id)
		next.ServeHTTP(w, r.WithContext(WithRequestID(r.Context(), id)))
	})
}

func validRequestID(id string) bool {
	if id == "" || len(id) > maxRequestIDLen {
		return false
	}
	for _, r := range id {
		if r < 0x20 || r > 0x7e {
			return false
		}
	}
	return true
}

func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "req-unknown"
	}
	return hex.EncodeToString(b[:])
}
