package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"
)

// 冷却退避参数：基准 10 秒，连续失败指数退避 ×2，封顶 60 秒。
const (
	cooldownBaseSec = 10
	cooldownMaxSec  = 60
)

// calcCooldownSec 根据连续失败次数计算退避秒数。
// failCount 从 1 起：1→10s, 2→20s, 3→40s, 4→80s→封顶 60s。
func calcCooldownSec(failCount int) int {
	sec := cooldownBaseSec
	for i := 1; i < failCount && sec < cooldownMaxSec; i++ {
		sec *= 2
	}
	if sec > cooldownMaxSec {
		sec = cooldownMaxSec
	}
	return sec
}

// poolPath 不再指向 JSON 文件，仅保留旧文件路径用于自动迁移备份。
var oldAccountsPath string

func init() {
	exe, _ := os.Executable()
	oldAccountsPath = filepath.Join(filepath.Dir(exe), ".cline-accounts.json")
}

// resolveDataPath 数据文件路径解析：优先可执行文件目录，其次当前工作目录。
// go run 运行时编译产物在临时目录，此时应回退到工作目录（项目根）查找数据文件。
func resolveDataPath(filename string) string {
	candidates := []string{}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), filename))
	}
	if pwd, err := os.Getwd(); err == nil {
		candidates = append(candidates, filepath.Join(pwd, filename))
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	// 都不存在：默认写到当前工作目录（go run 场景），保证数据落盘位置稳定
	if len(candidates) > 1 {
		return candidates[1]
	}
	return candidates[0]
}

// AccountPool 仅作为返回容器供遍历/状态统计，不再是持久层。
// 所有写操作各自走 SQL（accounts / api_keys / proxy_state 表）。
// 结构体定义见 types.go。

// loadPool 从数据库装载账号池与密钥到内存结构，供遍历与统计。
// 每次调用都查库（账号数量不大，简单优先于缓存一致性）。
func loadPool() *AccountPool {
	p := &AccountPool{Accounts: []*Account{}, Keys: []string{}}
	if statsDB == nil {
		return p
	}

	rows, err := statsDB.Query(`SELECT account_id, email, refresh_token, access_token,
		expires_at, status, cooldown_until, fail_count, usage_count, usage_count_today, usage_date,
		last_used, created_at, last_reason
		FROM accounts`)
	if err != nil {
		log.Printf("loadPool query accounts: %v", err)
		return p
	}
	defer rows.Close()
	for rows.Next() {
		var a Account
		var lastUsed, createdAt, cooldownMillis int64
		if err := rows.Scan(&a.AccountID, &a.Email, &a.RefreshToken, &a.AccessToken,
			&a.ExpiresAt, &a.Status, &cooldownMillis, &a.FailCount, &a.UsageCount,
			&a.UsageCountToday, &a.UsageDate, &lastUsed, &createdAt, &a.LastReason); err != nil {
			log.Printf("loadPool scan: %v", err)
			continue
		}
		if lastUsed > 0 {
			a.LastUsed = time.UnixMilli(lastUsed)
		}
		if createdAt > 0 {
			a.CreatedAt = time.UnixMilli(createdAt)
		}
		if cooldownMillis > 0 {
			a.CooldownUntil = time.UnixMilli(cooldownMillis)
		}
		// 冷却到期自动恢复（不改库，pickAccount 里统一写库恢复）
		if a.Status == "cooldown" && !a.CooldownUntil.IsZero() && time.Now().After(a.CooldownUntil) {
			a.Status = "active"
			a.CooldownUntil = time.Time{}
			a.LastReason = ""
		}
		p.Accounts = append(p.Accounts, &a)
	}

	keyRows, err := statsDB.Query(`SELECT key FROM api_keys ORDER BY created_at`)
	if err != nil {
		log.Printf("loadPool query keys: %v", err)
		return p
	}
	defer keyRows.Close()
	for keyRows.Next() {
		var k string
		if err := keyRows.Scan(&k); err == nil {
			p.Keys = append(p.Keys, k)
		}
	}

	_ = statsDB.QueryRow(`SELECT current_idx FROM proxy_state WHERE id=1`).Scan(&p.CurrentIdx)
	return p
}

