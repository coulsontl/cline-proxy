package app

import (
	"cline-go-proxy/internal/cline"
	"cline-go-proxy/internal/kit"
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

var defaultModel = "deepseek/deepseek-v4-flash"

// clineAPIBase 是 cline 上游基址。做成变量（而非直接用 cline.ClineAPIBase）
// 是为了让测试能把请求指向 httptest 假上游，端到端验证重试路径。
var clineAPIBase = cline.ClineAPIBase

var proxyListenAddress = "0.0.0.0:3457"

const (
	defaultMaxTokens       = 128000
	defaultReasoningEffort = "high"
)

var passThroughKeys = []string{
	"tools", "tool_choice", "parallel_tool_calls", "functions", "function_call",
	"temperature", "top_p", "top_k", "stop", "presence_penalty", "frequency_penalty",
	"response_format", "user", "n", "logit_bias", "seed", "logprobs", "top_logprobs",
	"stream_options", "metadata",
}

type chatRequest struct {
	Model               string          `json:"model"`
	Messages            json.RawMessage `json:"messages"`
	Stream              bool            `json:"stream,omitempty"`
	MaxTokens           int             `json:"max_tokens,omitempty"`
	MaxCompletionTokens int             `json:"max_completion_tokens,omitempty"`
	Tools               json.RawMessage `json:"tools,omitempty"`
	ToolChoice          json.RawMessage `json:"tool_choice,omitempty"`
	ReasoningEffort     string          `json:"reasoning_effort,omitempty"`
	ReasoningEffortAlt  string          `json:"reasoningEffort,omitempty"`
	Extra               map[string]any  `json:"-"`
}

func StartProxy(host string, port int) error {
	if strings.TrimSpace(host) == "" {
		host = "0.0.0.0"
	}
	initLogFile()

	p := loadPool()
	activeCount := 0
	for _, a := range p.Accounts {
		if a.Status == "active" {
			// Try to pre-warm tokens
			if a.AccessToken == "" || time.Now().UnixMilli() >= a.ExpiresAt {
				if err := refreshAccountToken(a); err != nil {
					log.Printf("  Pre-warm failed for %s: %v", a.Email, err)
					continue
				}
			}
			activeCount++
		}
	}
	log.Printf("Loaded %d active accounts from pool", activeCount)

	// 后台定期拉取上游 free 模型列表并校验请求模型。
	// startModelsRefresher 在下方启动，initFreeModels 由 models.go 取代。

	freePort(port)

	startModelsRefresher()
	startZenModelsRefresher()
	if err := InitStats(); err != nil {
		log.Printf("WARNING: stats db init failed: %v (database features disabled)", err)
	}
	MigrateOldAccounts()
	LoadRequestLogsFromFile()
	go cleanupCompactStates()
	go tokenKeepaliveLoop()

	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, "/admin/", http.StatusFound)
	})

	mux.HandleFunc("/v1/health", corsHandler(func(w http.ResponseWriter, r *http.Request) {
		info := map[string]any{
			"status":         "ok",
			"version":        "go-1.1",
			"activeAccounts": activeCount,
		}
		writeJSON(w, http.StatusOK, info)
	}))
	mux.HandleFunc("/health", corsHandler(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":         "ok",
			"version":        "go-1.1",
			"activeAccounts": activeCount,
		})
	}))

	// Admin API (frontend + REST)
	registerAdminRoutes(mux)

	apiKeyHandler := func(next http.HandlerFunc) http.HandlerFunc {
		return corsHandler(func(w http.ResponseWriter, r *http.Request) {
			// Allow requests without key if no keys configured
			p := loadPool()
			if len(p.Keys) == 0 {
				next(w, r)
				return
			}

			key := r.Header.Get("x-api-key")
			if key == "" {
				if b := r.Header.Get("Authorization"); len(b) > 7 && b[:7] == "Bearer " {
					key = b[7:]
				}
			}

			valid := false
			for _, k := range p.Keys {
				if k == key {
					valid = true
					break
				}
			}

			if !valid {
				writeJSON(w, http.StatusUnauthorized, map[string]any{
					"error": map[string]string{
						"message": "invalid API key. Generate one at /admin/ or set x-api-key header",
						"type":    "auth_error",
					},
				})
				return
			}
			next(w, r)
		})
	}

	modelsHandler := apiKeyHandler(func(w http.ResponseWriter, r *http.Request) {
		ensureModelsFresh()
		data := apiModelList()
		// 合并 zen 免费模型
		cfg := getZenConfig()
		if cfg.Enabled {
			for _, zm := range zenModelList() {
				data = append(data, map[string]any{
					"id":       zm["id"],
					"object":   "model",
					"created":  time.Now().UnixMilli(),
					"owned_by": "opencode-zen",
					"source":   "zen-free",
					"status":   "active",
					"cost":     "free",
					"context":  zm["context"],
					"output":   zm["output"],
				})
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
	})
	mux.HandleFunc("/v1/models", modelsHandler)
	mux.HandleFunc("/models", modelsHandler)

	chatHandler := apiKeyHandler(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		if activeCount == 0 && len(loadPool().Accounts) == 0 {
			insertRequestRecord(&requestContext{
				apiFormat: "openai", accountEmail: "no_account", startAt: time.Now(),
			}, tokenUsage{}, false, 401, "no accounts in pool")
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"error": map[string]string{
					"message": "No accounts in pool. Run with --add-account or POST /admin/login to add accounts.",
					"type":    "auth_error",
				},
			})
			return
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": map[string]string{"message": err.Error(), "type": "parse_error"},
			})
			return
		}

		var params map[string]any
		if err := json.Unmarshal(body, &params); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": map[string]string{"message": err.Error(), "type": "parse_error"},
			})
			return
		}

		isStream, _ := params["stream"].(bool)
		toolCount := 0
		if tools, ok := params["tools"]; ok {
			if t, ok := tools.([]any); ok {
				toolCount = len(t)
			}
		}
		model, _ := params["model"].(string)
		log.Printf("  client: stream=%v tools=%d model=%s", isStream, toolCount, model)

		// Override system prompt from override.md for OpenAI format
		applyOverride(params)

		// zen 免费模型路由
		if route := routeModel(model); route == "zen" {
			handleZenChat(w, r, params)
			return
		} else if route == "reject" {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": map[string]string{"message": fmt.Sprintf("model %q is a paid zen model; only free zen models are proxied", model), "type": "invalid_request_error"},
			})
			return
		}

		// 输入 token 上限校验：超限直接拒绝，不请求上游
		if limit := getModelLimit(model); limit > 0 {
			inputTokens := countRequestTokens(params)
			if inputTokens > limit {
				msg := fmt.Sprintf("input tokens %d exceeds model %s context limit %d", inputTokens, model, limit)
				insertRequestRecord(&requestContext{
					apiFormat: "openai", accountEmail: "no_account",
					model: model, isStream: isStream, startAt: time.Now(),
				}, tokenUsage{promptTokens: inputTokens}, false, 413, msg)
				writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{
					"error": map[string]string{"message": msg, "type": "context_limit_exceeded"},
				})
				return
			}
		}

		upstreamStream := isStream
		if !isStream {
			m := getDefaultModel()
			if mm, ok := params["model"].(string); ok && mm != "" {
				m = normalizeRequestModel(mm)
			}
			if modelNeedsStream(m) {
				upstreamStream = true
				log.Printf("  model %s requires stream: forcing upstream stream, will aggregate", m)
			}
		}

		serveClineChat(w, params, isStream, upstreamStream, callClineAPI)
	})
	mux.HandleFunc("/v1/chat/completions", chatHandler)
	mux.HandleFunc("/chat/completions", chatHandler)

	// Anthropic Messages API support
	anthropicHandler := apiKeyHandler(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		handleAnthropicMessages(w, r)
	})
	mux.HandleFunc("/v1/messages", anthropicHandler)
	mux.HandleFunc("/messages", anthropicHandler)

	// OpenAI Responses API
	responsesHandler := apiKeyHandler(handleResponses)
	mux.HandleFunc("/v1/responses", responsesHandler)
	mux.HandleFunc("/responses", responsesHandler)

	if host == "" {
		host = "0.0.0.0"
	}
	addr := fmt.Sprintf("%s:%d", host, port)
	proxyListenAddress = addr
	server := &http.Server{
		Addr:    addr,
		Handler: requestLogMiddleware(mux),
	}

	fmt.Println("")
	fmt.Println(strings.Repeat("=", 58))
	fmt.Println("  Cline Go Proxy v1.0 - No CLI Required")
	fmt.Println(strings.Repeat("=", 58))
	fmt.Printf("  Listen: http://%s\n", addr)
	fmt.Printf("  API:    http://%s/v1\n", addr)
	fmt.Println("  API Key: any value")
	fmt.Printf("  Model:   %s (auto-detected)\n", getDefaultModel())
	fmt.Printf("  Accounts: %d total, %d active\n", len(loadPool().Accounts), activeCount)
	fmt.Println(strings.Repeat("=", 58))

	return server.ListenAndServe()
}

// initLogFile 将日志同时输出到控制台与 cline-proxy.log（追加模式），
// 控制台窗口滚动内容有限，文件可完整保留所有日志。
func initLogFile() {
	path := kit.ResolveDataPath("cline-proxy.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		log.Printf("  open log file failed: %v", err)
		return
	}
	log.SetOutput(io.MultiWriter(os.Stderr, f))
	log.Printf("========== proxy started, log file: %s ==========", path)
}

