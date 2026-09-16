package app

import (
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cline-go-proxy/internal/kit"
)

// ============ 判定函数 ============

func TestHasMessageContent(t *testing.T) {
	cases := []struct {
		name string
		msg  map[string]any
		want bool
	}{
		{"nil", nil, false},
		{"role only", map[string]any{"role": "assistant"}, false},
		{"empty content", map[string]any{"content": ""}, false},
		{"content", map[string]any{"content": "hi"}, true},
		{"content array", map[string]any{"content": []any{map[string]any{"type": "text"}}}, true},
		{"reasoning", map[string]any{"reasoning": "why"}, true},
		{"reasoning_content", map[string]any{"reasoning_content": "why"}, true},
		{"reasoning_details text", map[string]any{"reasoning_details": []any{map[string]any{"text": "why"}}}, true},
		{"reasoning_details scaffold", map[string]any{"reasoning_details": []any{map[string]any{"type": "reasoning.text", "index": float64(0)}}}, false},
		{"tool_calls", map[string]any{"tool_calls": []any{map[string]any{"index": float64(0)}}}, true},
		{"empty tool_calls", map[string]any{"tool_calls": []any{}}, false},
		{"refusal", map[string]any{"refusal": "no"}, true},
	}
	for _, tc := range cases {
		if got := hasMessageContent(tc.msg); got != tc.want {
			t.Errorf("%s: hasMessageContent = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestHasChunkContentAndTerminal(t *testing.T) {
	roleChunk := map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"role": "assistant"}}}}
	contentChunk := map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": "hi"}}}}
	usageOnly := map[string]any{"choices": []any{}, "usage": map[string]any{"prompt_tokens": float64(9)}}
	finishChunk := map[string]any{"choices": []any{map[string]any{"delta": map[string]any{}, "finish_reason": "stop"}}}

	if hasChunkContent(roleChunk) {
		t.Error("role-only chunk must not count as content")
	}
	if !hasChunkContent(contentChunk) {
		t.Error("content chunk must count as content")
	}
	if hasChunkContent(usageOnly) {
		t.Error("usage-only chunk must not count as content")
	}
	if !hasChunkContent(map[string]any{"data": contentChunk}) {
		t.Error("{data:{...}} wrapped chunk must be unwrapped before judging")
	}
	if !isTerminalChunk(finishChunk) {
		t.Error("finish_reason must be terminal")
	}
	if isTerminalChunk(contentChunk) {
		t.Error("plain content chunk must not be terminal")
	}
}

func TestIsEmptyUpstreamError(t *testing.T) {
	body := []byte(`{"error":"empty response content","success":false}`)
	if !isEmptyUpstreamError(500, body) {
		t.Error("500 + empty response content must be detected")
	}
	if isEmptyUpstreamError(400, body) {
		t.Error("4xx must not be treated as upstream empty response")
	}
	if isEmptyUpstreamError(500, []byte(`{"error":"boom"}`)) {
		t.Error("500 without the empty-content marker must not match")
	}
}

// ============ streamClineBuffered ============

const (
	roleLine    = "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n"
	reasonLine  = "data: {\"choices\":[{\"delta\":{\"reasoning\":\"thinking\"}}]}\n\n"
	contentLine = "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\n"
	doneLine    = "data: [DONE]\n\n"
)

func sseResponse(body string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}
}

func testCtx() *requestContext {
	return &requestContext{apiFormat: "openai", startAt: time.Now()}
}

func TestStreamClineBufferedCommitsOnFirstContent(t *testing.T) {
	rec := httptest.NewRecorder()
	outcome, _, _ := streamClineBuffered(rec, sseResponse(roleLine+reasonLine+contentLine+doneLine), testCtx(), nil)
	if outcome != streamCommitted {
		t.Fatalf("outcome = %v, want streamCommitted", outcome)
	}
	body := rec.Body.String()
	for _, want := range []string{"assistant", "thinking", "hi", "[DONE]"} {
		if !strings.Contains(body, want) {
			t.Fatalf("output missing %q: %s", want, body)
		}
	}
	// 缓冲的无内容前言必须按原顺序回放，不能丢、不能乱序
	if !(strings.Index(body, "assistant") < strings.Index(body, "thinking") &&
		strings.Index(body, "thinking") < strings.Index(body, "hi") &&
		strings.Index(body, "hi") < strings.Index(body, "[DONE]")) {
		t.Fatalf("buffered preamble replayed out of order: %s", body)
	}
	if rec.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("committed response missing SSE content type")
	}
}

