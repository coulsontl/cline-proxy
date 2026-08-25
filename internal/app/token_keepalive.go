package app

import (
	"log"
	"time"
)

// tokenKeepaliveLoop 每 1 小时检查一次，在 access_token 距过期不足 2 小时时
// 主动刷新，使 refresh_token 滚动续命（refresh_token 无独立过期时间，靠持续刷新续命）。
// 失败标记 expired，不影响主流程。
func tokenKeepaliveLoop() {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()
	for range ticker.C {
		keepaliveAccounts()
	}
}

// keepaliveAccounts 扫描所有非 expired 账号，对距 access_token 过期不足 2 小时
// （或 token 为空/已过期）的账号主动刷新。复用 refreshAccountToken 滚动续 refresh_token。
func keepaliveAccounts() {
	if statsDB == nil {
		return
	}
	rows, err := statsDB.Query(`SELECT account_id, email, refresh_token, access_token, expires_at, status
		FROM accounts WHERE status != 'expired'`)
	if err != nil {
		log.Printf("keepalive query: %v", err)
		return
	}

	type keepaliveAccount struct {
		accountID, email, refreshToken, accessToken string
		expiresAt                                    int64
		status                                       string
	}
	var list []keepaliveAccount
	for rows.Next() {
		var a keepaliveAccount
		if err := rows.Scan(&a.accountID, &a.email, &a.refreshToken, &a.accessToken, &a.expiresAt, &a.status); err == nil {
			list = append(list, a)
		}
	}
	rows.Close()

	now := time.Now().UnixMilli()
	const refreshLeadMs = 2 * 60 * 60 * 1000 // 2 小时：距过期不足此值则刷新
	for _, a := range list {
		// access_token 未到期且距过期 > 2h：跳过，避免无谓刷新
		if a.accessToken != "" && a.expiresAt > now && (a.expiresAt-now) > refreshLeadMs {
			continue
		}
		// 距过期 < 2h 或已过期/空 token：主动刷新
		acc := &Account{
			AccountID:    a.accountID,
			Email:         a.email,
			RefreshToken: a.refreshToken,
			AccessToken:  a.accessToken,
			ExpiresAt:    a.expiresAt,
			Status:       a.status,
		}
		if err := refreshAccountToken(acc); err != nil {
			// refreshAccountToken 内部已 UPDATE status='expired'
			log.Printf("keepalive: %s refresh failed: %v (marked expired)", truncateEmail(a.email), err)
			continue
		}
		log.Printf("keepalive: %s token refreshed (expires_at=%d)", truncateEmail(a.email), acc.ExpiresAt)
	}
}