func corsHandler(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, x-api-key, anthropic-version, anthropic-beta")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		h(w, r)
	}
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

// applyOverride 用 override.md 替换系统提示词(不存在则跳过)
func applyOverride(params map[string]any) {
	override := loadOverrideContent()
	if override == "" {
		return
	}
	if msgs, ok := params["messages"].([]any); ok {
		found := false
		for _, m := range msgs {
			if mm, ok := m.(map[string]any); ok {
				if mm["role"] == "system" {
					mm["content"] = override
					found = true
					break
				}
			}
		}
		if !found {
			params["messages"] = append([]any{map[string]any{"role": "system", "content": override}}, msgs...)
		}
	}
}

// handleZenChat opencode zen 免费模型分支: 压缩 -> 上游 -> 透传,并记录统计
func handleZenChat(w http.ResponseWriter, r *http.Request, params map[string]any) {
	cfg := getZenConfig()
	if !cfg.Enabled {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": map[string]string{"message": "zen upstream disabled in /admin/ settings", "type": "api_error"},
		})
		return
	}
	model, _ := params["model"].(string)
	zm, ok := resolveZenFreeModel(model)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]string{"message": fmt.Sprintf("model %q is not a free zen model", model), "type": "invalid_request_error"},
		})
		return
	}
	isStream, _ := params["stream"].(bool)
	tracker := newZenStatsTracker(zenStatsRecord{
		TS:           time.Now().UnixMilli(),
		Upstream:     "zen",
		Model:        zm.ID,
		Stream:       isStream,
		PromptTokens: estimateJSON(params),
	})

	sid := requestSessionID(params, r.Header)
	out := maybeCompact(params, zm, sid)
	tracker.rec.Compacted = out.changed
	tracker.rec.CompactionTokens = out.compactTokens
	if out.changed {
		log.Printf("  zen: %s", out.note)
	}

	resp, rateLimited, err := callZenAPI(params, isStream)
	if err != nil {
		log.Printf("  zen api error: %v", err)
		tracker.rec.RateLimited = rateLimited
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": map[string]string{"message": err.Error(), "type": "api_error"},
		})
		tracker.finish(false, http.StatusBadGateway)
		return
	}
	tracker.rec.RateLimited = rateLimited
	defer resp.Body.Close()
	tracker.rec.Status = resp.StatusCode

	usageFn := func(u map[string]any) {
		if pt, ok := u["prompt_tokens"].(float64); ok {
			tracker.rec.CompletionTokens += int(pt) - tracker.rec.PromptTokens
			if tracker.rec.CompletionTokens < 0 {
				tracker.rec.CompletionTokens = 0
			}
		}
		if ct, ok := u["completion_tokens"].(float64); ok {
			tracker.rec.CompletionTokens = int(ct)
		}
	}

	if isStream {
		// zen 走 zen-stats.jsonl 统计，不写 SQLite request_log（两套统计不交叉）
		handleStreamResponse(w, resp, nil, usageFn)
		tracker.finish(true, resp.StatusCode)
		return
	}
	handleNonStreamResponse(w, resp, nil, usageFn)
	tracker.finish(true, resp.StatusCode)
}

func cleanMessages(messages []any) []any {
	cleaned := make([]any, 0, len(messages))
	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			cleaned = append(cleaned, m)
			continue
		}
		cleaned = append(cleaned, msg)
	}
	return cleaned
}

func buildUpstreamBody(params map[string]any, stream bool) map[string]any {
	sessionID := fmt.Sprintf("sess_%d", time.Now().UnixMilli())

	maxTokens := defaultMaxTokens
	if mt, ok := params["max_tokens"].(float64); ok {
		maxTokens = int(mt)
	} else if mt, ok := params["max_completion_tokens"].(float64); ok {
		maxTokens = int(mt)
	}

	model := getDefaultModel()
	if m, ok := params["model"].(string); ok && m != "" {
		model = normalizeRequestModel(m)
	}

	body := map[string]any{
		"model":            model,
		"max_tokens":       maxTokens,
		"session_id":       sessionID,
		"reasoning_effort": defaultReasoningEffort,
	}

	if msgsRaw, ok := params["messages"]; ok {
		if msgsArr, ok := msgsRaw.([]any); ok {
			body["messages"] = cleanMessages(msgsArr)
		} else {
			body["messages"] = msgsRaw
		}
	}

	if stream {
		body["stream"] = true
	}

	if re, ok := params["reasoning_effort"].(string); ok && re != "" {
		body["reasoning_effort"] = re
	} else if re, ok := params["reasoningEffort"].(string); ok && re != "" {
		body["reasoning_effort"] = re
	}

	for _, key := range passThroughKeys {
		if val, ok := params[key]; ok {
			body[key] = val
		}
	}

	return body
}

func clineHeaders(token, sessionID string) http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+token)
	h.Set("Content-Type", "application/json")
	h.Set("X-Task-ID", sessionID)

	cfg := getProxyConfig()
	for k, v := range cfg.Headers {
		h.Set(k, v)
	}

	return h
}

// clineAttempt 是可注入的上游调用形态（测试用假上游替换 callClineAPI）。
type clineAttempt func(params map[string]any, stream bool, excludeAccountIDs []string) (*http.Response, *Account, *requestContext, error)

// serveClineChat 处理 cline 渠道 /v1/chat/completions 的三种出口，并做空回检测：
// 上游返回空内容（200 但无 content/tool_calls/reasoning）或 5xx/网络错误时，
// 换一个账号重试（次数见设置页「空回/错误重试次数」，默认 1），仍失败返回 502。
//
// 关键点：检测发生在向客户端写出任何字节**之前**（流式靠 streamClineBuffered 的
// 提交前私有缓冲），所以失败时可以干净地改成 502，而不是给客户端一个
// 200 + 空 body（那只能报 stream ended without terminal event）。
func serveClineChat(w http.ResponseWriter, params map[string]any, isStream, upstreamStream bool, call clineAttempt) {
	retries := clineRetryCount()
	var (
		tried       []string // 已试过的账号 ID，重试时排除
		triedEmails []string // 仅用于日志/错误记录，便于排查
	)

	for attempt := 0; attempt <= retries; attempt++ {
		resp, acc, ctx, err := call(params, upstreamStream, tried)
		ctx.apiFormat = "openai"
		if err != nil {
			if attempt < retries && isRetryableClineError(err, ctx) {
				log.Printf("  retry %d/%d after upstream error (account %s): %v",
					attempt+1, retries, accountEmail(acc), err)
				tried = appendExcludedAccount(tried, acc)
				triedEmails = append(triedEmails, accountEmail(acc))
				continue
			}
			log.Printf("  api error: %v", err)
			insertRequestRecord(ctx, tokenUsage{}, false, ctx.statusCode, kit.Truncate(err.Error(), 2000))
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"error": map[string]string{"message": err.Error(), "type": "api_error"},
			})
			return
		}

		usageFn := accountUsageFn(acc, params)

		if isStream {
			outcome, _, fatalErr := streamClineBuffered(w, resp, ctx, usageFn)
			switch outcome {
			case streamCommitted:
				resp.Body.Close()
				return
			case streamFatal:
				resp.Body.Close()
				msg := "upstream stream aborted before any content"
				if fatalErr != nil {
					msg = fatalErr.Error()
				}
				writeEmptyResponseError(w, ctx, msg)
				return
			}
			resp.Body.Close()
			// streamEmpty：缓冲已被丢弃，客户端一个字节都没收到，可以换号重试
			if attempt < retries {
				log.Printf("  retry %d/%d after empty stream (account %s)",
					attempt+1, retries, accountEmail(acc))
				tried = appendExcludedAccount(tried, acc)
				triedEmails = append(triedEmails, acc.Email)
				continue
			}
			writeEmptyResponseError(w, ctx, emptyResponseMessage(triedEmails))
			return
		}

		if upstreamStream {
			out, sawContent, err := collectStreamResponse(resp)
			resp.Body.Close()
			if err != nil {
				insertRequestRecord(ctx, tokenUsage{}, false, http.StatusInternalServerError, kit.Truncate(err.Error(), 2000))
				writeJSON(w, http.StatusInternalServerError, map[string]any{
					"error": map[string]string{"message": err.Error(), "type": "parse_error"},
				})
				return
			}
			if !sawContent {
				if attempt < retries {
					log.Printf("  retry %d/%d after empty aggregated response (account %s)",
						attempt+1, retries, accountEmail(acc))
					tried = appendExcludedAccount(tried, acc)
					triedEmails = append(triedEmails, acc.Email)
					continue
				}
				writeEmptyResponseError(w, ctx, emptyResponseMessage(triedEmails))
				return
			}
			if u, ok := out["usage"].(map[string]any); ok && len(u) > 0 {
				usageFn(u)
			}
			out = normalizeOpenAIResponse(out)
			contentLen := 0
			if c, ok := getNested(out, "choices", 0, "message", "content").(string); ok {
				contentLen = len(c)
			}
			log.Printf("  nonstream (aggregated): model=%v content_len=%d finish=%v",
				out["model"], contentLen, getNested(out, "choices", 0, "finish_reason"))
			var u tokenUsage
			if usage, ok := out["usage"].(map[string]any); ok {
				extractOpenAIUsage(usage, &u)
			}
			writeJSON(w, http.StatusOK, out)
			insertRequestRecord(ctx, u, true, 200, "")
			return
		}

		out, u, err := decodeOpenAINonStream(resp, usageFn)
		resp.Body.Close()
		if err != nil {
			insertRequestRecord(ctx, tokenUsage{}, false, 0, "decode upstream: "+err.Error())
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"error": map[string]string{"message": err.Error(), "type": "parse_error"},
			})
			return
		}
		if !hasResponseContent(out) {
			if attempt < retries {
				log.Printf("  retry %d/%d after empty non-stream response (account %s)",
					attempt+1, retries, accountEmail(acc))
				tried = appendExcludedAccount(tried, acc)
				triedEmails = append(triedEmails, acc.Email)
				continue
			}
			writeEmptyResponseError(w, ctx, emptyResponseMessage(triedEmails))
			return
		}
		writeOpenAINonStream(w, out, u, ctx)
		return
	}
}