// setDefaultModel 更新默认模型（内存全局）。模型需在 free 缓存中存在才生效。
func setDefaultModel(modelID string) {
	initModelsCache()
	modelsMu.Lock()
	_, ok := modelsCache[modelID]
	modelsMu.Unlock()
	if !ok {
		return
	}
	defaultModel = modelID
}

// savePool 保留为空操作以兼容旧调用点。账户各操作已即时写库，无需全量保存。
func savePool() {}

// savePoolLocked 保留为空操作以兼容上游旧调用点（本仓库用 SQLite，无需文件持久化）。
func savePoolLocked() {}

// addAccount 插入一个账号。返回 error 以便调用方据实反馈失败，不再静默吞错。
func addAccount(acc *Account) error {
	if statsDB == nil {
		return fmt.Errorf("stats db not available")
	}
	if acc == nil {
		return fmt.Errorf("nil account")
	}
	if acc.CreatedAt.IsZero() {
		acc.CreatedAt = time.Now()
	}
	cooldown := int64(0)
	if !acc.CooldownUntil.IsZero() {
		cooldown = acc.CooldownUntil.UnixMilli()
	}
	_, err := statsDB.Exec(`INSERT INTO accounts
		(account_id, email, refresh_token, access_token, expires_at, status,
		 cooldown_until, fail_count, usage_count, usage_count_today, usage_date,
		 last_used, created_at, last_reason)
		VALUES (?,?,?,?,?,'active',?,0,0,0,'',?,?,?)`,
		acc.AccountID, acc.Email, acc.RefreshToken, acc.AccessToken, acc.ExpiresAt,
		cooldown, acc.LastUsed.UnixMilli(), acc.CreatedAt.UnixMilli(), acc.LastReason)
	if err != nil {
		return fmt.Errorf("insert account: %w", err)
	}
	acc.UsageCount = 0
	acc.UsageCountToday = 0
	acc.UsageDate = time.Now().Format("2006-01-02")
	acc.FailCount = 0
	acc.CooldownUntil = time.Time{}
	return nil
}

// removeAccount 删除账号。
func removeAccount(accountID string) bool {
	if statsDB == nil {
		return false
	}
	res, err := statsDB.Exec(`DELETE FROM accounts WHERE account_id=?`, accountID)
	if err != nil {
		log.Printf("removeAccount: %v", err)
		return false
	}
	n, _ := res.RowsAffected()
	return n > 0
}

// getAccountByID 按 ID 查账号返回（含 token）。
func getAccountByID(accountID string) *Account {
	if statsDB == nil {
		return nil
	}
	var a Account
	var lastUsed, createdAt, cooldownMillis int64
	err := statsDB.QueryRow(`SELECT account_id, email, refresh_token, access_token,
		expires_at, status, cooldown_until, fail_count, usage_count, usage_count_today, usage_date,
		last_used, created_at, last_reason
		FROM accounts WHERE account_id=?`, accountID).Scan(
		&a.AccountID, &a.Email, &a.RefreshToken, &a.AccessToken,
		&a.ExpiresAt, &a.Status, &cooldownMillis, &a.FailCount, &a.UsageCount,
		&a.UsageCountToday, &a.UsageDate, &lastUsed, &createdAt, &a.LastReason)
	if err != nil {
		return nil
	}
	if lastUsed > 0 {
		a.LastUsed = time.UnixMilli(lastUsed)
	}
	if createdAt > 0 {
		a.CreatedAt = time.UnixMilli(createdAt)
	}
	if cooldownMillis > 0 {
		a.CooldownUntil = time.UnixMilli(cooldownMillis)
	}
	return &a
}

