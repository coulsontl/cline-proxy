package app

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cline-go-proxy/internal/kit"
)

// handleAnthropicStream 现在走「提交前缓冲」：内容出现前不下发任何字节，
// 因此调用方能区分"可以换号重试"和"已经发给客户端，只能报错"。

func TestHandleAnthropicStreamEmptyWritesNothing(t *testing.T) {
	upstream := sseResponse(roleLine + doneLine)
	rec := httptest.NewRecorder()

	result := handleAnthropicStream(rec, upstream, "deepseek/deepseek-v4-flash", map[string]map[string]bool{}, nil)

	if result.outcome != streamEmpty {
		t.Fatalf("outcome = %v, want streamEmpty", result.outcome)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("nothing must be written before commit, got %d bytes: %s", rec.Body.Len(), rec.Body.String())
	}
	if rec.Header().Get("Content-Type") != "" {
		t.Fatal("SSE headers must not be sent before commit")
	}
}

func TestHandleAnthropicStreamCommitsOnContent(t *testing.T) {
	upstream := sseResponse(contentLine + doneLine)
	rec := httptest.NewRecorder()

	result := handleAnthropicStream(rec, upstream, "deepseek/deepseek-v4-flash", map[string]map[string]bool{}, nil)

	if result.outcome != streamCommitted || result.aborted != nil {
		t.Fatalf("result = %+v, want clean commit", result)
	}
	body := rec.Body.String()
	for _, want := range []string{"message_start", "content_block_delta", "message_stop", "hi"} {
		if !strings.Contains(body, want) {
			t.Fatalf("committed stream missing %q: %s", want, body)
		}
	}
}