// appendExcludedAccount 把已试过的账号加入排除列表（重试时不再选它）。
func appendExcludedAccount(tried []string, acc *Account) []string {
	if acc == nil || acc.AccountID == "" {
		return tried
	}
	for _, id := range tried {
		if id == acc.AccountID {
			return tried
		}
	}
	return append(tried, acc.AccountID)
}

// isRetryableClineError 判定上游错误是否值得换号重试一次：
// 网络错误与 5xx（callClineAPI 把网络错误记为 502），以及上游把空回当 500 返回的情况。
// 4xx 不重试——401 已由 callClineAPI 内部刷新 token 重试过，429 已有冷却语义。
func isRetryableClineError(err error, ctx *requestContext) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errUpstreamEmptyResponse) {
		return true
	}
	return ctx != nil && ctx.statusCode >= 500
}

func emptyResponseMessage(triedEmails []string) string {
	if len(triedEmails) == 0 {
		return "upstream returned an empty response (no content)"
	}
	return fmt.Sprintf("upstream returned an empty response (no content) after trying %d account(s): %s",
		len(triedEmails)+1, strings.Join(triedEmails, ", "))
}

// writeEmptyResponseError 空回重试仍失败：客户端还没收到任何字节，干净地返回 502。
func writeEmptyResponseError(w http.ResponseWriter, ctx *requestContext, msg string) {
	log.Printf("  empty response after retry: %s", msg)
	insertRequestRecord(ctx, tokenUsage{}, false, http.StatusBadGateway, msg)
	writeJSON(w, http.StatusBadGateway, map[string]any{
		"error": map[string]string{"message": msg, "type": "empty_response"},
	})
}

// excludeAccountIDs 非空时选号会跳过这些账号（空回/上游错误换号重试用）。
func callClineAPI(params map[string]any, stream bool, excludeAccountIDs []string) (*http.Response, *Account, *requestContext, error) {
	model, _ := params["model"].(string)
	ctx := &requestContext{
		model:    model,
		isStream: stream,
		startAt:  time.Now(),
	}

	acc := pickAccountExcluding(excludeAccountIDs)
	if acc == nil {
		ctx.accountEmail = "no_account"
		return nil, nil, ctx, fmt.Errorf("no active accounts available: %s", describePoolStatus())
	}
	ctx.accountEmail = acc.Email

	token, err := ensureAccountToken(acc)
	if err != nil {
		// refreshAccountToken 内部已置 status=expired
		return nil, acc, ctx, fmt.Errorf("account %s token failed: %w", acc.Email, err)
	}

	body := buildUpstreamBody(params, stream)
	sessionID, _ := body["session_id"].(string)

	bodyJSON, err := json.Marshal(body)
	if err != nil {
		return nil, acc, ctx, fmt.Errorf("marshal body: %w", err)
	}

	req, err := http.NewRequest("POST", clineAPIBase+"/chat/completions", bytes.NewReader(bodyJSON))
	if err != nil {
		return nil, acc, ctx, fmt.Errorf("create request: %w", err)
	}
	req.Header = clineHeaders(token, sessionID)

	toolCount := 0
	if tools, ok := params["tools"]; ok {
		if t, ok := tools.([]any); ok {
			toolCount = len(t)
		}
	}
	log.Printf("  upstream: account=%s stream=%v tools=%d msgs=%d max_tokens=%v effort=%v",
		accountEmail(acc), stream, toolCount, getMsgCount(params), body["max_tokens"], body["reasoning_effort"])

	resp, err := kit.HTTPClient().Do(req)
	if err != nil {
		// 网络错误：临时短冷却 5 分钟
		markAccountCooldown(acc, "network error: "+err.Error(), 5*time.Minute)
		ctx.statusCode = http.StatusBadGateway
		return nil, acc, ctx, fmt.Errorf("upstream request: %w", err)
	}

	if resp.StatusCode == 401 {
		resp.Body.Close()
		// Refresh token and retry
		if err := refreshAccountToken(acc); err == nil {
			token = acc.AccessToken
			req.Header = clineHeaders(token, sessionID)
			resp, err = kit.HTTPClient().Do(req)
			if err != nil {
				return nil, acc, ctx, fmt.Errorf("upstream retry: %w", err)
			}
			if resp.StatusCode == 401 {
				resp.Body.Close()
				ctx.statusCode = 401
				acc.Status = "expired"
				_, _ = statsDBExec(`UPDATE accounts SET status='expired' WHERE account_id=?`, acc.AccountID)
				return nil, acc, ctx, fmt.Errorf("account %s token expired permanently", acc.Email)
			}
		} else {
			ctx.statusCode = 401
			acc.Status = "expired"
			_, _ = statsDBExec(`UPDATE accounts SET status='expired' WHERE account_id=?`, acc.AccountID)
			return nil, acc, ctx, fmt.Errorf("account %s refresh failed: %w", acc.Email, err)
		}
	}

	if resp.StatusCode != 200 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		ctx.statusCode = resp.StatusCode
		// 429 限流：优先使用上游返回的明确等待时长，否则指数退避
		if resp.StatusCode == 429 {
			reason := kit.Truncate(string(bodyBytes), 500)
			duration := parseInferenceCapDuration(string(bodyBytes))
			if duration <= 0 {
				duration = parseRetryAfter(resp.Header.Get("Retry-After"))
			}
			markCooldown(acc, duration, "429: "+reason)
			log.Printf("  account %s cooldown %v (reason: %s)", accountEmail(acc), duration, reason)
		}
		// 上游把"空回"当 500 返回时打上哨兵，供上层换号重试一次。
		// 注意要在 kit.Truncate 之前判断关键字，避免截断后丢标记。
		if isEmptyUpstreamError(resp.StatusCode, bodyBytes) {
			return nil, acc, ctx, fmt.Errorf("%w: API %d: %s",
				errUpstreamEmptyResponse, resp.StatusCode, kit.Truncate(string(bodyBytes), 500))
		}
		return nil, acc, ctx, fmt.Errorf("API %d: %s", resp.StatusCode, kit.Truncate(string(bodyBytes), 500))
	}

	ctx.statusCode = 200
	markSuccess(acc)
	return resp, acc, ctx, nil
}

// accountUsageFn 构造账号 token 记账回调：从上游 usage 提取
// prompt_tokens + completion_tokens，计入该账号今日/累计消耗。
func accountUsageFn(acc *Account, params map[string]any) func(map[string]any) {
	return func(u map[string]any) {
		var pt, ct float64
		if v, ok := u["prompt_tokens"].(float64); ok {
			pt = v
		}
		if v, ok := u["completion_tokens"].(float64); ok {
			ct = v
		}
		tokens := int64(pt + ct)
		if tokens <= 0 && params != nil {
			// 上游未返回 usage 时用入站请求估算兜底（与 zen 统计一致）
			tokens = int64(estimateJSON(params))
		}
		recordAccountTokens(acc, tokens)
	}
}

// accountEmail 取账号邮箱用于日志；acc 为 nil（如"池里没有可用账号"）时给出占位符，
// 避免在错误路径上因为 nil 账号而 panic。
func accountEmail(acc *Account) string {
	if acc == nil || acc.Email == "" {
		return "no_account"
	}
	return acc.Email
}

func truncateEmail(email string) string {
	if len(email) <= 12 {
		return email
	}
	parts := splitEmail(email)
	if len(parts) == 2 && len(parts[0]) > 3 {
		return parts[0][:3] + "***@" + parts[1]
	}
	if len(email) > 12 {
		return email[:8] + "..."
	}
	return email
}

func splitEmail(email string) []string {
	for i := 0; i < len(email); i++ {
		if email[i] == '@' {
			return []string{email[:i], email[i+1:]}
		}
	}
	return []string{email}
}

func getMsgCount(params map[string]any) int {
	if msgs, ok := params["messages"].([]any); ok {
		return len(msgs)
	}
	return 0
}

