package app

import (
	"encoding/json"
	"net/http"
)

// GET /admin/api/model-limits 返回 free 模型列表 + 各自的上下文限制
func handleModelLimits(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	limits := allModelLimits()
	ids := listFreeModels()
	// 合并：free 列表每个模型带上其限制（未配置为0），外加已配置但不在 free 列表的
	combined := map[string]int{}
	for _, id := range ids {
		combined[id] = limits[id]
	}
	for id, lim := range limits {
		if _, ok := combined[id]; !ok {
			combined[id] = lim
		}
	}
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{
		"models": combined,
	}})
}

// POST /admin/api/model-limits body: {modelId, limit}  设置某模型上下文限制
func handleModelLimitUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	var req struct {
		ModelID string `json:"modelId"`
		Limit   int    `json:"limit"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid JSON"})
		return
	}
	if req.ModelID == "" {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "modelId is required"})
		return
	}
	if err := setModelLimit(req.ModelID, req.Limit); err != nil {
		writeAPI(w, http.StatusInternalServerError, apiResponse{Error: err.Error()})
		return
	}
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Message: "limit saved"})
}
