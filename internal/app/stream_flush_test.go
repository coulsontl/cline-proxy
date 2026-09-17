package app

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 回归：requestLogMiddleware 包裹后的 ResponseWriter 必须仍然实现 http.Flusher。
// statusWriter 只内嵌 ResponseWriter 接口时不会提升 Flush，SSE 处理器里的
// w.(http.Flusher) 断言会失败，直接返回空的 200 响应体（客户端报
// "stream ended without terminal event"）。
func TestStatusWriterKeepsFlusher(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := &statusWriter{ResponseWriter: rec}

	if _, ok := any(sw).(http.Flusher); !ok {
		t.Fatal("statusWriter 丢失 http.Flusher：SSE 流式响应会被判为 streaming not supported")
	}

	sw.WriteHeader(http.StatusOK)
	sw.Flush()
	if !rec.Flushed {
		t.Fatal("Flush 未转发到底层 ResponseWriter")
	}
}

// 端到端：经请求日志中间件包装后，SSE 流必须真正下发到客户端。
// 走的是真实的 chat/completions 出口（serveClineChat），固定住了当年
// "没有 [DONE]、客户端报 stream ended without terminal event" 的那个回归。
func TestStreamResponseThroughLogMiddleware(t *testing.T) {
	upstream := &http.Response{
		StatusCode: http.StatusOK,
		Body: io.NopCloser(strings.NewReader(
			"data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n" +
				"data: [DONE]\n\n")),
	}
	setClineRetryCountForTest(t, 0)

	srv := httptest.NewServer(requestLogMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveClineChat(w, map[string]any{"model": "m"}, true, true,
			func(params map[string]any, stream bool, exclude []string) (*http.Response, *Account, *requestContext, error) {
				return upstream, fakeAccount(1), fakeCtx(fakeAccount(1), 200), nil
			})
	})))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !strings.Contains(string(body), "hello") {
		t.Fatalf("SSE body not delivered through middleware, got %q", string(body))
	}
	if !strings.Contains(string(body), "[DONE]") {
		t.Fatalf("missing terminal event, got %q", string(body))
	}
}