func TestStreamClineBufferedEmptyWritesNothing(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"role then finish_reason", roleLine + "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"},
		{"done only", doneLine},
		{"eof without terminal", roleLine},
		{"empty body", ""},
		{"metadata only then done", "data: {\"choices\":[{\"delta\":{\"reasoning_details\":[{\"type\":\"x\",\"index\":0}]}}]}\n\n" + doneLine},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		outcome, _, _ := streamClineBuffered(rec, sseResponse(tc.body), testCtx(), nil)
		if outcome != streamEmpty {
			t.Fatalf("%s: outcome = %v, want streamEmpty", tc.name, outcome)
		}
		if rec.Body.Len() != 0 {
			t.Fatalf("%s: wrote %d bytes before commit, want 0", tc.name, rec.Body.Len())
		}
		if rec.Header().Get("Content-Type") != "" {
			t.Fatalf("%s: wrote SSE headers before commit", tc.name)
		}
	}
}

func TestStreamClineBufferedPreCommitReadErrorIsRetryable(t *testing.T) {
	rec := httptest.NewRecorder()
	resp := &http.Response{StatusCode: http.StatusOK, Body: readerCloser{Reader: &interruptedStreamReader{data: roleLine}}}
	outcome, _, _ := streamClineBuffered(rec, resp, testCtx(), nil)
	if outcome != streamEmpty {
		t.Fatalf("pre-commit read error should be retryable, got outcome %v", outcome)
	}
	if rec.Body.Len() != 0 {
		t.Fatal("nothing must be written on a pre-commit read error")
	}
}

func TestStreamClineBufferedFailsClosedOnBufferLimit(t *testing.T) {
	var b strings.Builder
	for i := 0; i <= maxPreCommitEvents+1; i++ {
		b.WriteString("data: {\"choices\":[{\"delta\":{}}]}\n\n")
	}
	rec := httptest.NewRecorder()
	outcome, _, _ := streamClineBuffered(rec, sseResponse(b.String()), testCtx(), nil)
	if outcome != streamFatal {
		t.Fatalf("outcome = %v, want streamFatal", outcome)
	}
	if rec.Body.Len() != 0 {
		t.Fatal("fatal outcome must not write to the client")
	}
}

// ============ pickAccountExcluding ============

// setupTestStatsDB 建一个临时 sqlite 并注册还原，返回 db。
func setupTestStatsDB(t *testing.T) *sql.DB {
	t.Helper()
	previousDB := statsDB
	database, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "stats.db"))
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	statsDB = database
	t.Cleanup(func() {
		statsDB = previousDB
		database.Close()
	})

	schema := []string{
		`CREATE TABLE accounts (
			account_id TEXT PRIMARY KEY, email TEXT, refresh_token TEXT, access_token TEXT,
			expires_at INTEGER, status TEXT, cooldown_until INTEGER, fail_count INTEGER,
			usage_count INTEGER, usage_count_today INTEGER, usage_date TEXT,
			last_used INTEGER, created_at INTEGER, last_reason TEXT,
			tokens_total INTEGER NOT NULL DEFAULT 0,
			tokens_today INTEGER NOT NULL DEFAULT 0,
			tokens_date TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE api_keys (key TEXT PRIMARY KEY, created_at INTEGER)`,
		`CREATE TABLE proxy_state (id INTEGER PRIMARY KEY, current_idx INTEGER)`,
		`INSERT INTO proxy_state(id, current_idx) VALUES(1, 0)`,
		`CREATE TABLE request_log (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			created_at TEXT NOT NULL DEFAULT (datetime('now','localtime')),
			api_format TEXT NOT NULL, account_email TEXT NOT NULL, model TEXT NOT NULL DEFAULT '',
			is_stream INTEGER NOT NULL DEFAULT 0, success INTEGER NOT NULL DEFAULT 0,
			status_code INTEGER NOT NULL DEFAULT 0, prompt_tokens INTEGER NOT NULL DEFAULT 0,
			completion_tokens INTEGER NOT NULL DEFAULT 0, total_tokens INTEGER NOT NULL DEFAULT 0,
			error_message TEXT NOT NULL DEFAULT '', duration_ms INTEGER NOT NULL DEFAULT 0
		)`,
	}
	for _, statement := range schema {
		if _, err := database.Exec(statement); err != nil {
			t.Fatalf("create test schema: %v", err)
		}
	}
	return database
}

