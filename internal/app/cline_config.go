package app

import (
	"encoding/json"
	"log"
	"os"
	"sync"

	"cline-go-proxy/internal/kit"
)

// ============ Cline 上游配置 ============

// clineConfigData 保留 Cline 通道与出站代理相关的运行时配置,持久化到
// data/.cline-config.json,镜像 Zen 的配置管理模式。代理列表为空时回落到
// HTTP_PROXY/HTTPS_PROXY 环境变量,再为空则直连。
type clineConfigData struct {
	Proxies       []string `json:"proxies"`       // http(s)/socks5 代理,轮询出口
	ProxyStrategy string   `json:"proxyStrategy"` // round_robin / random / fill
}

func defaultClineConfig() *clineConfigData {
	return &clineConfigData{
		ProxyStrategy: "round_robin",
	}
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