func handleStreamResponse(w http.ResponseWriter, upstream *http.Response, ctx *requestContext, onUsage func(map[string]any)) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(http.StatusOK)

	flusher, ok := w.(http.Flusher)
	if !ok {
		log.Printf("  streaming not supported for client")
		insertRequestRecord(ctx, tokenUsage{}, false, 0, "streaming not supported")
		return
	}

	var u tokenUsage
	reader := bufio.NewReader(upstream.Body)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				if line != "" {
					w.Write([]byte(line + "\n"))
				}
				break
			}
			// 上游中途断流：记录真实原因，否则只剩一个 200 + 空 body，
			// 客户端只能报笼统的 "stream ended without terminal event"。
			log.Printf("  upstream stream aborted: %v", err)
			insertRequestRecord(ctx, u, false, http.StatusBadGateway, "upstream stream aborted: "+err.Error())
			return
		}

		out, _, _ := processStreamLine(line, &u, onUsage)
		w.Write([]byte(out))
		flusher.Flush()
	}
	insertRequestRecord(ctx, u, true, 200, "")
}

// processStreamLine 处理一行上游 SSE，返回应下发给客户端的字节、归一化后的对象
// （能解析为 JSON 时非 nil）以及该行是否为终止事件（[DONE] 或 finish_reason）。
// 逐行处理逻辑从 handleStreamResponse 原样抽出，两条流式出口共用。
func processStreamLine(line string, u *tokenUsage, onUsage func(map[string]any)) (string, map[string]any, bool) {
	line = strings.TrimRight(line, "\r\n")

	if strings.HasPrefix(line, "data:") {
		payload := strings.TrimSpace(line[5:])
		if payload == "" || isDonePayload(payload) {
			return line + "\n\n", nil, isDonePayload(payload)
		}

		// Try to normalize the response
		var obj map[string]any
		if err := json.Unmarshal([]byte(payload), &obj); err == nil {
			if onUsage != nil {
				if usage, ok := obj["usage"].(map[string]any); ok && len(usage) > 0 {
					onUsage(usage)
				}
			}
			// Some Cline responses wrap in {data: {...}}
			if data, ok := obj["data"]; ok {
				if d, ok := data.(map[string]any); ok {
					if _, hasChoices := d["choices"]; hasChoices {
						obj = d
					}
					if _, hasID := d["id"]; hasID {
						obj = d
					}
				}
			}
			normalized := normalizeOpenAIResponse(obj)
			// 旁路提取 usage（不改 normalized，不改转发顺序）
			if usage, ok := normalized["usage"].(map[string]any); ok {
				extractOpenAIUsage(usage, u)
			}
			if normBytes, err := json.Marshal(normalized); err == nil {
				return "data: " + string(normBytes) + "\n\n", normalized, isTerminalChunk(normalized)
			}
		}
	}

	return line + "\n", nil, false
}

// streamOutcome 是"提交前缓冲"流式出口的三态结果。
type streamOutcome int

const (
	// streamCommitted 已提交并下发（含提交后中途断流：内部已记录 502），不可重试
	streamCommitted streamOutcome = iota
	// streamEmpty 未提交且始终没有内容 → 可换号重试
	streamEmpty
	// streamFatal 未提交但超出缓冲上限 / 客户端不支持流式 → 直接报错，不重试
	streamFatal
)

// 提交前私有缓冲上限，量级照抄 axonhub（maxPreCommitBufferedEvents/Bytes）。
// 只有"非内容事件"会被缓冲，且第一个 reasoning/content/tool_calls 即提交，
// 所以正常请求远达不到；达到上限说明上游在刷无意义事件，失败关闭而非无限缓存。
const (
	maxPreCommitEvents = 1024
	maxPreCommitBytes  = 8 << 20
)

// streamClineBuffered 以"提交前私有缓冲"方式转发 cline 上游流：
// 事件先缓冲不外发，直到出现有意义内容才提交（写 header + 回放缓冲）并继续流式；
// 若先等到终止事件（或 EOF）仍无内容，则丢弃缓冲返回 streamEmpty，让上层换号重试。
// 关键收益：失败发生在向客户端写出任何字节之前，因此可以干净地改成 502，
// 而不是给客户端一个 200 + 空 body（客户端只能报 stream ended without terminal event）。
func streamClineBuffered(w http.ResponseWriter, upstream *http.Response, ctx *requestContext, onUsage func(map[string]any)) (streamOutcome, tokenUsage, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		log.Printf("  streaming not supported for client")
		return streamFatal, tokenUsage{}, errors.New("client does not support streaming (no http.Flusher)")
	}

	var (
		u         tokenUsage
		buf       strings.Builder
		events    int
		committed bool
	)

	commit := func() {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.WriteHeader(http.StatusOK)
		if buf.Len() > 0 {
			w.Write([]byte(buf.String()))
		}
		flusher.Flush()
		committed = true
	}

	reader := bufio.NewReader(upstream.Body)
	for {
		line, err := reader.ReadString('\n')
		eos := err != nil
		if eos && err != io.EOF {
			if !committed {
				// 提交前上游断流：与空回同等对待，换号重试一次
				log.Printf("  pre-commit upstream stream error: %v", err)
				return streamEmpty, u, nil
			}
			log.Printf("  upstream stream aborted: %v", err)
			insertRequestRecord(ctx, u, false, http.StatusBadGateway, "upstream stream aborted: "+err.Error())
			return streamCommitted, u, nil
		}

		if line != "" {
			out, obj, terminal := processStreamLine(line, &u, onUsage)
			if committed {
				w.Write([]byte(out))
				flusher.Flush()
			} else {
				buf.WriteString(out)
				events++
				if obj != nil && hasChunkContent(obj) {
					commit()
				} else if terminal {
					// 终止事件但一路没有内容 → 空回，丢弃缓冲
					log.Printf("  empty response detected (events=%d)", events)
					return streamEmpty, u, nil
				} else if events > maxPreCommitEvents || buf.Len() > maxPreCommitBytes {
					log.Printf("  pre-commit buffer exceeded (events=%d bytes=%d)", events, buf.Len())
					return streamFatal, u, fmt.Errorf("pre-commit stream buffer limit exceeded (events=%d bytes=%d)", events, buf.Len())
				}
			}
		}

		if eos {
			break
		}
	}

	if !committed {
		// 既没有内容也没有终止事件就结束了（含上游 0 字节）：空回
		log.Printf("  empty response detected (events=%d, eof)", events)
		return streamEmpty, u, nil
	}
	insertRequestRecord(ctx, u, true, 200, "")
	return streamCommitted, u, nil
}

// decodeOpenAINonStream 读取并归一化非流式上游响应体。
// 抽成纯函数，让重试层能先判定空回再决定写出还是换号重试。
func decodeOpenAINonStream(upstream *http.Response, onUsage func(map[string]any)) (map[string]any, tokenUsage, error) {
	var raw map[string]any
	if err := json.NewDecoder(upstream.Body).Decode(&raw); err != nil {
		return nil, tokenUsage{}, err
	}

	if onUsage != nil {
		if usage, ok := raw["usage"].(map[string]any); ok && len(usage) > 0 {
			onUsage(usage)
		}
	}

	// Some Cline responses wrap in {data: {...}}
	out := raw
	if data, ok := raw["data"]; ok {
		if d, ok := data.(map[string]any); ok {
			out = d
		}
	}

	out = normalizeOpenAIResponse(out)

	var u tokenUsage
	if usage, ok := out["usage"].(map[string]any); ok {
		extractOpenAIUsage(usage, &u)
	}

	if msg, ok := getNested(out, "choices", 0, "message").(map[string]any); ok {
		tc, _ := msg["tool_calls"].([]any)
		content, _ := msg["content"].(string)
		log.Printf("  nonstream finish=%v tool_calls=%d content_len=%d",
			getNested(out, "choices", 0, "finish_reason"),
			len(tc), len(content))
	}

	return out, u, nil
}

// writeOpenAINonStream 下发非流式结果并记录成功。
func writeOpenAINonStream(w http.ResponseWriter, out map[string]any, u tokenUsage, ctx *requestContext) {
	writeJSON(w, http.StatusOK, out)
	insertRequestRecord(ctx, u, true, 200, "")
}

func handleNonStreamResponse(w http.ResponseWriter, upstream *http.Response, ctx *requestContext, onUsage func(map[string]any)) {
	out, u, err := decodeOpenAINonStream(upstream, onUsage)
	if err != nil {
		insertRequestRecord(ctx, tokenUsage{}, false, 0, "decode upstream: "+err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": map[string]string{"message": err.Error(), "type": "parse_error"},
		})
		return
	}
	writeOpenAINonStream(w, out, u, ctx)
}

