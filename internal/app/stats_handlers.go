package app

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// GET /admin/api/stats/usage?days=7  返回概览 + 按天趋势
func handleStatsUsage(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	days := atoiDefault(r.URL.Query().Get("days"), 7)
	cutoff := daysCutoff(days)
	overview, err := queryUsageOverview(cutoff)
	if err != nil {
		writeAPI(w, http.StatusInternalServerError, apiResponse{Error: "query overview: " + err.Error()})
		return
	}
	trend, err := queryUsageTrend(cutoff)
	if err != nil {
		writeAPI(w, http.StatusInternalServerError, apiResponse{Error: "query trend: " + err.Error()})
		return
	}
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{
		"days":     days,
		"overview": overview,
		"trend":    trend,
	}})
}

// GET /admin/api/stats/by-account?days=7  按账号汇总
func handleStatsByAccount(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	days := atoiDefault(r.URL.Query().Get("days"), 7)
	cutoff := daysCutoff(days)
	accounts, err := queryByAccount(cutoff)
	if err != nil {
		writeAPI(w, http.StatusInternalServerError, apiResponse{Error: "query by-account: " + err.Error()})
		return
	}
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{"accounts": accounts}})
}

// GET /admin/api/stats/by-model?days=7  按模型汇总
func handleStatsByModel(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	days := atoiDefault(r.URL.Query().Get("days"), 7)
	cutoff := daysCutoff(days)
	models, err := queryByModel(cutoff)
	if err != nil {
		writeAPI(w, http.StatusInternalServerError, apiResponse{Error: "query by-model: " + err.Error()})
		return
	}
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{"models": models}})
}

// GET /admin/api/stats/errors?days=7&limit=50&offset=0  错误明细列表
func handleStatsErrors(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	days := atoiDefault(r.URL.Query().Get("days"), 7)
	limit := atoiDefault(r.URL.Query().Get("limit"), 50)
	offset := atoiDefault(r.URL.Query().Get("offset"), 0)
	cutoff := daysCutoff(days)
	errors, err := queryErrors(cutoff, limit, offset)
	if err != nil {
		writeAPI(w, http.StatusInternalServerError, apiResponse{Error: "query errors: " + err.Error()})
		return
	}
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{
		"errors": errors,
		"total":  len(errors),
	}})
}

// POST /admin/api/stats/clear  body: {beforeDays}  清理统计（<=0 全清）
func handleStatsClear(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	var req struct {
		BeforeDays int `json:"beforeDays"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		// 容错：无 body 视为全清
		req.BeforeDays = 0
	}
	n, err := clearStats(req.BeforeDays)
	if err != nil {
		writeAPI(w, http.StatusInternalServerError, apiResponse{Error: err.Error()})
		return
	}
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Message: fmt.Sprintf("cleared %d rows", n)})
}