// refreshAccountToken 刷新账号 token 并写库。失败标记 expired。
func refreshAccountToken(acc *Account) error {
	resp, err := refreshClineToken(acc.RefreshToken)
	if err != nil {
		acc.Status = "expired"
		_, _ = statsDB.Exec(`UPDATE accounts SET status='expired' WHERE account_id=?`, acc.AccountID)
		return fmt.Errorf("token refresh failed: %w", err)
	}

	acc.AccessToken = "workos:" + resp.Data.AccessToken
	if resp.Data.RefreshToken != "" {
		acc.RefreshToken = resp.Data.RefreshToken
	}
	acc.ExpiresAt = parseExpiry(resp.Data.ExpiresAt) - 60000
	acc.Status = "active"
	_, err = statsDB.Exec(`UPDATE accounts SET access_token=?, refresh_token=?, expires_at=?, status='active',
		fail_count=0, cooldown_until=0, last_reason='' WHERE account_id=?`,
		acc.AccessToken, acc.RefreshToken, acc.ExpiresAt, acc.AccountID)
	if err != nil {
		log.Printf("refreshAccountToken update: %v", err)
	}
	return nil
}

// pickAccount 按策略选号，冷却到期账号自动恢复。
func pickAccount() *Account {
	if statsDB == nil {
		return nil
	}
	// 先把已到期的冷却账号恢复为 active（自动写库）
	now := time.Now()
	_, _ = statsDBExec(`UPDATE accounts SET status='active', fail_count=0, cooldown_until=0, last_reason=''
		WHERE status='cooldown' AND cooldown_until>0 AND ? >= cooldown_until`, now.UnixMilli())

	p := loadPool()
	active := make([]*Account, 0)
	for _, a := range p.Accounts {
		if a.Status == "active" {
			active = append(active, a)
		}
	}
	if len(active) == 0 {
		return nil
	}

	cfg := getProxyConfig()
	var acc *Account
	switch cfg.Strategy {
	case "fill":
		acc = active[0]
	case "random":
		n := time.Now().UnixNano() % int64(len(active))
		acc = active[n]
	default: // round_robin
		idx := p.CurrentIdx
		if idx >= len(active) {
			idx = 0
		}
		acc = active[idx]
		next := (idx + 1) % len(active)
		_, _ = statsDBExec(`UPDATE proxy_state SET current_idx=? WHERE id=1`, next)
	}
	return acc
}

// markCooldown 429 时调用：失败计数递增，按指数退避设定冷却截止时间。
// 若上游返回了明确等待时长 duration，则优先使用该时长；否则按 failCount 指数退避。
func markCooldown(acc *Account, duration time.Duration, reason string) {
	if statsDB == nil || acc == nil {
		return
	}
	acc.FailCount++
	sec := calcCooldownSec(acc.FailCount)
	var until time.Time
	if duration > 0 {
		until = time.Now().Add(duration)
	} else {
		until = time.Now().Add(time.Duration(sec) * time.Second)
	}
	acc.CooldownUntil = until
	acc.LastReason = reason
	acc.Status = "cooldown"
	_, _ = statsDB.Exec(`UPDATE accounts SET status='cooldown', fail_count=?, cooldown_until=?, last_reason=? WHERE account_id=?`,
		acc.FailCount, until.UnixMilli(), reason, acc.AccountID)
	log.Printf("  account %s cooldown until %s (fail #%d)", truncateEmail(acc.Email), until.Format("2006-01-02 15:04:05"), acc.FailCount)
}

// markAccountCooldown 将账号置为冷却状态，并记录预计恢复时间与原因。
// duration 为冷却时长；duration<=0 时使用默认冷却（18 小时）。
func markAccountCooldown(acc *Account, reason string, duration time.Duration) {
	if acc == nil {
		return
	}
	if duration <= 0 {
		duration = 18 * time.Hour // 默认 18 小时（Cline 免费额度每日重置）
	}
	until := time.Now().Add(duration)
	acc.Status = "cooldown"
	acc.CooldownUntil = until
	acc.LastReason = reason
	if statsDB != nil {
		_, _ = statsDB.Exec(`UPDATE accounts SET status='cooldown', cooldown_until=?, last_reason=? WHERE account_id=?`,
			until.UnixMilli(), reason, acc.AccountID)
	}
	log.Printf("  account %s cooldown until %s (reason: %s)", truncateEmail(acc.Email), until.Format("2006-01-02 15:04:05"), reason)
}