// 返回的 bool 表示"整条流里是否出现过有意义内容"，供空回检测使用：
// reasoning 不计入输出 message（保持原有输出形状），但必须算内容，
// 否则"只有 reasoning 的答案"会被误判成空回。
func collectStreamResponse(upstream *http.Response) (map[string]any, bool, error) {
	var (
		model                string
		content              strings.Builder
		reasoning            strings.Builder
		finishReason         string
		usage                map[string]any
		toolCallAccumulators = make(map[int]*toolAccumulator)
	)

	reader := bufio.NewReader(upstream.Body)
	for {
		line, err := reader.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")

		if strings.HasPrefix(line, "data:") {
			payload := strings.TrimSpace(line[5:])
			if payload == "" || payload == "[DONE]" {
				if err == io.EOF {
					break
				}
				continue
			}
			var obj map[string]any
			if json.Unmarshal([]byte(payload), &obj) != nil {
				continue
			}
			if data, ok := obj["data"]; ok {
				if d, ok := data.(map[string]any); ok {
					obj = d
				}
			}
			if m, ok := obj["model"].(string); ok && m != "" {
				model = m
			}
			if u, ok := obj["usage"].(map[string]any); ok && len(u) > 0 {
				usage = u
			}
			choices, _ := getNested(obj, "choices").([]any)
			if len(choices) == 0 {
				continue
			}
			choice, _ := choices[0].(map[string]any)
			if choice == nil {
				continue
			}
			delta, _ := choice["delta"].(map[string]any)
			if delta == nil {
				delta = choice
			}
			if fr, ok := choice["finish_reason"].(string); ok && fr != "" {
				finishReason = fr
			}
			if c, ok := delta["content"].(string); ok && c != "" {
				content.WriteString(c)
			}
			if r, ok := delta["reasoning"].(string); ok && r != "" {
				reasoning.WriteString(r)
			}
			if r, ok := delta["reasoning_content"].(string); ok && r != "" {
				reasoning.WriteString(r)
			}
			if tcRaw, ok := delta["tool_calls"].([]any); ok {
				for _, tc := range tcRaw {
					tcMap, _ := tc.(map[string]any)
					if tcMap == nil {
						continue
					}
					idx := 0
					if i, ok := tcMap["index"].(float64); ok {
						idx = int(i)
					}
					accumulator, exists := toolCallAccumulators[idx]
					if !exists {
						accumulator = &toolAccumulator{index: idx}
						toolCallAccumulators[idx] = accumulator
					}
					if id, ok := tcMap["id"].(string); ok && id != "" {
						accumulator.id = id
					}
					if fn, ok := tcMap["function"].(map[string]any); ok {
						if n, ok := fn["name"].(string); ok && n != "" {
							accumulator.name = n
						}
						if a, ok := fn["arguments"].(string); ok && a != "" {
							accumulator.args += a
						}
					}
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, false, fmt.Errorf("read upstream stream: %w", err)
		}
	}

	toolCallIndexes := make([]int, 0, len(toolCallAccumulators))
	for index := range toolCallAccumulators {
		toolCallIndexes = append(toolCallIndexes, index)
	}
	sort.Ints(toolCallIndexes)
	toolCalls := make([]any, 0, len(toolCallIndexes))
	for _, index := range toolCallIndexes {
		accumulator := toolCallAccumulators[index]
		toolCalls = append(toolCalls, map[string]any{
			"id":   accumulator.id,
			"type": "function",
			"function": map[string]any{
				"name":      accumulator.name,
				"arguments": accumulator.args,
			},
		})
	}

	message := map[string]any{
		"role":    "assistant",
		"content": content.String(),
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}
	choice := map[string]any{
		"index":         0,
		"message":       message,
		"finish_reason": finishReason,
	}
	out := map[string]any{
		"id":      "chatcmpl_" + fmt.Sprintf("%x", time.Now().UnixMilli()),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{choice},
	}
	if usage != nil {
		out["usage"] = usage
	}
	sawContent := content.Len() > 0 || reasoning.Len() > 0 || len(toolCallAccumulators) > 0
	return out, sawContent, nil
}

func modelNeedsStream(modelID string) bool {
	initModelsCache()
	modelsMu.Lock()
	defer modelsMu.Unlock()
	if m, ok := modelsCache[modelID]; ok && m.RequiresStream {
		return true
	}
	return false
}

// Anthropic Messages API support
type anthropicMsg struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type toolAccumulator struct {
	index   int
	id      string
	name    string
	args    string
	emitted bool
}

type anthropicReq struct {
	Model       string          `json:"model"`
	MaxTokens   int             `json:"max_tokens"`
	Messages    []anthropicMsg  `json:"messages"`
	System      json.RawMessage `json:"system,omitempty"`
	Stream      bool            `json:"stream,omitempty"`
	Temperature float64         `json:"temperature,omitempty"`
	TopP        float64         `json:"top_p,omitempty"`
	TopK        int             `json:"top_k,omitempty"`
	Stop        json.RawMessage `json:"stop_sequences,omitempty"`
	Tools       json.RawMessage `json:"tools,omitempty"`
	ToolChoice  json.RawMessage `json:"tool_choice,omitempty"`
	Metadata    json.RawMessage `json:"metadata,omitempty"`
	Extra       map[string]any  `json:"-"`
}

func loadOverrideContent() string {
	data, err := os.ReadFile(overridePath)
	if err != nil {
		// override.md 是可选功能，文件不存在时静默使用客户端自带提示词
		return ""
	}
	content := strings.TrimSpace(string(data))
	if content != "" {
		log.Printf("  using override.md as system prompt (%d bytes)", len(content))
	} else {
		log.Printf("  override.md is empty, using client system prompt")
	}
	return content
}

func extractStringContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// Try string first
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	// Try array of content blocks
	var blocks []map[string]any
	if err := json.Unmarshal(raw, &blocks); err == nil {
		parts := []string{}
		for _, b := range blocks {
			if b["type"] == "text" {
				if t, ok := b["text"].(string); ok {
					parts = append(parts, t)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func anthropicToolsToOpenAI(tools []any) []any {
	out := make([]any, 0, len(tools))
	for _, t := range tools {
		if tMap, ok := t.(map[string]any); ok {
			// Already in OpenAI format
			if tMap["type"] == "function" {
				out = append(out, t)
				continue
			}
			// Convert Anthropic format to OpenAI
			oai := map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        tMap["name"],
					"description": tMap["description"],
					"parameters":  tMap["input_schema"],
				},
			}
			out = append(out, oai)
		}
	}
	return out
}

func anthropicToOpenAI(req anthropicReq) map[string]any {
	openAI := map[string]any{
		"model":      req.Model,
		"max_tokens": req.MaxTokens,
		"stream":     req.Stream,
		"messages":   []any{},
	}
	if req.Temperature != 0 {
		openAI["temperature"] = req.Temperature
	}
	if req.TopP != 0 {
		openAI["top_p"] = req.TopP
	}
	// Convert Anthropic tools to OpenAI format
	if req.Tools != nil {
		var toolsArr []any
		if err := json.Unmarshal(req.Tools, &toolsArr); err == nil {
			openAI["tools"] = anthropicToolsToOpenAI(toolsArr)
		}
	}
	if req.ToolChoice != nil {
		openAI["tool_choice"] = req.ToolChoice
	}

	msgs := []any{}

	// System prompt: use override.md if it exists, otherwise use Anthropic's system field
	sysContent := loadOverrideContent()
	if sysContent == "" && req.System != nil {
		sysContent = extractStringContent(req.System)
	}
	if sysContent != "" {
		log.Printf("  system prompt: %d bytes (from override.md)", len(sysContent))
		msgs = append(msgs, map[string]any{"role": "system", "content": sysContent})
	}

	for _, m := range req.Messages {
		switch c := m.Content.(type) {
		case string:
			msgs = append(msgs, map[string]any{"role": m.Role, "content": c})
		case []any:
			textParts := []string{}
			var toolCalls []any
			var toolResults []map[string]any

			for _, block := range c {
				if b, ok := block.(map[string]any); ok {
					switch b["type"] {
					case "text":
						if t, ok := b["text"].(string); ok {
							textParts = append(textParts, t)
						}
					case "image":
						// skip images
					case "tool_use":
						argsStr := "{}"
						if input, ok := b["input"]; ok && input != nil {
							if s, ok := input.(string); ok {
								argsStr = s
							} else if bts, err := json.Marshal(input); err == nil {
								argsStr = string(bts)
							}
						}
						tc := map[string]any{
							"id":   b["id"],
							"type": "function",
							"function": map[string]any{
								"name":      b["name"],
								"arguments": argsStr,
							},
						}
						toolCalls = append(toolCalls, tc)
					case "tool_result":
						toolCallID, _ := b["tool_use_id"].(string)
						if toolCallID == "" {
							continue
						}
						toolResults = append(toolResults, map[string]any{
							"role":         "tool",
							"content":      anthropicContentToString(b["content"]),
							"tool_call_id": toolCallID,
						})
					}
				}
			}

			if m.Role == "assistant" && len(toolCalls) > 0 {
				msg := map[string]any{
					"role":       "assistant",
					"content":    strings.Join(textParts, "\n"),
					"tool_calls": toolCalls,
				}
				msgs = append(msgs, msg)
				log.Printf("  anthropic req: assistant tool_calls=%d", len(toolCalls))
			} else if m.Role == "user" && len(toolResults) > 0 {
				for _, tr := range toolResults {
					msgs = append(msgs, tr)
					content, _ := tr["content"].(string)
					id, _ := tr["tool_call_id"].(string)
					log.Printf("  anthropic req: tool_result id=%s content_len=%d prefix=%s", id, len(content), kit.Truncate(content, 400))
				}
			} else {
				content := strings.Join(textParts, "\n")
				msgs = append(msgs, map[string]any{"role": m.Role, "content": content})
			}
		}
	}

	openAI["messages"] = msgs
	return openAI
}

// parseToolArgs 解析工具调用参数 JSON，带容错修复
func parseToolArgs(raw string) (any, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return map[string]any{}, nil
	}
	// 清理杂散引号前缀：上游流式输出偶发 "" 前缀（如 ""{"file_path":...}）
	for strings.HasPrefix(raw, `""`) {
		raw = strings.TrimPrefix(raw, `""`)
	}
	raw = strings.TrimSpace(raw)
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err == nil {
		if v == nil {
			return map[string]any{}, nil
		}
		return v, nil
	}
	// 整体被 JSON 字符串包裹（"{\"file_path\": ...}"）时，解包字符串后再解析
	if strings.HasPrefix(raw, `"`) && strings.HasSuffix(raw, `"`) && len(raw) >= 2 {
		var s string
		if json.Unmarshal([]byte(raw), &s) == nil {
			var v2 any
			if json.Unmarshal([]byte(s), &v2) == nil && v2 != nil {
				return v2, nil
			}
		}
	}
	fixed := raw
	if strings.HasPrefix(fixed, "{") && !strings.HasSuffix(fixed, "}") {
		fixed += "}"
	} else if strings.HasPrefix(fixed, "[") && !strings.HasSuffix(fixed, "]") {
		fixed += "]"
	}
	if strings.HasSuffix(fixed, ",") {
		fixed = strings.TrimRight(fixed, ",") + "}"
	}
	if err := json.Unmarshal([]byte(fixed), &v); err == nil && v != nil {
		return v, nil
	}
	// 最终兜底：从杂散内容中提取首个 JSON 对象/数组
	if i := strings.IndexAny(raw, "{["); i >= 0 {
		openCh := raw[i]
		closeCh := byte('}')
		if openCh == '[' {
			closeCh = ']'
		}
		if j := strings.LastIndex(raw, string(closeCh)); j > i {
			sub := raw[i : j+1]
			if json.Unmarshal([]byte(sub), &v) == nil && v != nil {
				return v, nil
			}
		}
	}
	return nil, fmt.Errorf("invalid json: %s", kit.Truncate(raw, 120))
}

// extractToolSchemas 从 Anthropic 请求的 tools 定义中解析每个工具的 input_schema 属性集合，
// 用于转发 tool_use 时裁剪 input，避免多余字段触发客户端校验失败。
func extractToolSchemas(tools json.RawMessage) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	if len(tools) == 0 {
		return out
	}
	var arr []map[string]any
	if err := json.Unmarshal(tools, &arr); err != nil {
		return out
	}
	for _, t := range arr {
		name, _ := t["name"].(string)
		if name == "" {
			continue
		}
		schema, _ := t["input_schema"].(map[string]any)
		props, _ := schema["properties"].(map[string]any)
		var propNames []string
		for k := range props {
			propNames = append(propNames, k)
		}
		var required []string
		if req, ok := schema["required"].([]any); ok {
			for _, r := range req {
				if s, ok := r.(string); ok {
					required = append(required, s)
				}
			}
		}
		log.Printf("  tool schema: name=%s properties=%v required=%v", name, propNames, required)
		if len(props) == 0 {
			continue
		}
		allowed := map[string]bool{}
		for k := range props {
			allowed[k] = true
		}
		out[name] = allowed
	}
	return out
}

// filterToolInput 将工具参数裁剪到客户端 schema 允许的字段内；
// 找不到 schema 或过滤后为空时保留原参数，避免丢参数。
func filterToolInput(name string, input map[string]any, schemas map[string]map[string]bool) map[string]any {
	allowed, ok := schemas[name]
	if !ok || len(allowed) == 0 {
		return input
	}
	out := map[string]any{}
	for k, v := range input {
		if allowed[k] {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return input
	}
	return out
}

// anthropicContentToString 将 Anthropic content（字符串或块数组）转为纯文本
func anthropicContentToString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	if arr, ok := v.([]any); ok {
		parts := []string{}
		for _, it := range arr {
			if b, ok := it.(map[string]any); ok {
				if t, ok := b["text"].(string); ok {
					parts = append(parts, t)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func openAIToAnthropic(openAI map[string]any) map[string]any {
	out := map[string]any{
		"id":    "msg_" + fmt.Sprintf("%x", time.Now().UnixMilli()),
		"type":  "message",
		"role":  "assistant",
		"model": getNested(openAI, "model"),
	}

	choices := getNested(openAI, "choices")
	if choices == nil {
		out["content"] = []any{map[string]any{"type": "text", "text": ""}}
		out["stop_reason"] = "end_turn"
		out["usage"] = map[string]any{"input_tokens": 0, "output_tokens": 0}
		return out
	}

	text := ""
	choice0, ok := getNested(openAI, "choices", 0).(map[string]any)
	if !ok {
		out["content"] = []any{map[string]any{"type": "text", "text": text}}
		out["stop_reason"] = "end_turn"
		out["usage"] = map[string]any{"input_tokens": 0, "output_tokens": 0}
		return out
	}
	msg, _ := choice0["message"].(map[string]any)
	if msg == nil {
		msg, _ = choice0["delta"].(map[string]any)
	}
	if msg != nil {
		if c, ok := msg["content"].(string); ok {
			text = sanitizeContent(c)
		}
	}

	contentBlocks := []any{map[string]any{"type": "text", "text": text}}

	// Convert tool_calls to Anthropic tool_use blocks
	if msg != nil {
		if tc, ok := msg["tool_calls"].([]any); ok && len(tc) > 0 {
			contentBlocks = []any{}
			if text != "" {
				contentBlocks = append(contentBlocks, map[string]any{"type": "text", "text": text})
			}
			for _, tcItem := range tc {
				if tcMap, ok := tcItem.(map[string]any); ok {
					funcData, _ := tcMap["function"].(map[string]any)
					if funcData == nil {
						continue
					}
					input := funcData["arguments"]
					// OpenAI arguments is a JSON string; Anthropic expects an object
					if argsStr, ok := input.(string); ok {
						var argsObj any
						if json.Unmarshal([]byte(argsStr), &argsObj) == nil {
							input = argsObj
						}
					}
					if input == nil {
						input = map[string]any{}
					}
					id, _ := tcMap["id"].(string)
					if id == "" {
						id = fmt.Sprintf("toolu_%x_%d", time.Now().UnixMilli(), len(contentBlocks))
					}
					name, _ := funcData["name"].(string)
					if name == "" {
						continue
					}
					block := map[string]any{
						"type":  "tool_use",
						"id":    id,
						"name":  name,
						"input": input,
					}
					contentBlocks = append(contentBlocks, block)
				}
			}
		}
	}

	out["content"] = contentBlocks

	switch getNested(openAI, "choices", 0, "finish_reason") {
	case "stop":
		out["stop_reason"] = "end_turn"
	case "length":
		out["stop_reason"] = "max_tokens"
	case "tool_calls":
		out["stop_reason"] = "tool_use"
	default:
		out["stop_reason"] = "end_turn"
	}

	usage := map[string]any{}
	if u := getNested(openAI, "usage"); u != nil {
		if um, ok := u.(map[string]any); ok {
			usage["input_tokens"] = um["prompt_tokens"]
			usage["output_tokens"] = um["completion_tokens"]
		}
	}
	out["usage"] = usage

	return out
}

func handleAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]string{"message": err.Error(), "type": "parse_error"},
		})
		return
	}

	var req anthropicReq
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]string{"message": err.Error(), "type": "parse_error"},
		})
		return
	}

	if len(req.Messages) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]string{"message": "messages is required", "type": "parse_error"},
		})
		return
	}

	toolSchemas := extractToolSchemas(req.Tools)
	if len(toolSchemas) > 0 {
		log.Printf("  anthropic tools: %d schemas", len(toolSchemas))
	}

	if req.MaxTokens == 0 {
		req.MaxTokens = defaultMaxTokens
	}

	openAIReq := anthropicToOpenAI(req)

	log.Printf("  anthropic: model=%s stream=%v msgs=%d", req.Model, req.Stream, len(req.Messages))

	// zen 免费模型路由
	if route := routeModel(req.Model); route == "zen" {
		handleZenAnthropic(w, r, req, openAIReq, toolSchemas)
		return
	} else if route == "reject" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]string{"message": fmt.Sprintf("model %q is a paid zen model; only free zen models are proxied", req.Model), "type": "invalid_request_error"},
		})
		return
	}

	// 输入 token 上限校验：超限直接拒绝，不请求上游
	if limit := getModelLimit(req.Model); limit > 0 {
		inputTokens := countRequestTokens(openAIReq)
		if inputTokens > limit {
			msg := fmt.Sprintf("input tokens %d exceeds model %s context limit %d", inputTokens, req.Model, limit)
			insertRequestRecord(&requestContext{
				apiFormat: "anthropic", accountEmail: "no_account",
				model: req.Model, isStream: req.Stream, startAt: time.Now(),
			}, tokenUsage{promptTokens: inputTokens}, false, 413, msg)
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{
				"error": map[string]string{"message": msg, "type": "context_limit_exceeded"},
			})
			return
		}
	}

	activeCount := 0
	p := loadPool()
	for _, a := range p.Accounts {
		if a.Status == "active" {
			activeCount++
		}
	}

	if activeCount == 0 && len(p.Accounts) == 0 {
		insertRequestRecord(&requestContext{
			apiFormat: "anthropic", accountEmail: "no_account",
			model: req.Model, isStream: req.Stream, startAt: time.Now(),
		}, tokenUsage{}, false, 401, "no accounts in pool")
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"error": map[string]string{
				"message": "No accounts in pool",
				"type":    "auth_error",
			},
		})
		return
	}

	upstreamStream := req.Stream
	if !req.Stream && modelNeedsStream(normalizeRequestModel(req.Model)) {
		upstreamStream = true
		log.Printf("  anthropic model %s requires stream: forcing upstream stream, will aggregate", req.Model)
	}

	resp, acc, ctx, err := callClineAPI(openAIReq, upstreamStream, nil)
	ctx.apiFormat = "anthropic"
	if err != nil {
		log.Printf("  anthropic api error: %v", err)
		insertRequestRecord(ctx, tokenUsage{}, false, ctx.statusCode, kit.Truncate(err.Error(), 2000))
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": map[string]string{"message": err.Error(), "type": "api_error"},
		})
		return
	}
	defer resp.Body.Close()

	usageFn := accountUsageFn(acc, openAIReq)

	if req.Stream {
		handleAnthropicStream(w, resp, ctx, normalizeRequestModel(req.Model), toolSchemas, usageFn)
		return
	}

	if upstreamStream {
		out, _, err := collectStreamResponse(resp)
		if err != nil {
			insertRequestRecord(ctx, tokenUsage{}, false, http.StatusInternalServerError, kit.Truncate(err.Error(), 2000))
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"error": map[string]string{"message": err.Error(), "type": "parse_error"},
			})
			return
		}
		if u, ok := out["usage"].(map[string]any); ok && len(u) > 0 {
			usageFn(u)
		}
		out = normalizeOpenAIResponse(out)

		var u tokenUsage
		if usage, ok := out["usage"].(map[string]any); ok {
			extractOpenAIUsage(usage, &u)
		}

		anthropicResp := openAIToAnthropic(out)
		if tc, ok := getNested(out, "choices", 0, "message", "tool_calls").([]any); ok && len(tc) > 0 {
			anthropicResp["stop_reason"] = "tool_use"
		}
		writeJSON(w, http.StatusOK, anthropicResp)
		insertRequestRecord(ctx, u, true, 200, "")
		return
	}

	var raw map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": map[string]string{"message": err.Error(), "type": "parse_error"},
		})
		return
	}
	if u, ok := raw["usage"].(map[string]any); ok && len(u) > 0 {
		usageFn(u)
	}
	out := raw
	if data, ok := raw["data"]; ok {
		if d, ok := data.(map[string]any); ok {
			out = d
		}
	}
	out = normalizeOpenAIResponse(out)
	anthropicResp := openAIToAnthropic(out)

	if tc, ok := getNested(out, "choices", 0, "message", "tool_calls").([]any); ok && len(tc) > 0 {
		anthropicResp["stop_reason"] = "tool_use"
	}

	var u2 tokenUsage
	if usage, ok := out["usage"].(map[string]any); ok {
		extractOpenAIUsage(usage, &u2)
	}
	writeJSON(w, http.StatusOK, anthropicResp)
	insertRequestRecord(ctx, u2, true, 200, "")
}

