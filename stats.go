package main

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// 全局统计数据库句柄与路径。statsDB 为 nil 表示统计功能不可用（初始化失败时降级）。
var (
	statsDB   *sql.DB
	statsPath string
)

func init() {
	exe, _ := os.Executable()
	statsPath = filepath.Join(filepath.Dir(exe), "data", "stats.db")
	overridePath = filepath.Join(filepath.Dir(exe), "override.md")
}

// overridePath 指向 exe 目录下的 override.md，供 loadOverrideContent 与管理端读写。
var overridePath string

// requestContext 承载一次请求的元数据，由 callClineAPI 创建并透传到响应处理 handler。
// account_email / usage 两类信息原本分散在不同函数，借此结构在记录时同时拿到。
type requestContext struct {
	apiFormat    string // "openai" 或 "anthropic"，由调用方补填
	accountEmail string // callClineAPI 填充；无账号记 "no_account"
	model        string
	isStream     bool
	startAt      time.Time
	statusCode   int // 200 成功；非 200 错误由 callClineAPI 填充；网络错误留 0
}

// tokenUsage 保存从响应中解析出的 token 消耗。
type tokenUsage struct {
	promptTokens     int
	completionTokens int
	totalTokens      int
}

// initStats 初始化统计数据库。失败只返回 error，由调用方决定是否降级（不应 fatal）。
// 幂等：已初始化则直接返回。
func initStats() error {
	if statsDB != nil {
		return nil
	}
	dir := filepath.Dir(statsPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("mkdir data dir: %w", err)
	}

	d, err := sql.Open("sqlite", statsPath)
	if err != nil {
		return fmt.Errorf("open stats db: %w", err)
	}
	// SQLite 写并发最简方案：单连接 + WAL + busy_timeout 避免锁错。
	d.SetMaxOpenConns(1)

	stmts := []string{
		`PRAGMA journal_mode=WAL`,
		`PRAGMA synchronous=NORMAL`,
		`PRAGMA busy_timeout=5000`,
		// 账号池（替代 .cline-accounts.json 的 accounts + currentIdx）
		`CREATE TABLE IF NOT EXISTS accounts (
  account_id        TEXT    PRIMARY KEY,
  email             TEXT    NOT NULL DEFAULT '',
  refresh_token     TEXT    NOT NULL,
  access_token      TEXT    NOT NULL DEFAULT '',
  expires_at        INTEGER NOT NULL DEFAULT 0,
  status            TEXT    NOT NULL DEFAULT 'active',
  cooldown_until    INTEGER NOT NULL DEFAULT 0,
  fail_count        INTEGER NOT NULL DEFAULT 0,
  usage_count       INTEGER NOT NULL DEFAULT 0,
  usage_count_today INTEGER NOT NULL DEFAULT 0,
  usage_date        TEXT    NOT NULL DEFAULT '',
  last_reason       TEXT    NOT NULL DEFAULT '',
  last_used         INTEGER NOT NULL DEFAULT 0,
  created_at        INTEGER NOT NULL DEFAULT 0
)`,
		`CREATE INDEX IF NOT EXISTS idx_acc_status ON accounts(status)`,
		// API 密钥
		`CREATE TABLE IF NOT EXISTS api_keys (
  key        TEXT    PRIMARY KEY,
  created_at INTEGER NOT NULL DEFAULT 0
)`,
		// 代理状态（轮询游标等单行配置）
		`CREATE TABLE IF NOT EXISTS proxy_state (
  id INTEGER PRIMARY KEY CHECK(id=1),
  current_idx INTEGER NOT NULL DEFAULT 0
)`,
		`INSERT OR IGNORE INTO proxy_state(id, current_idx) VALUES(1, 0)`,
		// 请求统计日志
		`CREATE TABLE IF NOT EXISTS request_log (
  id                INTEGER PRIMARY KEY AUTOINCREMENT,
  created_at        TEXT    NOT NULL DEFAULT (datetime('now','localtime')),
  api_format        TEXT    NOT NULL,
  account_email     TEXT    NOT NULL,
  model             TEXT    NOT NULL DEFAULT '',
  is_stream         INTEGER NOT NULL DEFAULT 0,
  success           INTEGER NOT NULL DEFAULT 0,
  status_code       INTEGER NOT NULL DEFAULT 0,
  prompt_tokens     INTEGER NOT NULL DEFAULT 0,
  completion_tokens INTEGER NOT NULL DEFAULT 0,
  total_tokens      INTEGER NOT NULL DEFAULT 0,
  error_message     TEXT    NOT NULL DEFAULT '',
  duration_ms       INTEGER NOT NULL DEFAULT 0
)`,
		`CREATE INDEX IF NOT EXISTS idx_req_created ON request_log(created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_req_account ON request_log(account_email)`,
		`CREATE INDEX IF NOT EXISTS idx_req_model  ON request_log(model)`,
		`CREATE INDEX IF NOT EXISTS idx_req_success ON request_log(success)`,
		// 模型输入上下文限制（0 = 不限制，默认）
		`CREATE TABLE IF NOT EXISTS model_limits (
  model_id      TEXT PRIMARY KEY,
  context_limit INTEGER NOT NULL DEFAULT 0
)`,
	}
	for _, s := range stmts {
		if _, err := d.Exec(s); err != nil {
			d.Close()
			return fmt.Errorf("init stats schema: %w (stmt=%s)", err, s)
		}
	}

	// 迁移旧库：accounts 表补充新增列（列已存在时忽略 duplicate column 错误）。
	migrateStmts := []string{
		`ALTER TABLE accounts ADD COLUMN usage_count_today INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE accounts ADD COLUMN usage_date TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE accounts ADD COLUMN last_reason TEXT NOT NULL DEFAULT ''`,
	}
	for _, s := range migrateStmts {
		if _, err := d.Exec(s); err != nil {
			// 旧库可能已有部分列，忽略"列已存在"即可
			if !strings.Contains(err.Error(), "duplicate column name") {
				d.Close()
				return fmt.Errorf("migrate stats schema: %w (stmt=%s)", err, s)
			}
		}
	}

	statsDB = d
	log.Printf("stats db ready: %s", statsPath)
	return nil
}

