package main

import (
	"encoding/json"
	"log"
	"sync"
	"time"
)

// 上游推荐模型列表端点：返回 free 模型供校验。
const recommendedModelsURL = "https://api.cline.bot/api/v1/ai/cline/recommended-models"

// freeModelsRefreshInterval 后台刷新间隔。
const freeModelsRefreshInterval = 30 * time.Minute

var (
	freeModels     map[string]bool // id -> true
	freeModelsMu   sync.RWMutex
)

type recommendedModelsResp struct {
	Recommended []modelItem `json:"recommended"`
	Free       []modelItem `json:"free"`
	ClinePass  []modelItem `json:"clinePass"`
}

type modelItem struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// initFreeModels 启动时立即拉取一次 free 模型列表，并启动 30 分钟后台 ticker。
func initFreeModels() {
	refreshFreeModels()
	go func() {
		ticker := time.NewTicker(freeModelsRefreshInterval)
		defer ticker.Stop()
		for range ticker.C {
			refreshFreeModels()
		}
	}()
}

// refreshFreeModels 拉取上游推荐模型并更新 free 缓存。失败保留旧缓存兜底。
func refreshFreeModels() {
	resp, err := httpClient.Get(recommendedModelsURL)
	if err != nil {
		log.Printf("free models fetch failed (keep old cache): %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		log.Printf("free models fetch bad status %d (keep old cache)", resp.StatusCode)
		return
	}

	var data recommendedModelsResp
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		log.Printf("free models decode failed (keep old cache): %v", err)
		return
	}

	set := make(map[string]bool, len(data.Free))
	for _, m := range data.Free {
		if m.ID != "" {
			set[m.ID] = true
		}
	}
	freeModelsMu.Lock()
	freeModels = set
	freeModelsMu.Unlock()
	log.Printf("free models updated: %d models", len(set))
}

// modelIsFree 精确判断 model 是否在 free 缓存中。
func modelIsFree(model string) bool {
	if model == "" {
		return false
	}
	freeModelsMu.RLock()
	defer freeModelsMu.RUnlock()
	return freeModels != nil && freeModels[model]
}

// freeModelsReady 返回 free 缓存是否已成功填充（启动早期未就绪时不拦截）。
func freeModelsReady() bool {
	freeModelsMu.RLock()
	defer freeModelsMu.RUnlock()
	return len(freeModels) > 0
}

// listFreeModels 返回当前 free 缓存的 id 列表，供 admin 展示。
func listFreeModels() []string {
	freeModelsMu.RLock()
	defer freeModelsMu.RUnlock()
	out := make([]string, 0, len(freeModels))
	for id := range freeModels {
		out = append(out, id)
	}
	return out
}
