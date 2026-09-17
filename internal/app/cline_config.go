package app

import (
	"encoding/json"
	"log"
	"os"
	"sync"
	"time"

	"cline-go-proxy/internal/kit"
)

// ============ Cline 上游配置 ============

// clineConfigData 保留 Cline 通道与出站代理相关的运行时配置,持久化到
// data/.cline-config.json,镜像 Zen 的配置管理模式。代理列表为空时回落到
// HTTP_PROXY/HTTPS_PROXY 环境变量,再为空则直连。
type clineConfigData struct {
	Proxies       []string `json:"proxies"`       // http(s)/socks5 代理,轮询出口
	ProxyStrategy string   `json:"proxyStrategy"` // round_robin / random / fill
	// RetryCount 是空回/上游 5xx/网络错误时换号重试的次数（0 = 不重试）。
	// 用指针区分「未配置」（nil → 默认值）与「显式设为 0」。
	RetryCount *int `json:"retryCount,omitempty"`
	// CommitTimeoutSec 是流式"提交前最多等多久"（秒）。超过它就先给客户端发 header
	// （避免客户端的首字节超时），代价是这次请求不能再换号重试。0 = 关闭，默认 60。
	CommitTimeoutSec *int `json:"commitTimeoutSec,omitempty"`
}

const (
	defaultClineRetryCount = 1 // 默认重试一次
	maxClineRetryCount     = 5 // 上限，避免设置页填出离谱值把一次请求拉爆

	defaultCommitTimeoutSec = 60  // 提交前最多等 60s
	maxCommitTimeoutSec     = 600 // 上限 10 分钟
)

func defaultClineConfig() *clineConfigData {
	retry := defaultClineRetryCount
	commitTimeout := defaultCommitTimeoutSec
	return &clineConfigData{
		ProxyStrategy:    "round_robin",
		RetryCount:       &retry,
		CommitTimeoutSec: &commitTimeout,
	}
}

// clineCommitTimeout 返回生效的"提交前等待"时长（0 = 关闭超时兜底）。
func clineCommitTimeout() time.Duration {
	cfg := getClineConfig()
	if cfg == nil || cfg.CommitTimeoutSec == nil {
		return defaultCommitTimeoutSec * time.Second
	}
	n := *cfg.CommitTimeoutSec
	if n < 0 {
		n = 0
	}
	if n > maxCommitTimeoutSec {
		n = maxCommitTimeoutSec
	}
	return time.Duration(n) * time.Second
}

// clineRetryCount 返回生效的重试次数（未配置或越界时收敛到合法范围）。
func clineRetryCount() int {
	cfg := getClineConfig()
	if cfg == nil || cfg.RetryCount == nil {
		return defaultClineRetryCount
	}
	n := *cfg.RetryCount
	if n < 0 {
		n = 0
	}
	if n > maxClineRetryCount {
		n = maxClineRetryCount
	}
	return n
}

var (
	clineConfig   = loadClineConfig()
	clineConfigMu sync.Mutex
)

func loadClineConfig() *clineConfigData {
	cfg := defaultClineConfig()
	if data, err := os.ReadFile(kit.ResolveDataPath(".cline-config.json")); err == nil {
		var c clineConfigData
		if json.Unmarshal(data, &c) == nil {
			if c.ProxyStrategy == "" {
				c.ProxyStrategy = "round_robin"
			}
			cfg = &c
		}
	}
	// 启动即按配置重建 Cline 通道 transport(含 env-var 回落)
	kit.RebuildHTTPClient(cfg.Proxies, cfg.ProxyStrategy)
	return cfg
}

func saveClineConfig() {
	clineConfigMu.Lock()
	defer clineConfigMu.Unlock()
	data, _ := json.MarshalIndent(clineConfig, "", "  ")
	if err := os.WriteFile(kit.ResolveDataPath(".cline-config.json"), data, 0600); err != nil {
		log.Printf("cline config save failed: %v", err)
	}
}

func getClineConfig() *clineConfigData {
	clineConfigMu.Lock()
	defer clineConfigMu.Unlock()
	return clineConfig
}

func setClineConfig(c *clineConfigData) {
	clineConfigMu.Lock()
	clineConfig = c
	clineConfigMu.Unlock()
	saveClineConfig()
	kit.RebuildHTTPClient(c.Proxies, c.ProxyStrategy)
}