// handleZenAnthropic Anthropic Messages 请求路由到 zen 免费模型上游
func handleZenAnthropic(w http.ResponseWriter, r *http.Request, req anthropicReq, openAIReq map[string]any, toolSchemas map[string]map[string]bool) {
	cfg := getZenConfig()
	if !cfg.Enabled {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": map[string]string{"message": "zen upstream disabled in /admin/ settings", "type": "api_error"},
		})
		return
	}
	zm, ok := resolveZenFreeModel(req.Model)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]string{"message": fmt.Sprintf("model %q is not a free zen model", req.Model), "type": "invalid_request_error"},
		})
		return
	}
	isStream := req.Stream
	tracker := newZenStatsTracker(zenStatsRecord{
		TS:           time.Now().UnixMilli(),
		Upstream:     "zen",
		Model:        zm.ID,
		Stream:       isStream,
		PromptTokens: estimateJSON(openAIReq),
	})

	sid := requestSessionID(openAIReq, r.Header)
	out := maybeCompact(openAIReq, zm, sid)
	tracker.rec.Compacted = out.changed
	tracker.rec.CompactionTokens = out.compactTokens
	if out.changed {
		log.Printf("  anthropic zen: %s", out.note)
	}

	resp, rateLimited, err := callZenAPI(openAIReq, isStream)
	if err != nil {
		log.Printf("  anthropic zen api error: %v", err)
		tracker.rec.RateLimited = rateLimited
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": map[string]string{"message": err.Error(), "type": "api_error"},
		})
		tracker.finish(false, http.StatusBadGateway)
		return
	}
	tracker.rec.RateLimited = rateLimited
	defer resp.Body.Close()
	tracker.rec.Status = resp.StatusCode

	usageFn := func(u map[string]any) {
		if ct, ok := u["completion_tokens"].(float64); ok {
			tracker.rec.CompletionTokens = int(ct)
		}
	}

	if isStream {
		// zen 走 zen-stats.jsonl 统计，不写 SQLite request_log（两套统计不交叉）
		handleAnthropicStream(w, resp, nil, zm.ID, toolSchemas, usageFn)
		tracker.finish(true, resp.StatusCode)
		return
	}

	var raw map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": map[string]string{"message": err.Error(), "type": "parse_error"},
		})
		tracker.finish(false, http.StatusInternalServerError)
		return
	}
	if u, ok := raw["usage"].(map[string]any); ok && len(u) > 0 {
		usageFn(u)
	}
	chatOut := raw
	if data, ok := raw["data"]; ok {
		if d, ok := data.(map[string]any); ok {
			chatOut = d
		}
	}
	chatOut = normalizeOpenAIResponse(chatOut)
	anthropicResp := openAIToAnthropic(chatOut)
	if tc, ok := getNested(chatOut, "choices", 0, "message", "tool_calls").([]any); ok && len(tc) > 0 {
		anthropicResp["stop_reason"] = "tool_use"
	}
	writeJSON(w, http.StatusOK, anthropicResp)
	tracker.finish(true, resp.StatusCode)
}