// markSuccess 成功时调用：重置失败计数与冷却，递增本地累计/今日调用次数（跨日自动重置）。
func markSuccess(acc *Account) {
	if statsDB == nil || acc == nil {
		return
	}
	now := time.Now()
	today := now.Format("2006-01-02")
	acc.FailCount = 0
	acc.Status = "active"
	acc.LastUsed = now
	acc.UsageCount++
	if acc.UsageDate != today {
		acc.UsageDate = today
		acc.UsageCountToday = 0
	}
	acc.UsageCountToday++
	_, _ = statsDB.Exec(`UPDATE accounts SET status='active', fail_count=0, cooldown_until=0, last_reason='',
		usage_count=usage_count+1,
		usage_count_today=CASE WHEN usage_date=? THEN usage_count_today+1 ELSE 1 END,
		usage_date=?, last_used=? WHERE account_id=?`,
		today, today, now.UnixMilli(), acc.AccountID)
}

// resetTodayUsage 仅重置本地今日调用计数，不影响累计调用次数。
func resetTodayUsage(acc *Account) {
	if acc == nil {
		return
	}
	acc.UsageCountToday = 0
	acc.UsageDate = time.Now().Format("2006-01-02")
	if statsDB != nil {
		_, _ = statsDB.Exec(`UPDATE accounts SET usage_count_today=0, usage_date=? WHERE account_id=?`,
			acc.UsageDate, acc.AccountID)
	}
}

// describePoolStatus 汇总当前账号池状态，用于错误诊断。
func describePoolStatus() string {
	p := loadPool()
	total := len(p.Accounts)
	if total == 0 {
		return "pool is empty, use --add-account or admin API to add accounts"
	}

	active, cooldown, expired := 0, 0, 0
	var nextRecover *time.Time
	for _, a := range p.Accounts {
		switch a.Status {
		case "active":
			active++
		case "cooldown":
			cooldown++
			if !a.CooldownUntil.IsZero() {
				if nextRecover == nil || a.CooldownUntil.Before(*nextRecover) {
					t := a.CooldownUntil
					nextRecover = &t
				}
			}
		case "expired":
			expired++
		}
	}

	s := fmt.Sprintf("total=%d active=%d cooldown=%d expired=%d", total, active, cooldown, expired)
	if cooldown > 0 && nextRecover != nil {
		s += fmt.Sprintf(", earliest recover at %s", nextRecover.Format("2006-01-02 15:04:05"))
	}
	return s
}

// ensureAccountToken 确保 token 有效，必要时刷新。
func ensureAccountToken(acc *Account) (string, error) {
	if acc.AccessToken != "" && time.Now().UnixMilli() < acc.ExpiresAt {
		return acc.AccessToken, nil
	}
	if err := refreshAccountToken(acc); err != nil {
		return "", err
	}
	return acc.AccessToken, nil
}

// listAccounts 返回脱敏账号列表（不含 token）。
func listAccounts() []*Account {
	p := loadPool()
	result := make([]*Account, 0, len(p.Accounts))
	for _, a := range p.Accounts {
		result = append(result, &Account{
			AccountID:       a.AccountID,
			Email:           a.Email,
			Status:          a.Status,
			CooldownUntil:   a.CooldownUntil,
			LastReason:      a.LastReason,
			FailCount:       a.FailCount,
			LastUsed:        a.LastUsed,
			UsageCount:      a.UsageCount,
			UsageCountToday: a.UsageCountToday,
			UsageDate:       a.UsageDate,
			CreatedAt:       a.CreatedAt,
		})
	}
	return result
}

// ---- API 密钥操作 ----

func addKey(key string) {
	if statsDB == nil || key == "" {
		return
	}
	_, _ = statsDB.Exec(`INSERT OR IGNORE INTO api_keys(key, created_at) VALUES(?, ?)`,
		key, time.Now().UnixMilli())
}

func removeKey(key string) bool {
	if statsDB == nil {
		return false
	}
	res, _ := statsDB.Exec(`DELETE FROM api_keys WHERE key=?`, key)
	n, _ := res.RowsAffected()
	return n > 0
}

// ---- 自动迁移旧 JSON ----

