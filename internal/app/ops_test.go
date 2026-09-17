package app

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------- 日志轮转 ----------

func TestRotatingWriterRotatesAndKeepsNewest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	w, err := newRotatingWriter(path, 100, 2)
	if err != nil {
		t.Fatalf("newRotatingWriter: %v", err)
	}
	t.Cleanup(func() { w.Close() }) // Windows 上句柄没关会挡住 TempDir 清理
	// 每行 20 字节，写 15 行 → 至少轮转 3 次，只保留 .1/.2 两份历史
	for i := 0; i < 15; i++ {
		if _, err := w.Write([]byte(fmt.Sprintf("line-%02d-xxxxxxxxxxx\n", i))); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	if _, err := os.Stat(path + ".3"); err == nil {
		t.Fatal("rotation exceeded the keep limit (.3 should not exist)")
	}
	for _, p := range []string{path, path + ".1", path + ".2"} {
		if fi, err := os.Stat(p); err != nil || fi.Size() == 0 {
			t.Fatalf("%s missing or empty (err=%v)", p, err)
		}
	}
	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read current log: %v", err)
	}
	if !strings.Contains(string(current), "line-14") {
		t.Fatalf("newest lines must be in the live file: %q", current)
	}
	if strings.Contains(string(current), "line-00") {
		t.Fatalf("oldest lines should have been rotated out: %q", current)
	}
}

// ---------- 版本端点 ----------

func TestHandleVersionReportsBuildInfo(t *testing.T) {
	previous := Version
	Version = "abc1234"
	t.Cleanup(func() { Version = previous })

	rec := httptest.NewRecorder()
	handleVersion(rec, httptest.NewRequest("GET", "/admin/api/version", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"abc1234", "goVersion", "uptime", "builtAt"} {
		if !strings.Contains(body, want) {
			t.Fatalf("version payload missing %q: %s", want, body)
		}
	}
}

// ---------- 全账号冷却 → 429 + Retry-After ----------

func TestServeClineChatCoolingAccountsReturns429(t *testing.T) {
	setupTestStatsDB(t)
	setClineRetryCountForTest(t, 1)

	rec := httptest.NewRecorder()
	serveClineChat(rec, map[string]any{"model": "deepseek/deepseek-v4-flash"}, false, false,
		func(params map[string]any, stream bool, exclude []string) (*http.Response, *Account, *requestContext, error) {
			return nil, nil, &requestContext{model: "deepseek/deepseek-v4-flash", startAt: time.Now()},
				&coolingError{retryAfter: 90 * time.Second, detail: "total=2 active=0 cooldown=2 expired=0"}
		})

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code = %d, want 429", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "90" {
		t.Fatalf("Retry-After = %q, want 90", got)
	}
	if !strings.Contains(rec.Body.String(), "cooling down") {
		t.Fatalf("body should explain the reason: %s", rec.Body.String())
	}

	var status int
	var message string
	if err := statsDB.QueryRow(`SELECT status_code, error_message FROM request_log ORDER BY id DESC LIMIT 1`).
		Scan(&status, &message); err != nil {
		t.Fatalf("query request_log: %v", err)
	}
	if status != http.StatusTooManyRequests {
		t.Fatalf("recorded status = %d, want 429", status)
	}
	if !strings.Contains(message, "cooling down") {
		t.Fatalf("recorded message = %q", message)
	}
}

func TestCooldownRemainingUsesEarliestAccount(t *testing.T) {
	db := setupTestStatsDB(t)
	seedActiveAccounts(t, db, "acc-1")
	now := time.Now().UnixMilli()
	if _, err := db.Exec(`UPDATE accounts SET status='cooldown', cooldown_until=? WHERE account_id='acc-1'`,
		now+120000); err != nil {
		t.Fatalf("set cooldown: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO accounts
		(account_id, email, refresh_token, access_token, expires_at, status, cooldown_until,
		 fail_count, usage_count, usage_count_today, usage_date, last_used, created_at, last_reason,
		 tokens_total, tokens_today, tokens_date)
		VALUES ('acc-2', 'acc-2@example.com', '', '', 0, 'cooldown', ?, 0, 0, 0, '', 0, 0, '', 0, 0, '')`,
		now+30000); err != nil {
		t.Fatalf("insert second cooling account: %v", err)
	}

	remaining := cooldownRemaining()
	if remaining <= 0 || remaining > 35*time.Second {
		t.Fatalf("cooldownRemaining = %v, want ~30s (earliest account)", remaining)
	}
}

// ---------- 统计口径：重试可见性 ----------

func TestServeClineChatRecordsAttempts(t *testing.T) {
	db := setupTestStatsDB(t)
	setClineRetryCountForTest(t, 1)

	// 第一次空回、第二次成功 → 成功行 attempts=2
	rec := httptest.NewRecorder()
	call := 0
	serveClineChat(rec, map[string]any{"model": "m"}, true, false,
		func(params map[string]any, stream bool, exclude []string) (*http.Response, *Account, *requestContext, error) {
			call++
			if call == 1 {
				return sseResponse(roleLine), fakeAccount(1), fakeCtx(fakeAccount(1), 200), nil
			}
			return sseResponse(contentLine + doneLine), fakeAccount(2), fakeCtx(fakeAccount(2), 200), nil
		})

	var attempts, success int
	if err := db.QueryRow(`SELECT attempts, success FROM request_log ORDER BY id DESC LIMIT 1`).
		Scan(&attempts, &success); err != nil {
		t.Fatalf("query request_log: %v", err)
	}
	if success != 1 || attempts != 2 {
		t.Fatalf("recorded success=%d attempts=%d, want 1/2", success, attempts)
	}
}

func TestStatsErrorsExposesAttempts(t *testing.T) {
	db := setupTestStatsDB(t)
	if _, err := db.Exec(`INSERT INTO request_log
		(api_format, account_email, model, is_stream, success, status_code, error_message, attempts)
		VALUES ('openai','a@example.com','m',1,0,502,'empty response',2)`); err != nil {
		t.Fatalf("seed request_log: %v", err)
	}

	rows, err := queryErrors(time.Now().Add(-time.Hour), 10, 0)
	if err != nil {
		t.Fatalf("queryErrors: %v", err)
	}
	if len(rows) != 1 || rows[0].Attempts != 2 {
		t.Fatalf("attempts not exposed: %+v", rows)
	}
}
