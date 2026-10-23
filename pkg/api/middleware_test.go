package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequestIDMiddlewareGeneratesAndPropagates(t *testing.T) {
	var got string
	h := RequestIDMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = RequestID(r.Context())
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))

	if got == "" {
		t.Fatal("请求 ID 未注入 context")
	}
	if rec.Header().Get(headerRequestID) != got {
		t.Errorf("响应头与 context 中的 ID 不一致: header=%q ctx=%q",
			rec.Header().Get(headerRequestID), got)
	}
}

func TestRequestIDMiddlewareKeepsUpstreamID(t *testing.T) {
	var got string
	h := RequestIDMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = RequestID(r.Context())
	}))
	req := httptest.NewRequest("GET", "/healthz", nil)
	req.Header.Set(headerRequestID, "trace-abc-123")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got != "trace-abc-123" {
		t.Errorf("应沿用上游请求 ID，实际 %q", got)
	}
}

func TestRequestIDMiddlewareRejectsLogInjection(t *testing.T) {
	var got string
	h := RequestIDMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = RequestID(r.Context())
	}))
	req := httptest.NewRequest("GET", "/healthz", nil)
	req.Header.Set(headerRequestID, "bad\nid")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got == "bad\nid" || strings.ContainsAny(got, "\n") || got == "" {
		t.Errorf("非法请求 ID 应被替换，实际 %q", got)
	}
}

func TestStreamTrackerDrain(t *testing.T) {
	tr := NewStreamTracker()
	ctx, release, err := tr.Track(context.Background())
	if err != nil {
		t.Fatalf("Track: %v", err)
	}
	if tr.Active() != 1 {
		t.Fatalf("Active 应为 1，实际 %d", tr.Active())
	}

	tr.Drain()
	select {
	case <-ctx.Done():
	default:
		t.Fatal("Drain 应取消在途流的 ctx")
	}
	release()
	if tr.Active() != 0 {
		t.Errorf("释放后 Active 应为 0，实际 %d", tr.Active())
	}

	if _, _, err := tr.Track(context.Background()); err != ErrStreamsClosed {
		t.Errorf("排空后 Track 应返回 ErrStreamsClosed，实际 %v", err)
	}
}
