package app

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cline-go-proxy/internal/kit"
)

// 同类问题回归：上游中途断流不能被静默吞掉，让客户端以为请求正常完成。

func TestChatStreamToResponsesAbortEmitsFailed(t *testing.T) {
	upstream := &http.Response{StatusCode: http.StatusOK, Body: readerCloser{Reader: &interruptedStreamReader{
		data: "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n",
	}}}

	rec := httptest.NewRecorder()
	result := chatStreamToResponses(rec, upstream, nil)
	if result.outcome != streamCommitted || result.aborted == nil {
		t.Fatalf("expected a committed stream that aborted, got %+v", result)
	}

	body := rec.Body.String()
	if !strings.Contains(body, "response.failed") {
		t.Fatalf("aborted stream must emit response.failed: %s", body)
	}
	if strings.Contains(body, "response.completed") {
		t.Fatalf("aborted stream must not emit response.completed: %s", body)
	}
	if !strings.Contains(body, "upstream_stream_interrupted") {
		t.Fatalf("failure event should carry an error code: %s", body)
	}
}

func TestChatStreamToResponsesNormalEOFStillCompletes(t *testing.T) {
	upstream := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(
		"data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"))}

	rec := httptest.NewRecorder()
	result := chatStreamToResponses(rec, upstream, nil)
	if result.outcome != streamCommitted || result.aborted != nil {
		t.Fatalf("clean EOF must not be treated as an abort: %+v", result)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "response.completed") {
		t.Fatalf("clean stream must still emit response.completed: %s", body)
	}
	if strings.Contains(body, "response.failed") {
		t.Fatalf("clean stream must not emit response.failed: %s", body)
	}
}

// 提交后断流必须记为失败（以前会记成功）。记账现在由调用方做，
// 所以这里走真实的 /v1/messages 入口验证端到端。
func TestHandleAnthropicMessagesStreamAbortRecordsFailure(t *testing.T) {
	db := setupTestStatsDB(t)
	seedActiveAccounts(t, db, "acc-1")
	initModelsCache()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, contentLine) // 先给内容（提交），然后连接断开
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		panic("simulate upstream dropping the connection mid-stream")
	}))
	defer srv.Close()

	clineCfg := getClineConfig()
	kit.RebuildHTTPClient(nil, "")
	t.Cleanup(func() { kit.RebuildHTTPClient(clineCfg.Proxies, clineCfg.ProxyStrategy) })
	previousBase := clineAPIBase
	clineAPIBase = srv.URL
	t.Cleanup(func() { clineAPIBase = previousBase })

	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(
		`{"model":"poolside/laguna-s-2.1:free","max_tokens":50,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	handleAnthropicMessages(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `"type":"error"`) {
		t.Fatalf("aborted anthropic stream must emit an error event: %s", body)
	}
	if strings.Contains(body, "message_stop") {
		t.Fatalf("aborted anthropic stream must not emit message_stop: %s", body)
	}
	if !strings.Contains(body, "message_start") {
		t.Fatalf("message_start should still be in the replayed buffer: %s", body)
	}

	var success, status int
	var message string
	if err := db.QueryRow(`SELECT success, status_code, error_message FROM request_log ORDER BY id DESC LIMIT 1`).
		Scan(&success, &status, &message); err != nil {
		t.Fatalf("query request_log: %v", err)
	}
	if success != 0 || status != http.StatusBadGateway {
		t.Fatalf("aborted stream logged as success=%d status=%d, want 0/502", success, status)
	}
	if !strings.Contains(message, "aborted") {
		t.Fatalf("unexpected error message: %q", message)
	}
}

// 解码失败要能在管理面板错误列表里看到（以前 Anthropic 非流式解码失败不进统计）。
func TestHandleAnthropicNonStreamDecodeFailureIsRecorded(t *testing.T) {
	db := setupTestStatsDB(t)
	seedActiveAccounts(t, db, "acc-1")
	initModelsCache() // 让下面的模型命中缓存，避免被强制走上游流

	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "not-json") // 上游 200 但 body 无法解码
	}))
	defer srv.Close()

	clineCfg := getClineConfig()
	kit.RebuildHTTPClient(nil, "")
	t.Cleanup(func() { kit.RebuildHTTPClient(clineCfg.Proxies, clineCfg.ProxyStrategy) })

	previousBase := clineAPIBase
	clineAPIBase = srv.URL
	t.Cleanup(func() { clineAPIBase = previousBase })

	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(
		`{"model":"poolside/laguna-s-2.1:free","max_tokens":50,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	handleAnthropicMessages(rec, req)

	if gotPath != "/chat/completions" {
		t.Fatalf("fake upstream hit %q, want /chat/completions", gotPath)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code = %d, want 500", rec.Code)
	}

	var success int
	var message string
	if err := db.QueryRow(`SELECT success, error_message FROM request_log ORDER BY id DESC LIMIT 1`).
		Scan(&success, &message); err != nil {
		t.Fatalf("query request_log: %v", err)
	}
	if success != 0 || !strings.Contains(message, "decode upstream") {
		t.Fatalf("decode failure not recorded: success=%d message=%q", success, message)
	}
}
