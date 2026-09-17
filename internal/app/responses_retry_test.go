package app

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cline-go-proxy/internal/kit"
)

// /v1/responses 与 chat/messages 一致：空回/5xx 换号重试（提交前缓冲 → 客户端零字节）

// withFakeUpstream 把 cline 上游指向假服务器，返回"到目前为止的请求数"。
// handler 的 call 参数是第几次请求（1 起）。
func withFakeUpstream(t *testing.T, handler func(w http.ResponseWriter, r *http.Request, call int)) func() int {
	t.Helper()
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		handler(w, r, requests)
	}))
	t.Cleanup(srv.Close)

	clineCfg := getClineConfig()
	kit.RebuildHTTPClient(nil, "")
	t.Cleanup(func() { kit.RebuildHTTPClient(clineCfg.Proxies, clineCfg.ProxyStrategy) })
	previousBase := clineAPIBase
	clineAPIBase = srv.URL
	t.Cleanup(func() { clineAPIBase = previousBase })
	return func() int { return requests }
}

func TestHandleResponsesRetriesEmptyStream(t *testing.T) {
	db := setupTestStatsDB(t)
	seedActiveAccounts(t, db, "acc-1", "acc-2")
	setClineRetryCountForTest(t, 1)
	initModelsCache()
	calls := withFakeUpstream(t, func(w http.ResponseWriter, r *http.Request, call int) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if call == 1 {
			io.WriteString(w, roleLine+doneLine)
			return
		}
		io.WriteString(w, contentLine+doneLine)
	})

	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(
		`{"model":"deepseek/deepseek-v4-flash","stream":true,"input":"hi"}`))
	rec := httptest.NewRecorder()
	handleResponses(rec, req)

	if calls() != 2 {
		t.Fatalf("upstream requests = %d, want 2 (empty then content)", calls())
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "response.completed") {
		t.Fatalf("client should get a completed response: %s", body)
	}
	if strings.Contains(body, "response.failed") {
		t.Fatalf("retried request must not look failed: %s", body)
	}

	var attempts, success int
	if err := db.QueryRow(`SELECT attempts, success FROM request_log ORDER BY id DESC LIMIT 1`).Scan(&attempts, &success); err != nil {
		t.Fatalf("query request_log: %v", err)
	}
	if success != 1 || attempts != 2 {
		t.Fatalf("recorded success=%d attempts=%d, want 1/2", success, attempts)
	}
}

func TestHandleResponsesEmptyTwiceReturns502(t *testing.T) {
	db := setupTestStatsDB(t)
	seedActiveAccounts(t, db, "acc-1", "acc-2")
	setClineRetryCountForTest(t, 1)
	initModelsCache()
	calls := withFakeUpstream(t, func(w http.ResponseWriter, r *http.Request, call int) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, roleLine+doneLine)
	})

	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(
		`{"model":"deepseek/deepseek-v4-flash","stream":true,"input":"hi"}`))
	rec := httptest.NewRecorder()
	handleResponses(rec, req)

	if calls() != 2 {
		t.Fatalf("upstream requests = %d, want 2", calls())
	}
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code = %d, want 502", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "response.created") {
		t.Fatalf("must not send a half stream: %s", rec.Body.String())
	}

	var rows, status int
	if err := db.QueryRow(`SELECT COUNT(*) FROM request_log`).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 1 {
		t.Fatalf("request_log rows = %d, want 1", rows)
	}
	if err := db.QueryRow(`SELECT status_code FROM request_log ORDER BY id DESC LIMIT 1`).Scan(&status); err != nil {
		t.Fatalf("query: %v", err)
	}
	if status != http.StatusBadGateway {
		t.Fatalf("recorded status = %d, want 502", status)
	}
}

// 提交后断流：客户端拿到 response.failed + 记 502（不是 200 假成功）
func TestHandleResponsesAbortAfterCommitRecordsFailure(t *testing.T) {
	db := setupTestStatsDB(t)
	seedActiveAccounts(t, db, "acc-1")
	setClineRetryCountForTest(t, 0)
	initModelsCache()
	withFakeUpstream(t, func(w http.ResponseWriter, r *http.Request, call int) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, contentLine)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		panic("simulate upstream dropping the connection mid-stream")
	})

	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(
		`{"model":"deepseek/deepseek-v4-flash","stream":true,"input":"hi"}`))
	rec := httptest.NewRecorder()
	handleResponses(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "response.failed") || !strings.Contains(body, "upstream_stream_interrupted") {
		t.Fatalf("aborted stream must emit response.failed: %s", body)
	}
	if strings.Contains(body, "response.completed") {
		t.Fatalf("aborted stream must not emit response.completed: %s", body)
	}

	var success, status int
	if err := db.QueryRow(`SELECT success, status_code FROM request_log ORDER BY id DESC LIMIT 1`).Scan(&success, &status); err != nil {
		t.Fatalf("query request_log: %v", err)
	}
	if success != 0 || status != http.StatusBadGateway {
		t.Fatalf("recorded success=%d status=%d, want 0/502", success, status)
	}
}