func handleAnthropicStream(w http.ResponseWriter, upstream *http.Response, ctx *requestContext, modelName string, toolSchemas map[string]map[string]bool, onUsage func(map[string]any)) {
	log.Printf("  anthropic stream: starting real-time forward")
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(http.StatusOK)
	flusher, ok := w.(http.Flusher)
	if !ok {
		insertRequestRecord(ctx, tokenUsage{}, false, 0, "streaming not supported")
		return
	}

	var u tokenUsage
	var streamLog *os.File
	if sf, err := os.OpenFile(kit.ResolveDataPath("cline-proxy-stream.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644); err == nil {
		streamLog = sf
	}
	defer func() {
		if streamLog != nil {
			streamLog.Close()
		}
	}()

	emit := func(event string, data any) {
		d, _ := json.Marshal(data)
		line := fmt.Sprintf("event: %s\ndata: %s\n\n", event, string(d))
		w.Write([]byte(line))
		if streamLog != nil {
			streamLog.WriteString(line)
		}
		flusher.Flush()
	}

	msgID := "msg_" + fmt.Sprintf("%x", time.Now().UnixMilli())
	stopReason := "end_turn"
	emit("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":      msgID,
			"type":    "message",
			"role":    "assistant",
			"content": []any{},
			"model":   modelName,
			"usage": map[string]any{
				"input_tokens":  0,
				"output_tokens": 0,
			},
			"stop_reason": nil,
		},
	})

	textIndex := new(int)
	*textIndex = -1
	hasText := false
	pendingTools := map[int]*toolAccumulator{}
	emitIndex := 0
	nextIndex := func() int {
		i := emitIndex
		emitIndex++
		return i
	}

	emitToolBlock := func(acc *toolAccumulator) {
		acc.emitted = true
		if acc.name == "" {
			log.Printf("  tool_use missing name, skipping (id=%s)", acc.id)
			return
		}
		idx := nextIndex()
		id := acc.id
		if id == "" {
			id = fmt.Sprintf("toolu_%x_%d", time.Now().UnixMilli(), idx)
			log.Printf("  tool_use missing id, generated %s", id)
		}
		argsObj, err := parseToolArgs(acc.args)
		if err != nil {
			log.Printf("  tool args parse failed for %s: %v (raw: %s)", acc.name, err, kit.Truncate(acc.args, 300))
			argsObj = map[string]any{}
		}
		if inputMap, ok := argsObj.(map[string]any); ok {
			argsObj = filterToolInput(acc.name, inputMap, toolSchemas)
		}
		parsed, _ := json.Marshal(argsObj)
		log.Printf("  tool_use emit: name=%s id=%s input=%s", acc.name, id, string(parsed))
		emit("content_block_start", map[string]any{
			"type":  "content_block_start",
			"index": idx,
			"content_block": map[string]any{
				"type":  "tool_use",
				"id":    id,
				"name":  acc.name,
				"input": map[string]any{},
			},
		})
		emit("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": idx,
			"delta": map[string]any{
				"type":         "input_json_delta",
				"partial_json": string(parsed),
			},
		})
		emit("content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": idx,
		})
	}

	processSSELine := func(line string) {
		line = strings.TrimRight(line, "\r\n")
		if !strings.HasPrefix(line, "data:") {
			return
		}
		payload := strings.TrimSpace(line[5:])
		if payload == "" || payload == "[DONE]" {
			return
		}

		var obj map[string]any
		if err := json.Unmarshal([]byte(payload), &obj); err != nil {
			return
		}
		if onUsage != nil {
			if u, ok := obj["usage"].(map[string]any); ok && len(u) > 0 {
				onUsage(u)
			}
		}
		if data, ok := obj["data"]; ok {
			if d, ok := data.(map[string]any); ok {
				obj = d
			}
		}

		if errPayload, ok := obj["error"]; ok {
			errBody, _ := json.Marshal(errPayload)
			log.Printf("  upstream SSE error: %s", string(errBody))
			emit("error", map[string]any{"type": "error", "error": errPayload})
			return
		}

		// 旁路提取上游 OpenAI 格式 usage（必须在 choices 空检查之前，
		// 因为 usage-only chunk 的 choices 常为空数组会被跳过）。
		if usage, ok := obj["usage"].(map[string]any); ok {
			extractOpenAIUsage(usage, &u)
		}

		choices, _ := getNested(obj, "choices").([]any)
		if len(choices) == 0 {
			return
		}
		choice, _ := choices[0].(map[string]any)
		if choice == nil {
			return
		}

		delta, ok := choice["delta"].(map[string]any)
		if !ok {
			delta = choice
		}

		if c, ok := delta["content"].(string); ok && c != "" {
			if !hasText {
				hasText = true
				*textIndex = nextIndex()
				emit("content_block_start", map[string]any{
					"type":  "content_block_start",
					"index": *textIndex,
					"content_block": map[string]any{
						"type": "text",
						"text": "",
					},
				})
			}
			emit("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": *textIndex,
				"delta": map[string]any{
					"type": "text_delta",
					"text": sanitizeContent(c),
				},
			})
		}

		if tcRaw, ok := delta["tool_calls"].([]any); ok {
			for _, tc := range tcRaw {
				tcMap, _ := tc.(map[string]any)
				if tcMap == nil {
					continue
				}
				idx := 0
				if i, ok := tcMap["index"].(float64); ok {
					idx = int(i)
				}
				acc, exists := pendingTools[idx]
				if !exists {
					acc = &toolAccumulator{index: idx}
					pendingTools[idx] = acc
				}
				if id, ok := tcMap["id"].(string); ok && id != "" {
					acc.id = id
				}
				if fn, ok := tcMap["function"].(map[string]any); ok {
					if name, ok := fn["name"].(string); ok && name != "" {
						acc.name = name
					}
					if args, ok := fn["arguments"].(string); ok && args != "" {
						acc.args += args
					} else if argsRaw, ok := fn["arguments"]; ok && argsRaw != nil {
						if bts, err := json.Marshal(argsRaw); err == nil {
							acc.args = string(bts)
						}
					}
				}
			}
		}

		if fr, ok := choice["finish_reason"].(string); ok && fr != "" {
			switch fr {
			case "length":
				stopReason = "max_tokens"
			case "tool_calls":
				stopReason = "tool_use"
			}
		}
	}

	reader := bufio.NewReader(upstream.Body)

	for {
		line, err := reader.ReadString('\n')
		if line != "" {
			processSSELine(line)
		}
		if err != nil {
			break
		}
	}

	// Stop text block if active
	if hasText {
		emit("content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": *textIndex,
		})
	}

	// Emit any remaining un-emitted tool blocks
	for _, acc := range pendingTools {
		if !acc.emitted {
			emitToolBlock(acc)
		}
	}

	emit("message_delta", map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason":   stopReason,
			"stop_sequence": nil,
		},
		"usage": map[string]any{
			"input_tokens":  u.promptTokens,
			"output_tokens": u.completionTokens,
		},
	})

	emit("message_stop", map[string]any{"type": "message_stop"})
	log.Printf("  anthropic stream done: hasText=%v tools=%d reason=%s", hasText, len(pendingTools), stopReason)
	insertRequestRecord(ctx, u, true, 200, "")
}

