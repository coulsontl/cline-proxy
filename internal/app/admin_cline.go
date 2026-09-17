package app

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// ============ Cline 上游代理配置 API ============

// GET /admin/api/cline/config
func handleClineConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	cfg := getClineConfig()
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{
		"proxies":       cfg.Proxies,
		"proxyStrategy": cfg.ProxyStrategy,
		"retryCount":    clineRetryCount(),
		"maxRetryCount": maxClineRetryCount,
	}})
}

// clineConfigPatch 配置更新的部分字段：nil = 这次请求不改这个字段。
type clineConfigPatch struct {
	Proxies       *[]string `json:"proxies"`
	ProxyStrategy *string   `json:"proxyStrategy"`
	RetryCount    *int      `json:"retryCount"`
}

// applyClineConfigPatch 把部分更新合并到当前配置上。未出现的字段保持原值——
// 否则只改 retryCount 的 POST 会把代理列表一起清空（代理清空 = 所有出口回落
// 到环境变量/直连），一个字段的更新不该有这种副作用。
func applyClineConfigPatch(cur *clineConfigData, patch clineConfigPatch) (*clineConfigData, error) {
	if patch.Proxies != nil {
		if err := validateProxyList(*patch.Proxies); err != nil {
			return nil, err
		}
	}
	if patch.RetryCount != nil {
		if *patch.RetryCount < 0 || *patch.RetryCount > maxClineRetryCount {
			return nil, fmt.Errorf("retryCount 必须在 0-%d 之间", maxClineRetryCount)
		}
	}

	next := *cur
	if patch.Proxies != nil {
		next.Proxies = *patch.Proxies
	}
	if patch.RetryCount != nil {
		retry := *patch.RetryCount
		next.RetryCount = &retry
	}
	if patch.ProxyStrategy != nil && *patch.ProxyStrategy != "" {
		switch *patch.ProxyStrategy {
		case "round_robin", "random", "fill":
			next.ProxyStrategy = *patch.ProxyStrategy
		default:
			return nil, fmt.Errorf("proxyStrategy 必须是 round_robin/random/fill")
		}
	}
	return &next, nil
}

// POST /admin/api/cline/config/update
// 代理列表为空时 Cline 通道回落到 HTTP_PROXY/HTTPS_PROXY 环境变量,再为空则直连。
func handleClineConfigUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
		return
	}
	defer r.Body.Close()

	var patch clineConfigPatch
	if err := json.Unmarshal(body, &patch); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid JSON: " + err.Error()})
		return
	}

	next, err := applyClineConfigPatch(getClineConfig(), patch)
	if err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
		return
	}
	setClineConfig(next)
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Message: "Cline 代理配置已保存", Data: map[string]any{
		"proxies":       next.Proxies,
		"proxyStrategy": next.ProxyStrategy,
		"retryCount":    clineRetryCount(),
	}})
}
