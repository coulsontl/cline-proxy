package app

import (
	"encoding/json"
	"strings"
	"testing"
)

// 设置页现在一次发三个字段（proxies + proxyStrategy + retryCount），
// 但接口必须容忍只发其中一个：早期实现里 proxies 是无条件覆盖的，
// 于是 `{"retryCount":3}` 会把代理列表清空（出口回落环境变量/直连）。
// 这里锁住"未出现的字段保持原值、显式空数组仍然表示清空"。

func patchFromJSON(t *testing.T, raw string) clineConfigPatch {
	t.Helper()
	var p clineConfigPatch
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatalf("unmarshal patch: %v", err)
	}
	return p
}

func baseClineConfig() *clineConfigData {
	retry := 1
	return &clineConfigData{
		Proxies:       []string{"http://127.0.0.1:1080", "socks5://127.0.0.1:1081"},
		ProxyStrategy: "fill",
		RetryCount:    &retry,
	}
}

func TestApplyClineConfigPatchKeepsUnmentionedFields(t *testing.T) {
	cur := baseClineConfig()

	next, err := applyClineConfigPatch(cur, patchFromJSON(t, `{"retryCount":3}`))
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(next.Proxies) != 2 || next.Proxies[0] != "http://127.0.0.1:1080" {
		t.Fatalf("proxies were clobbered by a retryCount-only update: %v", next.Proxies)
	}
	if next.ProxyStrategy != "fill" {
		t.Fatalf("proxyStrategy = %q, want fill", next.ProxyStrategy)
	}
	if next.RetryCount == nil || *next.RetryCount != 3 {
		t.Fatalf("retryCount not applied: %v", next.RetryCount)
	}

	// 原配置不能被就地改动（getClineConfig 返回的是全局指针）
	if len(cur.Proxies) != 2 || *cur.RetryCount != 1 {
		t.Fatalf("patch mutated the current config: %+v", cur)
	}
}

func TestApplyClineConfigPatchEmptyProxiesStillClears(t *testing.T) {
	next, err := applyClineConfigPatch(baseClineConfig(), patchFromJSON(t, `{"proxies":[]}`))
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(next.Proxies) != 0 {
		t.Fatalf("explicit empty proxies list should clear: %v", next.Proxies)
	}
	if next.RetryCount == nil || *next.RetryCount != 1 {
		t.Fatalf("retryCount should be preserved: %v", next.RetryCount)
	}
}

func TestApplyClineConfigPatchSettingsPageShape(t *testing.T) {
	next, err := applyClineConfigPatch(baseClineConfig(), patchFromJSON(t,
		`{"proxies":["socks5://10.0.0.1:1080"],"proxyStrategy":"random","retryCount":0}`))
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(next.Proxies) != 1 || next.Proxies[0] != "socks5://10.0.0.1:1080" {
		t.Fatalf("proxies = %v", next.Proxies)
	}
	if next.ProxyStrategy != "random" {
		t.Fatalf("proxyStrategy = %q", next.ProxyStrategy)
	}
	if next.RetryCount == nil || *next.RetryCount != 0 {
		t.Fatalf("explicit 0 must be honoured, got %v", next.RetryCount)
	}
}

func TestApplyClineConfigPatchCommitTimeout(t *testing.T) {
	// 未出现 → 保持原值（不因为改了别的字段被清成 nil/默认值）
	next, err := applyClineConfigPatch(baseClineConfig(), patchFromJSON(t, `{"retryCount":2}`))
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if next.CommitTimeoutSec != nil {
		t.Fatalf("unmentioned commitTimeoutSec must stay nil, got %v", *next.CommitTimeoutSec)
	}

	// 显式 0（关闭超时兜底）必须被保留，不能被当成"未配置"
	next, err = applyClineConfigPatch(baseClineConfig(), patchFromJSON(t, `{"commitTimeoutSec":0}`))
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if next.CommitTimeoutSec == nil || *next.CommitTimeoutSec != 0 {
		t.Fatalf("explicit 0 must be honoured, got %v", next.CommitTimeoutSec)
	}
}

func TestApplyClineConfigPatchRejectsInvalidValues(t *testing.T) {
	cases := []struct {
		name  string
		patch string
		want  string
	}{
		{"retryCount negative", `{"retryCount":-1}`, "retryCount"},
		{"retryCount above max", `{"retryCount":99}`, "retryCount"},
		{"commitTimeout negative", `{"commitTimeoutSec":-1}`, "commitTimeoutSec"},
		{"commitTimeout above max", `{"commitTimeoutSec":601}`, "commitTimeoutSec"},
		{"bad strategy", `{"proxyStrategy":"sticky"}`, "proxyStrategy"},
		{"bad proxy url", `{"proxies":["127.0.0.1:1080"]}`, "代理"},
	}
	for _, tc := range cases {
		next, err := applyClineConfigPatch(baseClineConfig(), patchFromJSON(t, tc.patch))
		if err == nil {
			t.Fatalf("%s: expected error, got %+v", tc.name, next)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: error %q does not mention %q", tc.name, err, tc.want)
		}
	}
}