func normalizeOpenAIResponse(obj map[string]any) map[string]any {
	out := make(map[string]any)
	for k, v := range obj {
		if k == "provider_metadata" || k == "proxy_metadata" {
			continue
		}
		out[k] = v
	}

	if choices, ok := out["choices"].([]any); ok {
		normalized := make([]any, 0, len(choices))
		for _, ch := range choices {
			if c, ok := ch.(map[string]any); ok {
				nc := make(map[string]any)
				for k, v := range c {
					if k == "provider_metadata" || k == "proxy_metadata" {
						continue
					}
					nc[k] = v
				}
				if msg, ok := nc["message"].(map[string]any); ok {
					nc["message"] = normalizeMessage(msg)
				}
				if delta, ok := nc["delta"].(map[string]any); ok {
					nd := make(map[string]any)
					for k, v := range delta {
						if k == "provider_metadata" || k == "proxy_metadata" {
							continue
						}
						nd[k] = v
					}
					if tc, ok := nd["tool_calls"].([]any); ok && len(tc) > 0 {
						if nd["content"] == nil {
							nd["content"] = ""
						}
					}
					nc["delta"] = nd
				}
				normalized = append(normalized, nc)
			} else {
				normalized = append(normalized, ch)
			}
		}
		out["choices"] = normalized
	}

	return out
}

func sanitizeContent(s string) string {
	return s
}

func normalizeMessage(msg map[string]any) map[string]any {
	out := make(map[string]any)
	for k, v := range msg {
		if k == "provider_metadata" || k == "proxy_metadata" {
			continue
		}
		out[k] = v
	}
	if tc, ok := out["tool_calls"].([]any); ok && len(tc) > 0 {
		if out["content"] == nil {
			out["content"] = ""
		}
	}
	if c, ok := out["content"].(string); ok {
		out["content"] = sanitizeContent(c)
	}
	return out
}

func getNested(obj map[string]any, keys ...any) any {
	current := any(obj)
	for _, key := range keys {
		switch k := key.(type) {
		case string:
			if m, ok := current.(map[string]any); ok {
				current = m[k]
			} else {
				return nil
			}
		case int:
			if arr, ok := current.([]any); ok && k < len(arr) {
				current = arr[k]
			} else {
				return nil
			}
		default:
			return nil
		}
	}
	return current
}

func freePort(port int) {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return // port is free
	}
	conn.Close()

	// Try to kill the process using the port
	cmd := kit.ExecCommand("powershell", "-Command",
		fmt.Sprintf(`$p=Get-NetTCPConnection -LocalPort %d -ErrorAction SilentlyContinue; if($p){$p.OwningProcess | Sort-Object -Unique | ForEach-Object {Stop-Process -Id $_ -Force -ErrorAction SilentlyContinue}}`, port))
	_ = cmd.Run()
	// 杀进程后确认端口确实释放，避免旧进程尚未退出时立刻竞争监听。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err != nil {
			return
		}
		conn.Close()
		time.Sleep(100 * time.Millisecond)
	}
}

// parseInferenceCapDuration 从 Cline 429 错误体中解析 "Try again in 17h 59m" 形式的等待时长。
// 支持 "17h 59m"、"17h"、"59m"、"30s"、"1d 2h 30m" 等组合。
func parseInferenceCapDuration(body string) time.Duration {
	// 在错误体中查找 "Try again in ..." 子串
	idx := strings.Index(body, "Try again in")
	if idx < 0 {
		return 0
	}
	rest := body[idx+len("Try again in"):]
	// 截取到下一个引号或换行
	end := len(rest)
	if i := strings.IndexAny(rest, "\"\n\r}"); i >= 0 {
		end = i
	}
	segment := strings.TrimSpace(rest[:end])
	return parseHumanDuration(segment)
}

// parseHumanDuration 解析 "17h 59m" / "2h" / "59m" / "30s" / "1d 2h" 之类的时长。
func parseHumanDuration(s string) time.Duration {
	if s == "" {
		return 0
	}
	var total time.Duration
	num := 0
	valid := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			num = num*10 + int(c-'0')
			valid = true
		case c == 'd':
			total += time.Duration(num) * 24 * time.Hour
			num, valid = 0, false
		case c == 'h':
			total += time.Duration(num) * time.Hour
			num, valid = 0, false
		case c == 'm' && i+1 < len(s) && s[i+1] == 's':
			total += time.Duration(num) * time.Millisecond
			num, valid = 0, false
			i++
		case c == 'm':
			total += time.Duration(num) * time.Minute
			num, valid = 0, false
		case c == 's':
			total += time.Duration(num) * time.Second
			num, valid = 0, false
		case c == ' ':
			// 分隔符
		default:
			// 未知字符，重置
			num, valid = 0, false
		}
	}
	if total <= 0 {
		return 0
	}
	_ = valid
	return total
}

// parseRetryAfter 解析 HTTP Retry-After 头（秒数或 HTTP 日期）。
func parseRetryAfter(header string) time.Duration {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0
	}
	// 尝试秒数
	if secs, err := parseIntSafe(header); err == nil {
		return time.Duration(secs) * time.Second
	}
	// 尝试 HTTP 日期
	if t, err := http.ParseTime(header); err == nil {
		d := time.Until(t)
		if d > 0 {
			return d
		}
	}
	return 0
}

func parseIntSafe(s string) (int, error) {
	var n int
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, fmt.Errorf("not a number")
		}
		n = n*10 + int(s[i]-'0')
	}
	return n, nil
}
