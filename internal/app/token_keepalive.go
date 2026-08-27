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

// keepaliveAccounts 扫描 active 与 expired 账号：
//   - active: 距 access_token 过期不足 2h（或 token 空/已过期）时主动刷新
//   - expired: 无条件重试刷新（网络抖动导致误标 expired 时给恢复机会）
//
// 复用 refreshAccountToken 滚动续 refresh_token。网络错误不标 expired（refreshAccountToken 内处理）。
func keepaliveAccounts() {
	if statsDB == nil {
		return
	}
	rows, err := statsDB.Query(`SELECT account_id, email, refresh_token, access_token, expires_at, status
		FROM accounts WHERE status IN ('active','expired')`)
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
	log.Printf("keepalive: scan %d accounts (active+expired)", len(list))

	now := time.Now().UnixMilli()
	const refreshLeadMs = 2 * 60 * 60 * 1000 // 2 小时：距过期不足此值则刷新
	recovered, refreshed, failed := 0, 0, 0
	for _, a := range list {
		// expired 账号无条件重试；active 账号距过期 > 2h 且 token 非空则跳过
		if a.status == "active" && a.accessToken != "" && a.expiresAt > now && (a.expiresAt-now) > refreshLeadMs {
			continue
		}
		acc := &Account{
			AccountID:    a.accountID,
			Email:         a.email,
			RefreshToken: a.refreshToken,
			AccessToken:  a.accessToken,
			ExpiresAt:    a.expiresAt,
			Status:       a.status,
		}
		if err := refreshAccountToken(acc); err != nil {
			failed++
			log.Printf("keepalive: %s refresh failed (status=%s): %v", truncateEmail(a.email), a.status, err)
			continue
		}
		if a.status == "expired" {
			recovered++
			log.Printf("keepalive: %s RECOVERED from expired (expires_at=%d)", truncateEmail(a.email), acc.ExpiresAt)
		} else {
			refreshed++
			log.Printf("keepalive: %s token refreshed (expires_at=%d)", truncateEmail(a.email), acc.ExpiresAt)
		}
	}
	log.Printf("keepalive: done (scanned=%d, refreshed=%d, recovered=%d, failed=%d)", len(list), refreshed, recovered, failed)
}