// insertRequestRecord 是唯一的写入点。所有成功/失败路径都经此入库。
// 统计功能不可用（statsDB==nil）或 ctx 为空时静默跳过，不影响代理主流程。
func insertRequestRecord(ctx *requestContext, u tokenUsage, success bool, statusCode int, errMsg string) {
	if statsDB == nil || ctx == nil {
		return
	}
	email := ctx.accountEmail
	if email == "" {
		email = "no_account"
	}
	dur := time.Since(ctx.startAt).Milliseconds()
	_, _ = statsDB.Exec(
		`INSERT INTO request_log
  (created_at, api_format, account_email, model, is_stream,
   success, status_code, prompt_tokens, completion_tokens,
   total_tokens, error_message, duration_ms)
  VALUES (datetime('now','localtime'),?,?,?,?,?,?,?,?,?,?,?)`,
		ctx.apiFormat, email, ctx.model, boolToInt(ctx.isStream),
		boolToInt(success), statusCode,
		u.promptTokens, u.completionTokens, u.totalTokens,
		truncate(errMsg, 2000), dur,
	)
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// extractOpenAIUsage 从 OpenAI 格式 usage 对象提取 token。
// JSON unmarshal 出的数字均为 float64，需断言后转 int。
// 最后一个带 usage 的 chunk 胜出（流式场景）。
func extractOpenAIUsage(usage map[string]any, u *tokenUsage) {
	if v, ok := usage["prompt_tokens"].(float64); ok {
		u.promptTokens = int(v)
	}
	if v, ok := usage["completion_tokens"].(float64); ok {
		u.completionTokens = int(v)
	}
	if v, ok := usage["total_tokens"].(float64); ok {
		u.totalTokens = int(v)
	}
	if u.totalTokens == 0 && (u.promptTokens > 0 || u.completionTokens > 0) {
		u.totalTokens = u.promptTokens + u.completionTokens
	}
}

// ---------- 查询：返回统计聚合结果 ----------

type usageOverview struct {
	TotalRequests    int `json:"total_requests"`
	Success          int `json:"success"`
	Errors           int `json:"errors"`
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type usageDay struct {
	Date             string `json:"date"`
	Requests         int    `json:"requests"`
	Success          int    `json:"success"`
	Errors           int    `json:"errors"`
	PromptTokens     int    `json:"prompt_tokens"`
	CompletionTokens int    `json:"completion_tokens"`
	TotalTokens      int    `json:"total_tokens"`
}

type accountStat struct {
	Email            string `json:"email"`
	Requests         int    `json:"requests"`
	Success          int    `json:"success"`
	Errors           int    `json:"errors"`
	PromptTokens     int    `json:"prompt_tokens"`
	CompletionTokens int    `json:"completion_tokens"`
	TotalTokens      int    `json:"total_tokens"`
}

type modelStat struct {
	Model            string `json:"model"`
	Requests         int    `json:"requests"`
	Success          int    `json:"success"`
	Errors           int    `json:"errors"`
	PromptTokens     int    `json:"prompt_tokens"`
	CompletionTokens int    `json:"completion_tokens"`
	TotalTokens      int    `json:"total_tokens"`
}

type errorRow struct {
	ID           int64  `json:"id"`
	CreatedAt    string `json:"created_at"`
	AccountEmail string `json:"account_email"`
	Model        string `json:"model"`
	StatusCode   int    `json:"status_code"`
	ErrorMessage string `json:"error_message"`
}

func queryUsageOverview(cutoff time.Time) (usageOverview, error) {
	var o usageOverview
	err := statsDB.QueryRow(
		`SELECT COUNT(*),
		        COALESCE(SUM(success),0), COALESCE(SUM(1-success),0),
		        COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0), COALESCE(SUM(total_tokens),0)
		 FROM request_log WHERE created_at >= ?`,
		cutoff.Format("2006-01-02 15:04:05"),
	).Scan(&o.TotalRequests, &o.Success, &o.Errors, &o.PromptTokens, &o.CompletionTokens, &o.TotalTokens)
	return o, err
}

func queryUsageTrend(cutoff time.Time) ([]usageDay, error) {
	rows, err := statsDB.Query(
		`SELECT substr(created_at,1,10) d, COUNT(*) n,
		        COALESCE(SUM(success),0), COALESCE(SUM(1-success),0),
		        COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0), COALESCE(SUM(total_tokens),0)
		 FROM request_log WHERE created_at >= ?
		 GROUP BY d ORDER BY d`,
		cutoff.Format("2006-01-02 15:04:05"),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []usageDay
	for rows.Next() {
		var d usageDay
		if err := rows.Scan(&d.Date, &d.Requests, &d.Success, &d.Errors, &d.PromptTokens, &d.CompletionTokens, &d.TotalTokens); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func queryByAccount(cutoff time.Time) ([]accountStat, error) {
	rows, err := statsDB.Query(
		`SELECT account_email, COUNT(*),
		        COALESCE(SUM(success),0), COALESCE(SUM(1-success),0),
		        COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0), COALESCE(SUM(total_tokens),0)
		 FROM request_log WHERE created_at >= ?
		 GROUP BY account_email ORDER BY COUNT(*) DESC`,
		cutoff.Format("2006-01-02 15:04:05"),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []accountStat
	for rows.Next() {
		var a accountStat
		if err := rows.Scan(&a.Email, &a.Requests, &a.Success, &a.Errors, &a.PromptTokens, &a.CompletionTokens, &a.TotalTokens); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func queryByModel(cutoff time.Time) ([]modelStat, error) {
	rows, err := statsDB.Query(
		`SELECT model, COUNT(*),
		        COALESCE(SUM(success),0), COALESCE(SUM(1-success),0),
		        COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0), COALESCE(SUM(total_tokens),0)
		 FROM request_log WHERE created_at >= ?
		 GROUP BY model ORDER BY COUNT(*) DESC`,
		cutoff.Format("2006-01-02 15:04:05"),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []modelStat
	for rows.Next() {
		var m modelStat
		if err := rows.Scan(&m.Model, &m.Requests, &m.Success, &m.Errors, &m.PromptTokens, &m.CompletionTokens, &m.TotalTokens); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func queryErrors(cutoff time.Time, limit, offset int) ([]errorRow, error) {
	rows, err := statsDB.Query(
		`SELECT id, created_at, account_email, model, status_code, error_message
		 FROM request_log WHERE success=0 AND created_at >= ?
		 ORDER BY id DESC LIMIT ? OFFSET ?`,
		cutoff.Format("2006-01-02 15:04:05"), limit, offset,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []errorRow
	for rows.Next() {
		var e errorRow
		if err := rows.Scan(&e.ID, &e.CreatedAt, &e.AccountEmail, &e.Model, &e.StatusCode, &e.ErrorMessage); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// clearStats 清理统计记录。beforeDays <= 0 时全部清空，否则删除该天数之前的记录。
func clearStats(beforeDays int) (int64, error) {
	if statsDB == nil {
		return 0, fmt.Errorf("stats db not available")
	}
	var (
		res sql.Result
		err error
	)
	if beforeDays <= 0 {
		res, err = statsDB.Exec(`DELETE FROM request_log`)
	} else {
		cutoff := time.Now().AddDate(0, 0, -beforeDays).Format("2006-01-02 15:04:05")
		res, err = statsDB.Exec(`DELETE FROM request_log WHERE created_at < ?`, cutoff)
	}
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// daysCutoff 返回统计窗口起点：包含今天在内的 days 个自然日的 0 点。
// 与账号管理"今日=自然日 0 点起"口径一致（days=1 即今天 0 点）；
// days<=0 时默认 7。
func daysCutoff(days int) time.Time {
	if days <= 0 {
		days = 7
	}
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	return today.AddDate(0, 0, -(days - 1))
}

// atoiDefault 解析查询参数，失败或非正返回默认值。
func atoiDefault(s string, def int) int {
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

// ---------- 模型上下文限制 ----------

// getModelLimit 返回某模型的输入上下文限制（0=不限制）。未配置返回 0。
func getModelLimit(modelID string) int {
	if statsDB == nil || modelID == "" {
		return 0
	}
	var limit int
	_ = statsDB.QueryRow(`SELECT context_limit FROM model_limits WHERE model_id=?`, modelID).Scan(&limit)
	return limit
}

// setModelLimit 设置某模型的输入上下文限制（0=不限制）。
func setModelLimit(modelID string, limit int) error {
	if statsDB == nil {
		return fmt.Errorf("stats db not available")
	}
	if limit < 0 {
		limit = 0
	}
	_, err := statsDB.Exec(`INSERT INTO model_limits(model_id, context_limit) VALUES(?,?)
		ON CONFLICT(model_id) DO UPDATE SET context_limit=excluded.context_limit`,
		modelID, limit)
	return err
}

// allModelLimits 返回所有已配置限制的 map（model_id -> limit）。
func allModelLimits() map[string]int {
	out := map[string]int{}
	if statsDB == nil {
		return out
	}
	rows, err := statsDB.Query(`SELECT model_id, context_limit FROM model_limits`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var limit int
		if err := rows.Scan(&id, &limit); err == nil {
			out[id] = limit
		}
	}
	return out
}
