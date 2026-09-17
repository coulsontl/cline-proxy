package app

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 提交门的单元测试：提交前只缓冲、丢弃后一个字节都不下发、
// 超时兜底提交之后不能再作废（客户端已经收到了）。

func TestCommitGateBuffersUntilCommit(t *testing.T) {
	rec := httptest.NewRecorder()
	g := newCommitGate(rec, 0, nil)
	g.Header().Set("Content-Type", "text/event-stream")

	if _, err := g.Write([]byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}
	g.Flush()
	if rec.Body.Len() != 0 {
		t.Fatalf("nothing may reach the client before Commit: %q", rec.Body.String())
	}
	if rec.Header().Get("Content-Type") != "" {
		t.Fatal("headers must not be sent before Commit")
	}

	g.Commit()
	if rec.Body.String() != "hello" {
		t.Fatalf("buffered bytes must be replayed on commit: %q", rec.Body.String())
	}
	if rec.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatal("headers must be set on commit")
	}

	// 提交后透传
	g.Write([]byte(" world"))
	if rec.Body.String() != "hello world" {
		t.Fatalf("post-commit writes must pass through: %q", rec.Body.String())
	}
}

func TestCommitGateDiscardDropsEverything(t *testing.T) {
	rec := httptest.NewRecorder()
	g := newCommitGate(rec, 0, nil)
	g.WriteHeader(http.StatusOK)
	g.Write([]byte("partial"))

	g.Discard()
	if rec.Body.Len() != 0 || rec.Code != http.StatusOK {
		t.Fatalf("discarded attempt must not reach the client (code=%d body=%q)", rec.Code, rec.Body.String())
	}
	g.Write([]byte("late")) // 丢弃后写入应被忽略
	if rec.Body.Len() != 0 {
		t.Fatalf("writes after discard must be dropped: %q", rec.Body.String())
	}
	if g.Committed() {
		t.Fatal("discarded gate must not report committed")
	}
}

func TestCommitGateTimeoutCommitsByItself(t *testing.T) {
	rec := httptest.NewRecorder()
	g := newCommitGate(rec, 30*time.Millisecond, nil)
	g.Header().Set("Content-Type", "text/event-stream")
	g.Write([]byte("early"))

	deadline := time.Now().Add(2 * time.Second)
	for !g.Committed() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !g.Committed() {
		t.Fatal("gate should commit itself after the pre-commit timeout")
	}
	if rec.Body.String() != "early" {
		t.Fatalf("timeout commit must flush the buffer: %q", rec.Body.String())
	}
	// 超时提交之后再丢弃是空操作（客户端已经收到内容）
	g.Discard()
	if rec.Body.String() != "early" {
		t.Fatalf("discard after commit must be a no-op: %q", rec.Body.String())
	}
}

// 提交前超时之后，空流不能再换号（客户端已收到 200 + header），
// 必须记一次失败而不是重复一次请求。
func TestStreamClineBufferedTimeoutMakesEmptyStreamNonRetryable(t *testing.T) {
	db := setupTestStatsDB(t)
	setClineCommitTimeoutForTest(t, 1)

	slow := &delayedReader{delay: 1200 * time.Millisecond, data: roleLine + doneLine}
	rec := httptest.NewRecorder()
	outcome, _, _ := streamClineBuffered(rec, &http.Response{
		StatusCode: http.StatusOK, Body: readerCloser{Reader: slow}}, testCtx(), nil)

	if outcome != streamCommittedEmpty {
		t.Fatalf("outcome = %v, want streamCommittedEmpty (committed by timeout, no retry)", outcome)
	}
	var success int
	if err := db.QueryRow(`SELECT success FROM request_log ORDER BY id DESC LIMIT 1`).Scan(&success); err != nil {
		t.Fatalf("query request_log: %v", err)
	}
	if success != 0 {
		t.Fatal("a committed-but-empty stream must be recorded as a failure")
	}
}

// 提交前超时后内容照常到达：客户端拿到 header 与内容（只是不能再换号）
func TestStreamClineBufferedTimeoutStillDeliversContent(t *testing.T) {
	setClineCommitTimeoutForTest(t, 1)
	slow := &delayedReader{delay: 1200 * time.Millisecond, data: contentLine + doneLine}
	rec := httptest.NewRecorder()
	outcome, _, _ := streamClineBuffered(rec, &http.Response{
		StatusCode: http.StatusOK, Body: readerCloser{Reader: slow}}, testCtx(), nil)

	if outcome != streamCommitted {
		t.Fatalf("outcome = %v", outcome)
	}
	if !strings.Contains(rec.Body.String(), "hi") {
		t.Fatalf("content must still be forwarded: %q", rec.Body.String())
	}
}

func TestClineCommitTimeoutClampsSettings(t *testing.T) {
	cases := []struct {
		secs *int
		want time.Duration
	}{
		{nil, defaultCommitTimeoutSec * time.Second},
		{intPtr(0), 0},
		{intPtr(30), 30 * time.Second},
		{intPtr(-5), 0},
		{intPtr(99999), maxCommitTimeoutSec * time.Second},
	}
	for _, tc := range cases {
		setClineCommitTimeoutForTestPtr(t, tc.secs)
		if got := clineCommitTimeout(); got != tc.want {
			t.Fatalf("clineCommitTimeout(%v) = %v, want %v", tc.secs, got, tc.want)
		}
	}
}

// delayedReader 先阻塞 delay，再把 data 交出去（模拟上游迟迟不出内容）。
type delayedReader struct {
	delay time.Duration
	data  string
	done  bool
}

func (r *delayedReader) Read(p []byte) (int, error) {
	if !r.done {
		time.Sleep(r.delay)
		r.done = true
	}
	if r.data == "" {
		return 0, io.EOF
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

func intPtr(v int) *int { return &v }

func setClineCommitTimeoutForTest(t *testing.T, seconds int) {
	t.Helper()
	setClineCommitTimeoutForTestPtr(t, intPtr(seconds))
}

func setClineCommitTimeoutForTestPtr(t *testing.T, seconds *int) {
	t.Helper()
	clineConfigMu.Lock()
	previous := clineConfig
	retry := defaultClineRetryCount
	clineConfig = &clineConfigData{ProxyStrategy: "round_robin", RetryCount: &retry, CommitTimeoutSec: seconds}
	clineConfigMu.Unlock()
	t.Cleanup(func() {
		clineConfigMu.Lock()
		clineConfig = previous
		clineConfigMu.Unlock()
	})
}