// seedActiveAccounts 插入若干 active 账号（token 未过期，选中后不会再走刷新）。
func seedActiveAccounts(t *testing.T, db *sql.DB, ids ...string) {
	t.Helper()
	expires := time.Now().Add(time.Hour).UnixMilli()
	for _, id := range ids {
		if _, err := db.Exec(`INSERT INTO accounts
			(account_id, email, refresh_token, access_token, expires_at, status, cooldown_until,
			 fail_count, usage_count, usage_count_today, usage_date, last_used, created_at, last_reason,
			 tokens_total, tokens_today, tokens_date)
			VALUES (?, ?, '', 'workos:test-token', ?, 'active', 0, 0, 0, 0, '', 0, 0, '', 0, 0, '')`,
			id, id+"@example.com", expires); err != nil {
			t.Fatalf("insert test account %s: %v", id, err)
		}
	}
}

func setStrategyForTest(t *testing.T, strategy string) {
	t.Helper()
	previous := getProxyConfig()
	next := *previous
	next.Strategy = strategy
	setProxyConfig(&next)
	t.Cleanup(func() { setProxyConfig(previous) })
}

// setClineRetryCountForTest 直接替换内存中的 cline 配置，不落盘、不改动 data/。
func setClineRetryCountForTest(t *testing.T, n int) {
	t.Helper()
	clineConfigMu.Lock()
	previous := clineConfig
	retry := n
	clineConfig = &clineConfigData{ProxyStrategy: "round_robin", RetryCount: &retry}
	clineConfigMu.Unlock()
	t.Cleanup(func() {
		clineConfigMu.Lock()
		clineConfig = previous
		clineConfigMu.Unlock()
	})
}

func TestPickAccountExcludingSkipsTriedAccounts(t *testing.T) {
	db := setupTestStatsDB(t)
	seedActiveAccounts(t, db, "acc-a", "acc-b", "acc-c")

	acc := pickAccountExcluding([]string{"acc-a"})
	if acc == nil || acc.AccountID != "acc-b" {
		t.Fatalf("expected acc-b, got %#v", acc)
	}
	// 选中的是下标 1，游标应推进到 2
	var idx int
	if err := db.QueryRow(`SELECT current_idx FROM proxy_state WHERE id=1`).Scan(&idx); err != nil {
		t.Fatalf("read cursor: %v", err)
	}
	if idx != 2 {
		t.Fatalf("cursor = %d, want 2", idx)
	}

	// 排除多个：从游标 2 开始只剩 acc-a 可用（acc-b/acc-c 都被排除）
	acc = pickAccountExcluding([]string{"acc-b", "acc-c"})
	if acc == nil || acc.AccountID != "acc-a" {
		t.Fatalf("expected acc-a, got %#v", acc)
	}
}

func TestPickAccountExcludingFallsBackToTriedAccountWhenAlone(t *testing.T) {
	db := setupTestStatsDB(t)
	seedActiveAccounts(t, db, "acc-only")

	acc := pickAccountExcluding([]string{"acc-only"})
	if acc == nil || acc.AccountID != "acc-only" {
		t.Fatalf("single-account pool must fall back to the tried account, got %#v", acc)
	}
}

func TestPickAccountExcludingAppliesToAllStrategies(t *testing.T) {
	for _, strategy := range []string{"fill", "random"} {
		t.Run(strategy, func(t *testing.T) {
			db := setupTestStatsDB(t)
			seedActiveAccounts(t, db, "acc-a", "acc-b")
			setStrategyForTest(t, strategy)

			acc := pickAccountExcluding([]string{"acc-a"})
			if acc == nil || acc.AccountID != "acc-b" {
				t.Fatalf("%s: expected acc-b, got %#v", strategy, acc)
			}
		})
	}
}

// ============ serveClineChat 重试编排 ============

type callRecorder struct {
	streams  []bool
	excludes [][]string
}

func (c *callRecorder) record(stream bool, exclude []string) {
	c.streams = append(c.streams, stream)
	c.excludes = append(c.excludes, append([]string(nil), exclude...))
}

func fakeAccount(n int) *Account {
	return &Account{AccountID: fmt.Sprintf("acc-%d", n), Email: fmt.Sprintf("acc%d@example.com", n)}
}

func fakeCtx(acc *Account, statusCode int) *requestContext {
	return &requestContext{apiFormat: "openai", accountEmail: acc.Email, statusCode: statusCode, startAt: time.Now()}
}

