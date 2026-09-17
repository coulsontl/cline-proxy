package app

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// zen 渠道的空回检测：以前 zen 的空流会变成"静默空答"（200 + 只有 [DONE]），
// 现在明确报 502。zen 没有账号池，换不了号。

func withZenUpstream(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	previous := getZenConfig()
	next := *previous
	next.Enabled = true
	next.BaseURL = srv.URL
	setZenConfig(&next)
	t.Cleanup(func() { setZenConfig(previous) })
}

func TestHandleZenChatEmptyStreamReturns502(t *testing.T) {
	withZenUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, roleLine+doneLine)
	})

	rec := httptest.NewRecorder()
	handleZenChat(rec, httptest.NewRequest("POST", "/v1/chat/completions", nil), map[string]any{
		"model":    "deepseek-v4-flash-free",
		"stream":   true,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code = %d, want 502 for an empty zen stream", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "empty_response") {
		t.Fatalf("body should explain the empty response: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "data: ") {
		t.Fatalf("no half stream may be sent to the client: %s", rec.Body.String())
	}
}

func TestHandleZenChatEmptyNonStreamReturns502(t *testing.T) {
	withZenUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":""},"finish_reason":"stop"}]}`)
	})

	rec := httptest.NewRecorder()
	handleZenChat(rec, httptest.NewRequest("POST", "/v1/chat/completions", nil), map[string]any{
		"model":    "deepseek-v4-flash-free",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code = %d, want 502", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "empty_response") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

// 提交前超时兜底先把 header 发出去、随后确认是空流：状态码已经改不了，
// 但绝不能再去写一个 502 body（那会变成 superfluous WriteHeader + 污染已发出的流）。
func TestHandleZenChatEmptyStreamAfterTimeoutDoesNotWriteBody(t *testing.T) {
	setClineCommitTimeoutForTest(t, 1)
	withZenUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		time.Sleep(1200 * time.Millisecond) // 让提交前超时先触发
		io.WriteString(w, roleLine+doneLine)
	})

	rec := httptest.NewRecorder()
	handleZenChat(rec, httptest.NewRequest("POST", "/v1/chat/completions", nil), map[string]any{
		"model":    "deepseek-v4-flash-free",
		"stream":   true,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})

	if strings.Contains(rec.Body.String(), "empty_response") {
		t.Fatalf("must not write an error body after the header was committed: %s", rec.Body.String())
	}
}

func TestHandleZenChatNormalStreamStillWorks(t *testing.T) {
	withZenUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, contentLine+doneLine)
	})

	rec := httptest.NewRecorder()
	handleZenChat(rec, httptest.NewRequest("POST", "/v1/chat/completions", nil), map[string]any{
		"model":    "deepseek-v4-flash-free",
		"stream":   true,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "hi") || !strings.Contains(body, "[DONE]") {
		t.Fatalf("zen stream must be forwarded verbatim: %s", body)
	}
}