// migrateOldAccounts 启动时若库中无账号且存在旧 JSON，导入一次（保留旧文件作备份）。
func migrateOldAccounts() {
	if statsDB == nil {
		return
	}
	var cnt int
	_ = statsDB.QueryRow(`SELECT COUNT(*) FROM accounts`).Scan(&cnt)
	if cnt > 0 {
		return // 库已有数据，不重复导入
	}

	data, err := os.ReadFile(oldAccountsPath)
	if err != nil {
		return // 无旧文件，正常
	}
	var p AccountPool
	if err := json.Unmarshal(data, &p); err != nil {
		log.Printf("migrate: parse old JSON failed: %v", err)
		return
	}
	if len(p.Accounts) == 0 && len(p.Keys) == 0 {
		return
	}

	for _, a := range p.Accounts {
		cooldown := int64(0)
		if !a.CooldownUntil.IsZero() {
			cooldown = a.CooldownUntil.UnixMilli()
		}
		_, _ = statsDB.Exec(`INSERT INTO accounts
			(account_id, email, refresh_token, access_token, expires_at, status,
			 cooldown_until, fail_count, usage_count, usage_count_today, usage_date,
			 last_used, created_at, last_reason)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			a.AccountID, a.Email, a.RefreshToken, a.AccessToken, a.ExpiresAt,
			a.Status, cooldown, a.FailCount, a.UsageCount, a.UsageCountToday, a.UsageDate,
			a.LastUsed.UnixMilli(), a.CreatedAt.UnixMilli(), a.LastReason)
	}
	for _, k := range p.Keys {
		addKey(k)
	}
	if p.CurrentIdx > 0 {
		_, _ = statsDB.Exec(`UPDATE proxy_state SET current_idx=? WHERE id=1`, p.CurrentIdx)
	}
	log.Printf("migrated %d accounts, %d keys from old JSON (kept as backup)", len(p.Accounts), len(p.Keys))
}

// statsDBExec 是 statsDB.Exec 的小包裹，便于省略 nil 判断。
func statsDBExec(query string, args ...any) (sql.Result, error) {
	if statsDB == nil {
		return nil, fmt.Errorf("stats db not available")
	}
	return statsDB.Exec(query, args...)
}

// ---- OAuth 设备码登录添加账号（供 -login / -add-account CLI 模式）----

func addAccountFromDeviceAuth() (*Account, error) {
	fmt.Println("\n=== Add New Cline Account (OAuth) ===")

	device, err := workosDeviceAuth()
	if err != nil {
		return nil, err
	}

	authURL := device.VerificationURIComplete
	if authURL == "" {
		authURL = device.VerificationURI
	}

	fmt.Println("  1. Open this URL in your browser:")
	fmt.Println("     " + authURL)
	fmt.Println("  2. Enter code: " + device.UserCode)
	fmt.Println("  3. Log in with Google, GitHub, or email")

	_ = openBrowser(authURL)
	fmt.Println("  Waiting for authorization...")

	interval := device.Interval
	if interval < 5 {
		interval = 5
	}
	expiresIn := device.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 300
	}

	workosTok, err := pollWorkosToken(device.DeviceCode, interval, expiresIn)
	if err != nil {
		return nil, err
	}

	fmt.Println("  WorkOS authorized. Registering with Cline...")

	cline, err := registerWithCline(workosTok.AccessToken, workosTok.RefreshToken)
	if err != nil {
		return nil, err
	}

	if cline.Data.RefreshToken == "" {
		return nil, fmt.Errorf("cline registration missing refresh token")
	}

	email := "unknown"
	if cline.Data.UserInfo != nil && cline.Data.UserInfo.Email != "" {
		email = cline.Data.UserInfo.Email
	}

	acc := &Account{
		AccountID:    fmt.Sprintf("acc_%d", time.Now().UnixMilli()),
		Email:        email,
		RefreshToken: cline.Data.RefreshToken,
		AccessToken:  "workos:" + cline.Data.AccessToken,
		ExpiresAt:    parseExpiry(cline.Data.ExpiresAt) - 60000,
		Status:       "active",
		CreatedAt:    time.Now(),
	}

	if err := addAccount(acc); err != nil {
		fmt.Printf("  Account add failed: %v\n", err)
		return nil, err
	}
	fmt.Printf("  Account added! Email: %s\n", email)
	return acc, nil
}