func TestServeClineChatRetriesEmptyStreamWithAnotherAccount(t *testing.T) {
	setupTestStatsDB(t)
	setClineRetryCountForTest(t, 1)

	rec := httptest.NewRecorder()
	var calls callRecorder
	attempt := 0
	call := func(params map[string]any, stream bool, exclude []string) (*http.Response, *Account, *requestContext, error) {
		calls.record(stream, exclude)
		attempt++
		acc := fakeAccount(attempt)
		if attempt == 1 {
			return sseResponse(roleLine + doneLine), acc, fakeCtx(acc, 200), nil
		}
		return sseResponse(roleLine + contentLine + doneLine), acc, fakeCtx(acc, 200), nil
	}

	serveClineChat(rec, map[string]any{"model": "m"}, true, true, call)

	if len(calls.streams) != 2 {
		t.Fatalf("upstream calls = %d, want 2", len(calls.streams))
	}
	if len(calls.excludes[0]) != 0 {
		t.Fatalf("first attempt must not exclude anything: %#v", calls.excludes[0])
	}
	if len(calls.excludes[1]) != 1 || calls.excludes[1][0] != "acc-1" {
		t.Fatalf("retry must exclude the first account: %#v", calls.excludes[1])
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "hi") {
		t.Fatalf("retry output missing content: %s", rec.Body.String())
	}
}

