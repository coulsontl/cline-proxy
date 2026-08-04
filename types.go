package main

import "time"

type Account struct {
	AccountID     string    `json:"accountId"`
	Email         string    `json:"email"`
	RefreshToken  string    `json:"refreshToken"`
	AccessToken   string    `json:"-"`
	ExpiresAt     int64     `json:"-"`
	Status        string    `json:"status"` // active, cooldown, expired
	CooldownUntil int64     `json:"cooldownUntil"`
	FailCount     int       `json:"failCount"`
	LastUsed      time.Time `json:"lastUsed"`
	UsageCount    int64     `json:"usageCount"`
	CreatedAt     time.Time `json:"createdAt"`
}

type LoginMethod int

const (
	MethodDeviceOAuth LoginMethod = iota
	MethodRefreshToken
	MethodSSOCookie
)
