package app

import (
	"encoding/json"
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
	}})
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

	var patch struct {
		Proxies       []string `json:"proxies"`
		ProxyStrategy *string  `json:"proxyStrategy"`
	}
	if err := json.Unmarshal(body, &patch); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid JSON: " + err.Error()})
		return
	}

	if err := validateProxyList(patch.Proxies); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
		return
	}

	cur := getClineConfig()
	next := *cur
	next.Proxies = patch.Proxies
	if patch.ProxyStrategy != nil && *patch.ProxyStrategy != "" {
		switch *patch.ProxyStrategy {
		case "round_robin", "random", "fill":
			next.ProxyStrategy = *patch.ProxyStrategy
		default:
			writeAPI(w, http.StatusBadRequest, apiResponse{Error: "proxyStrategy 必须是 round_robin/random/fill"})
			return
		}
	}
	setClineConfig(&next)
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Message: "Cline 代理配置已保存", Data: map[string]any{
		"proxies":       next.Proxies,
		"proxyStrategy": next.ProxyStrategy,
	}})
}