func TestServeClineChatEmptyTwiceReturns502(t *testing.T) {
	db := setupTestStatsDB(t)
	setClineRetryCountForTest(t, 1)

	rec := httptest.NewRecorder()
	calls := 0
	call := func(params map[string]any, stream bool, exclude []string) (*http.Response, *Account, *requestContext, error) {
		calls++
		acc := fakeAccount(calls)
		return sseResponse(roleLine + doneLine), acc, fakeCtx(acc, 200), nil
	}

	serveClineChat(rec, map[string]any{"model": "m"}, true, true, call)

	if calls != 2 {
		t.Fatalf("upstream calls = %d, want 2", calls)
	}
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code = %d, want 502", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "empty response") {
		t.Fatalf("unexpected 502 body: %s", rec.Body.String())
	}
	if rec.Header().Get("Content-Type") == "text/event-stream" {
		t.Fatal("must not have committed SSE headers for a failed retry")
	}
	// 一次客户端请求只记一行，且记为失败
	var rows, failed int
	if err := db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(CASE WHEN success=0 THEN 1 ELSE 0 END),0) FROM request_log`).Scan(&rows, &failed); err != nil {
		t.Fatalf("query request_log: %v", err)
	}
	if rows != 1 || failed != 1 {
		t.Fatalf("request_log rows=%d failed=%d, want 1/1", rows, failed)
	}
}

func TestServeClineChatRetryCountZeroDisablesRetry(t *testing.T) {
	setupTestStatsDB(t)
	setClineRetryCountForTest(t, 0)

	rec := httptest.NewRecorder()
	calls := 0
	call := func(params map[string]any, stream bool, exclude []string) (*http.Response, *Account, *requestContext, error) {
		calls++
		acc := fakeAccount(calls)
		return sseResponse(roleLine + doneLine), acc, fakeCtx(acc, 200), nil
	}

	serveClineChat(rec, map[string]any{"model": "m"}, true, true, call)

	if calls != 1 {
		t.Fatalf("upstream calls = %d, want 1 (retry disabled)", calls)
	}
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code = %d, want 502", rec.Code)
	}
}

func TestServeClineChatRetriesUpstream5xx(t *testing.T) {
	setupTestStatsDB(t)
	setClineRetryCountForTest(t, 1)

	rec := httptest.NewRecorder()
	attempt := 0
	call := func(params map[string]any, stream bool, exclude []string) (*http.Response, *Account, *requestContext, error) {
		attempt++
		acc := fakeAccount(attempt)
		if attempt == 1 {
			return nil, acc, fakeCtx(acc, http.StatusBadGateway), fmt.Errorf("upstream request: EOF")
		}
		return sseResponse(contentLine + doneLine), acc, fakeCtx(acc, 200), nil
	}

	serveClineChat(rec, map[string]any{"model": "m"}, true, true, call)

	if attempt != 2 {
		t.Fatalf("upstream calls = %d, want 2", attempt)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
}

func TestServeClineChatDoesNotRetry4xx(t *testing.T) {
	setupTestStatsDB(t)
	setClineRetryCountForTest(t, 1)

	rec := httptest.NewRecorder()
	attempt := 0
	call := func(params map[string]any, stream bool, exclude []string) (*http.Response, *Account, *requestContext, error) {
		attempt++
		acc := fakeAccount(attempt)
		return nil, acc, fakeCtx(acc, http.StatusBadRequest), fmt.Errorf("API 400: bad request")
	}

	serveClineChat(rec, map[string]any{"model": "m"}, true, true, call)

	if attempt != 1 {
		t.Fatalf("upstream calls = %d, want 1 (4xx must not be retried)", attempt)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code = %d, want 500", rec.Code)
	}
}

func TestServeClineChatRetriesEmptyNonStreamResponse(t *testing.T) {
	setupTestStatsDB(t)
	setClineRetryCountForTest(t, 1)

	rec := httptest.NewRecorder()
	attempt := 0
	call := func(params map[string]any, stream bool, exclude []string) (*http.Response, *Account, *requestContext, error) {
		attempt++
		acc := fakeAccount(attempt)
		if attempt == 1 {
			return sseResponse(`{"choices":[{"message":{"role":"assistant","content":""}}]}`), acc, fakeCtx(acc, 200), nil
		}
		return sseResponse(`{"choices":[{"message":{"role":"assistant","content":"pong"}}]}`), acc, fakeCtx(acc, 200), nil
	}

	serveClineChat(rec, map[string]any{"model": "m"}, false, false, call)

	if attempt != 2 {
		t.Fatalf("upstream calls = %d, want 2", attempt)
	}
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "pong") {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestServeClineChatReasoningOnlyAggregateIsNotTreatedAsEmpty(t *testing.T) {
	setupTestStatsDB(t)
	setClineRetryCountForTest(t, 1)

	rec := httptest.NewRecorder()
	attempt := 0
	call := func(params map[string]any, stream bool, exclude []string) (*http.Response, *Account, *requestContext, error) {
		attempt++
		acc := fakeAccount(attempt)
		return sseResponse(roleLine + reasonLine + doneLine), acc, fakeCtx(acc, 200), nil
	}

	// 客户端要非流式，聚合路径（upstreamStream=true）
	serveClineChat(rec, map[string]any{"model": "m"}, false, true, call)

	if attempt != 1 {
		t.Fatalf("reasoning-only answer must not be retried, calls = %d", attempt)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
}

// ============ 端到端：真实 callClineAPI + 假上游 ============

func TestServeClineChatEndToEndEmptyThenContent(t *testing.T) {
	db := setupTestStatsDB(t)
	seedActiveAccounts(t, db, "acc-1", "acc-2")
	setClineRetryCountForTest(t, 1)

	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if requests == 1 {
			io.WriteString(w, roleLine+doneLine) // 第一次空回
			return
		}
		io.WriteString(w, roleLine+contentLine+doneLine)
	}))
	defer srv.Close()

	// 直连本机假上游，避免受 data/.cline-config.json 里配置的出口代理影响
	clineCfg := getClineConfig()
	kit.RebuildHTTPClient(nil, "")
	t.Cleanup(func() { kit.RebuildHTTPClient(clineCfg.Proxies, clineCfg.ProxyStrategy) })

	previousBase := clineAPIBase
	clineAPIBase = srv.URL
	t.Cleanup(func() { clineAPIBase = previousBase })

	rec := httptest.NewRecorder()
	serveClineChat(rec, map[string]any{
		"model":    "deepseek/deepseek-v4-flash",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, true, true, callClineAPI)

	if requests != 2 {
		t.Fatalf("upstream requests = %d, want 2", requests)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "hi") {
		t.Fatalf("missing streamed content: %s", rec.Body.String())
	}
}

func TestServeClineChatEndToEndUpstream500EmptyContent(t *testing.T) {
	db := setupTestStatsDB(t)
	seedActiveAccounts(t, db, "acc-1", "acc-2")
	setClineRetryCountForTest(t, 1)

	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, `{"error":"empty response content","success":false}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, contentLine+doneLine)
	}))
	defer srv.Close()

	clineCfg := getClineConfig()
	kit.RebuildHTTPClient(nil, "")
	t.Cleanup(func() { kit.RebuildHTTPClient(clineCfg.Proxies, clineCfg.ProxyStrategy) })

	previousBase := clineAPIBase
	clineAPIBase = srv.URL
	t.Cleanup(func() { clineAPIBase = previousBase })

	rec := httptest.NewRecorder()
	serveClineChat(rec, map[string]any{
		"model":    "deepseek/deepseek-v4-flash",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, true, true, callClineAPI)

	if requests != 2 {
		t.Fatalf("upstream requests = %d, want 2 (500 empty content must be retried)", requests)
	}
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "hi") {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
}