// 提交后断流：客户端已收到半截流，只能发 error 事件并让调用方记失败
func TestHandleAnthropicStreamAbortAfterCommit(t *testing.T) {
	upstream := &http.Response{StatusCode: http.StatusOK, Body: readerCloser{Reader: &interruptedStreamReader{
		data: contentLine,
	}}}
	rec := httptest.NewRecorder()

	result := handleAnthropicStream(rec, upstream, "deepseek/deepseek-v4-flash", map[string]map[string]bool{}, nil)

	if result.outcome != streamCommitted || result.aborted == nil {
		t.Fatalf("result = %+v, want committed with abort", result)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"type":"error"`) {
		t.Fatalf("aborted stream must emit an error event: %s", body)
	}
	if strings.Contains(body, "message_stop") {
		t.Fatalf("aborted stream must not emit message_stop: %s", body)
	}
	if !strings.Contains(body, "message_start") {
		t.Fatalf("message_start should still be in the replayed buffer: %s", body)
	}
}

// 提交前断流：客户端一个字节都没收到 → 可换号重试
func TestHandleAnthropicStreamAbortBeforeCommitIsRetryable(t *testing.T) {
	upstream := &http.Response{StatusCode: http.StatusOK, Body: readerCloser{Reader: &interruptedStreamReader{
		data: roleLine, // 只有 role，没有内容
	}}}
	rec := httptest.NewRecorder()

	result := handleAnthropicStream(rec, upstream, "deepseek/deepseek-v4-flash", map[string]map[string]bool{}, nil)

	if result.outcome != streamEmpty {
		t.Fatalf("outcome = %v, want streamEmpty (retryable)", result.outcome)
	}
	if result.aborted == nil {
		t.Fatal("aborted should carry the read error for logging")
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("pre-commit abort must not leak bytes: %s", rec.Body.String())
	}
}

// ============ /v1/messages 端到端：空回换号重试 ============

func TestHandleAnthropicMessagesRetriesEmptyStream(t *testing.T) {
	db := setupTestStatsDB(t)
	seedActiveAccounts(t, db, "acc-1", "acc-2")
	setClineRetryCountForTest(t, 1)
	initModelsCache()

	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if requests == 1 {
			io.WriteString(w, roleLine+doneLine) // 第一次空回
			return
		}
		io.WriteString(w, contentLine+doneLine)
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

	if requests != 2 {
		t.Fatalf("upstream requests = %d, want 2 (empty then content)", requests)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "message_stop") {
		t.Fatalf("client should get a complete stream: %s", body)
	}
	if strings.Contains(body, `"type":"error"`) {
		t.Fatalf("retried request must not report an error to the client: %s", body)
	}

	var attempts, success int
	if err := db.QueryRow(`SELECT attempts, success FROM request_log ORDER BY id DESC LIMIT 1`).Scan(&attempts, &success); err != nil {
		t.Fatalf("query request_log: %v", err)
	}
	if success != 1 || attempts != 2 {
		t.Fatalf("recorded success=%d attempts=%d, want 1/2 (one row per client request)", success, attempts)
	}
}

func TestHandleAnthropicMessagesRetriesEmptyNonStream(t *testing.T) {
	db := setupTestStatsDB(t)
	seedActiveAccounts(t, db, "acc-1", "acc-2")
	setClineRetryCountForTest(t, 1)
	initModelsCache()

	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if requests == 1 {
			io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":""},"finish_reason":"stop"}]}`)
			return
		}
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`)
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

	if requests != 2 {
		t.Fatalf("upstream requests = %d, want 2", requests)
	}
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "ok") {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}

	var attempts, success, tokens int
	if err := db.QueryRow(`SELECT attempts, success, completion_tokens FROM request_log ORDER BY id DESC LIMIT 1`).
		Scan(&attempts, &success, &tokens); err != nil {
		t.Fatalf("query request_log: %v", err)
	}
	if success != 1 || attempts != 2 || tokens != 1 {
		t.Fatalf("recorded success=%d attempts=%d completion=%d, want 1/2/1", success, attempts, tokens)
	}
}

// 两次都空回 → 502 + 一行失败（客户端不会收到半截流）
func TestHandleAnthropicMessagesEmptyTwiceReturns502(t *testing.T) {
	db := setupTestStatsDB(t)
	seedActiveAccounts(t, db, "acc-1", "acc-2")
	setClineRetryCountForTest(t, 1)
	initModelsCache()

	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, roleLine+doneLine)
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

	if requests != 2 {
		t.Fatalf("upstream requests = %d, want 2", requests)
	}
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code = %d, want 502", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "message_start") {
		t.Fatalf("must not send a half stream: %s", rec.Body.String())
	}

	var status, success int
	if err := db.QueryRow(`SELECT status_code, success FROM request_log ORDER BY id DESC LIMIT 1`).Scan(&status, &success); err != nil {
		t.Fatalf("query request_log: %v", err)
	}
	if success != 0 || status != http.StatusBadGateway {
		t.Fatalf("recorded success=%d status=%d, want 0/502", success, status)
	}

	// 只应有一行（一次客户端请求 = 一行）
	var rows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM request_log`).Scan(&rows); err != nil {
		t.Fatalf("count request_log: %v", err)
	}
	if rows != 1 {
		t.Fatalf("request_log rows = %d, want 1", rows)
	}
}

// 提交前超时兜底之后，空流改不了状态码，但统计必须记失败（不能是查不到的静默空答）
func TestHandleAnthropicMessagesTimeoutCommittedEmptyRecordedAsFailure(t *testing.T) {
	db := setupTestStatsDB(t)
	seedActiveAccounts(t, db, "acc-1")
	setClineRetryCountForTest(t, 1)
	setClineCommitTimeoutForTest(t, 1)
	initModelsCache()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(1200 * time.Millisecond) // 拖过提交前超时
		io.WriteString(w, roleLine+doneLine)
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

	// 客户端已经拿到 200（header 已发出），改不了；关键是记账
	var success, status int
	var message string
	if err := db.QueryRow(`SELECT success, status_code, error_message FROM request_log ORDER BY id DESC LIMIT 1`).
		Scan(&success, &status, &message); err != nil {
		t.Fatalf("query request_log: %v", err)
	}
	if success != 0 || status != http.StatusBadGateway {
		t.Fatalf("recorded success=%d status=%d, want 0/502", success, status)
	}
	if !strings.Contains(message, "empty response") {
		t.Fatalf("message should say the response was empty: %q", message)
	}
}
