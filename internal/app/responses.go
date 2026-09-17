package app

import (
	"bufio"
	"cline-go-proxy/internal/kit"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// ============================================================================
// OpenAI Responses API (/v1/responses) -> chat/completions 转换
// 支持 Cursor 等客户端直连反代,无需 opencode CLI
// ============================================================================

// responsesToChat 将 Responses 请求体转换为 chat.completions 请求体
func responsesToChat(body map[string]any) map[string]any {
	out := map[string]any{}
	if m, ok := body["model"].(string); ok {
		out["model"] = m
	}
	if s, ok := body["stream"].(bool); ok {
		out["stream"] = s
	}
	if mt, ok := body["max_output_tokens"].(float64); ok {
		out["max_tokens"] = int(mt)
	}
	for _, k := range []string{"temperature", "top_p", "stop", "seed", "user", "metadata", "logit_bias"} {
		if v, ok := body[k]; ok {
			out[k] = v
		}
	}
	if instr, ok := body["instructions"].(string); ok && instr != "" {
		out["messages"] = append([]any{map[string]any{"role": "system", "content": instr}}, responsesInputToMessages(body["input"])...)
	} else {
		out["messages"] = responsesInputToMessages(body["input"])
	}
	if tools, ok := body["tools"].([]any); ok {
		out["tools"] = responsesToolsToChat(tools)
	}
	if tc, ok := body["tool_choice"]; ok {
		out["tool_choice"] = tc
	}
	return out
}

func responsesInputToMessages(input any) []any {
	var msgs []any
	switch v := input.(type) {
	case string:
		msgs = append(msgs, map[string]any{"role": "user", "content": v})
	case []any:
		for _, item := range v {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			switch m["type"] {
			case "message":
				role, _ := m["role"].(string)
				if role == "" {
					role = "user"
				}
				msgs = append(msgs, map[string]any{"role": role, "content": stringifyResponsesContent(m["content"])})
			case "function_call":
				callID, _ := m["call_id"].(string)
				if callID == "" {
					callID, _ = m["id"].(string)
				}
				name, _ := m["name"].(string)
				args := ""
				switch a := m["arguments"].(type) {
				case string:
					args = a
				case map[string]any:
					if b, err := json.Marshal(a); err == nil {
						args = string(b)
					}
				}
				msgs = append(msgs, map[string]any{
					"role":       "assistant",
					"content":    "",
					"tool_calls": []any{map[string]any{"id": callID, "type": "function", "function": map[string]any{"name": name, "arguments": args}}},
				})
			case "function_call_output":
				callID, _ := m["call_id"].(string)
				if callID == "" {
					callID, _ = m["id"].(string)
				}
				output := ""
				switch o := m["output"].(type) {
				case string:
					output = o
				case map[string]any:
					if b, err := json.Marshal(o); err == nil {
						output = string(b)
					}
				}
				msgs = append(msgs, map[string]any{"role": "tool", "content": output, "tool_call_id": callID})
			case "reasoning":
				// 忽略 Reasoning 输入项(无法映射到 chat 输入)
			}
		}
	}
	return msgs
}

func stringifyResponsesContent(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		parts := []string{}
		for _, block := range v {
			if b, ok := block.(map[string]any); ok {
				if t, ok := b["text"].(string); ok {
					parts = append(parts, t)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func responsesToolsToChat(tools []any) []any {
	out := make([]any, 0, len(tools))
	for _, t := range tools {
		tm, ok := t.(map[string]any)
		if !ok {
			continue
		}
		if tm["type"] == "function" {
			fn := map[string]any{}
			if n, ok := tm["name"].(string); ok {
				fn["name"] = n
			}
			if d, ok := tm["description"].(string); ok {
				fn["description"] = d
			}
			if p, ok := tm["parameters"].(map[string]any); ok {
				fn["parameters"] = p
			}
			out = append(out, map[string]any{"type": "function", "function": fn})
		}
	}
	return out
}

// ============ 非流式响应转换 ============

// chatToResponses chat.completions 响应 -> Responses 响应
func chatToResponses(chat map[string]any) map[string]any {
	resp := map[string]any{
		"id":         "resp_" + fmt.Sprintf("%x", time.Now().UnixMilli()),
		"object":     "response",
		"created_at": time.Now().Unix(),
		"status":     "completed",
		"model":      chat["model"],
		"output":     []any{},
		"output_text": "",
	}
	choices, _ := chat["choices"].([]any)
	outputs := []any{}
	var outputText strings.Builder
	if len(choices) > 0 {
		if ch, ok := choices[0].(map[string]any); ok {
			msg, _ := ch["message"].(map[string]any)
			if msg == nil {
				msg, _ = ch["delta"].(map[string]any)
			}
			content := []any{}
			if c, ok := msg["content"].(string); ok && c != "" {
				outputText.WriteString(c)
				content = append(content, map[string]any{"type": "output_text", "text": c, "annotations": []any{}})
			}
			msgOut := map[string]any{
				"type":      "message",
				"id":        "msg_" + fmt.Sprintf("%x", time.Now().UnixMilli()),
				"status":    "completed",
				"role":      "assistant",
				"content":   content,
				"output_text": outputText.String(),
			}
			outputs = append(outputs, msgOut)

			if tc, ok := msg["tool_calls"].([]any); ok {
				for _, c := range tc {
					if cm, ok := c.(map[string]any); ok {
						fn, _ := cm["function"].(map[string]any)
						callID, _ := cm["id"].(string)
						if callID == "" {
							callID = fmt.Sprintf("fc_%x", time.Now().UnixNano())
						}
						name := ""
						args := ""
						if fn != nil {
							name, _ = fn["name"].(string)
							if a, ok := fn["arguments"].(string); ok {
								args = a
							}
						}
						outputs = append(outputs, map[string]any{
							"type":      "function_call",
							"id":        "fc_" + fmt.Sprintf("%x", time.Now().UnixMilli()),
							"call_id":   callID,
							"name":      name,
							"arguments": args,
							"status":    "completed",
						})
					}
				}
			}
		}
	}
	resp["output"] = outputs
	resp["output_text"] = outputText.String()
	if u, ok := chat["usage"].(map[string]any); ok {
		resp["usage"] = openAIUsageToResponses(u)
	}
	return resp
}

// ============ 流式响应转换 (Responses SSE) ============

type responsesSSEWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
	msgID   string
	respID  string
}

func newResponsesSSE(w http.ResponseWriter) *responsesSSEWriter {
	f, _ := w.(http.Flusher)
	return &responsesSSEWriter{w: w, flusher: f, msgID: "msg_" + fmt.Sprintf("%x", time.Now().UnixMilli()), respID: "resp_" + fmt.Sprintf("%x", time.Now().UnixMilli())}
}

func (s *responsesSSEWriter) event(event string, data any) {
	b, _ := json.Marshal(data)
	fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", event, string(b))
	if s.flusher != nil {
		s.flusher.Flush()
	}
}

// respToolCall 是一条流式 tool_call 的累积状态。
// 之前用全局 curCallName/curArgs：多个工具调用会互相顶掉（名字取最后一个、
// 参数拼在一起），item id 也只用名字拼（同名调用撞 id）。
type respToolCall struct {
	itemID   string
	callID   string
	name     string
	args     strings.Builder
	outIndex int
	added    bool
}

// usageInt 取 usage 字段，缺失时给 0（避免 JSON 里出现 null）。
func usageInt(u map[string]any, key string) any {
	if v, ok := u[key]; ok {
		return v
	}
	return 0
}

// openAIUsageToResponses 把 chat.completions 的 usage 转成 Responses 的 usage 形状。
// 非流式与流式的 response.completed 共用，避免两边口径不一致（流式那边以前恒为 0）。
func openAIUsageToResponses(u map[string]any) map[string]any {
	details := map[string]any{}
	if pd, ok := u["prompt_tokens_details"].(map[string]any); ok {
		if cached, ok := pd["cached_tokens"]; ok {
			details["cached_tokens"] = cached
		}
	}
	od := map[string]any{}
	if rd, ok := u["reasoning_tokens"]; ok {
		od["reasoning_tokens"] = rd
	}
	if cd, ok := u["completion_tokens_details"].(map[string]any); ok {
		if rd, ok := cd["reasoning_tokens"]; ok {
			od["reasoning_tokens"] = rd
		}
	}
	return map[string]any{
		"input_tokens":          usageInt(u, "prompt_tokens"),
		"input_tokens_details":  details,
		"output_tokens":         usageInt(u, "completion_tokens"),
		"output_tokens_details": od,
		"total_tokens":          usageInt(u, "total_tokens"),
	}
}

// callItemID 生成 function_call 的 item id：优先用上游 call id（唯一），
// 缺失时退回累积下标，避免同名工具调用撞 id。
func callItemID(call *respToolCall, callIndex int) string {
	if call.callID != "" {
		return "fc_" + call.callID
	}
	return fmt.Sprintf("fc_%d", callIndex)
}

// chatStreamToResponses 将上游 chat.completions SSE 流转换为 Responses SSE 流。
// 返回 nil 表示上游流正常结束；返回非 nil 表示上游中途断流（已发 response.failed），
// 调用方据此把这次请求记为失败而不是成功。
func chatStreamToResponses(w http.ResponseWriter, upstream *http.Response, onUsage func(map[string]any)) error {
	model := ""
	// 开场
	s := newResponsesSSE(w)
	s.event("response.created", map[string]any{
		"type": "response.created",
		"response": map[string]any{
			"id":         s.respID,
			"object":     "response",
			"created_at": time.Now().Unix(),
			"status":     "in_progress",
			"model":      "",
			"output":     []any{},
		},
	})
	s.event("response.in_progress", map[string]any{"type": "response.in_progress", "response": map[string]any{"id": s.respID}})

	// output_index 按「项出现的先后」递增分配（协议要求单调）：文本项与每个
	// function_call 各占一个，不再把 function_call 写死成 1。
	var (
		nextIndex int
		textIndex = -1
		outText   strings.Builder
		lastUsage map[string]any
		calls     = map[int]*respToolCall{}
		callOrder []int
	)

	textOutputIndex := func() int {
		if textIndex >= 0 {
			return textIndex
		}
		return 0
	}

	reader := bufio.NewReader(upstream.Body)
	var streamErr error
	for {
		line, err := reader.ReadString('\n')
		if line != "" {
			line = strings.TrimRight(line, "\r\n")
			if strings.HasPrefix(line, "data:") {
				payload := strings.TrimSpace(line[5:])
				if payload == "" || payload == "[DONE]" {
					if err != nil {
						break
					}
					continue
				}
				var obj map[string]any
				if json.Unmarshal([]byte(payload), &obj) != nil {
					if err != nil {
						break
					}
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
					if onUsage != nil {
						onUsage(u)
					}
					lastUsage = u
				}
				choices, _ := obj["choices"].([]any)
				if len(choices) == 0 {
					continue
				}
				ch, _ := choices[0].(map[string]any)
				if ch == nil {
					continue
				}
				delta, _ := ch["delta"].(map[string]any)
				if delta == nil {
					delta = ch
				}
				// 文本
				if c, ok := delta["content"].(string); ok && c != "" {
					if textIndex < 0 {
						textIndex = nextIndex
						nextIndex++
						s.event("response.output_item.added", map[string]any{
							"type":         "response.output_item.added",
							"output_index": textIndex,
							"item":         map[string]any{"id": s.msgID, "type": "message", "role": "assistant", "status": "in_progress", "content": []any{}},
						})
						s.event("response.content_part.added", map[string]any{
							"type":          "response.content_part.added",
							"item_id":       s.msgID,
							"output_index":  textIndex,
							"content_index": 0,
							"part":          map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
						})
					}
					outText.WriteString(c)
					s.event("response.output_text.delta", map[string]any{
						"type":          "response.output_text.delta",
						"item_id":       s.msgID,
						"output_index":  textIndex,
						"content_index": 0,
						"delta":         c,
					})
				}
				// 推理
				if r, ok := delta["reasoning_content"].(string); ok && r != "" {
					s.event("response.reasoning_summary_text.delta", map[string]any{
						"type":          "response.reasoning_summary_text.delta",
						"item_id":       s.msgID,
						"output_index":  textOutputIndex(),
						"content_index": 0,
						"delta":         r,
					})
				}
				// 工具调用：按上游给的 index 分别累积，互不干扰
				if tc, ok := delta["tool_calls"].([]any); ok {
					for position, c := range tc {
						cm, ok := c.(map[string]any)
						if !ok {
							continue
						}
						callIndex := position
						if f, ok := cm["index"].(float64); ok {
							callIndex = int(f)
						}
						call := calls[callIndex]
						if call == nil {
							call = &respToolCall{outIndex: nextIndex}
							nextIndex++
							calls[callIndex] = call
							callOrder = append(callOrder, callIndex)
						}
						if id, ok := cm["id"].(string); ok && id != "" {
							call.callID = id
						}
						argChunk := ""
						if fn, ok := cm["function"].(map[string]any); ok {
							if n, ok := fn["name"].(string); ok && n != "" {
								call.name = n
							}
							if a, ok := fn["arguments"].(string); ok && a != "" {
								argChunk = a
								call.args.WriteString(a)
							}
						}
						if !call.added {
							call.added = true
							call.itemID = callItemID(call, callIndex)
							s.event("response.output_item.added", map[string]any{
								"type":         "response.output_item.added",
								"output_index": call.outIndex,
								"item": map[string]any{
									"type":      "function_call",
									"id":        call.itemID,
									"call_id":   call.callID,
									"name":      call.name,
									"arguments": "",
									"status":    "in_progress",
								},
							})
						}
						if argChunk != "" {
							s.event("response.function_call_arguments.delta", map[string]any{
								"type":         "response.function_call_arguments.delta",
								"item_id":      call.itemID,
								"output_index": call.outIndex,
								"delta":        argChunk,
							})
						}
					}
				}
			}
		}
		if err != nil {
			if err != io.EOF {
				streamErr = err
			}
			break
		}
	}

	if streamErr != nil {
		// 上游中途断流：头已发出，只能发 response.failed，不能发 response.completed
		// （否则客户端会把被截断的结果当成正常完成）。调用方据返回值记失败。
		log.Printf("  responses upstream stream aborted: %v", streamErr)
		s.event("response.failed", map[string]any{
			"type": "response.failed",
			"response": map[string]any{
				"id":         s.respID,
				"object":     "response",
				"created_at": time.Now().Unix(),
				"status":     "failed",
				"model":      model,
				"error": map[string]any{
					"code":    "upstream_stream_interrupted",
					"message": "upstream stream aborted: " + streamErr.Error(),
				},
			},
		})
		return streamErr
	}

	// 收尾：文本项
	output := []any{}
	if textIndex >= 0 {
		messageItem := map[string]any{
			"id": s.msgID, "type": "message", "role": "assistant", "status": "completed",
			"content": []any{map[string]any{"type": "output_text", "text": outText.String(), "annotations": []any{}}},
		}
		output = append(output, messageItem)
		s.event("response.output_text.done", map[string]any{"type": "response.output_text.done", "item_id": s.msgID, "output_index": textIndex, "content_index": 0, "text": outText.String()})
		s.event("response.content_part.done", map[string]any{"type": "response.content_part.done", "item_id": s.msgID, "output_index": textIndex, "content_index": 0, "part": map[string]any{"type": "output_text", "text": outText.String(), "annotations": []any{}}})
		s.event("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": textIndex, "item": messageItem})
	}
	// 收尾：每个工具调用各一份 done（以前只有一个，且参数是所有调用拼在一起的）
	for _, callIndex := range callOrder {
		call := calls[callIndex]
		args := call.args.String()
		if !call.added {
			// 上游连名字都没给就结束：补一个 added，别让客户端只收到 done
			call.itemID = callItemID(call, callIndex)
			call.added = true
			s.event("response.output_item.added", map[string]any{
				"type":         "response.output_item.added",
				"output_index": call.outIndex,
				"item": map[string]any{
					"type": "function_call", "id": call.itemID, "call_id": call.callID,
					"name": call.name, "arguments": "", "status": "in_progress",
				},
			})
		}
		callItem := map[string]any{
			"type": "function_call", "id": call.itemID, "call_id": call.callID,
			"name": call.name, "arguments": args, "status": "completed",
		}
		output = append(output, callItem)
		s.event("response.function_call_arguments.done", map[string]any{"type": "response.function_call_arguments.done", "item_id": call.itemID, "output_index": call.outIndex, "arguments": args})
		s.event("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": call.outIndex, "item": callItem})
	}

	s.event("response.completed", map[string]any{
		"type": "response.completed",
		"response": map[string]any{
			"id":          s.respID,
			"object":      "response",
			"created_at":  time.Now().Unix(),
			"status":      "completed",
			"model":       model,
			"output":      output,
			"output_text": outText.String(),
			"usage":       openAIUsageToResponses(lastUsage),
		},
	})
	return nil
}

// ============ /v1/responses 入口 ============

func handleResponses(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	var params map[string]any
	if err := json.Unmarshal(body, &params); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	model, _ := params["model"].(string)
	isStream, _ := params["stream"].(bool)
	log.Printf("  responses: model=%s stream=%v", model, isStream)

	chat := responsesToChat(params)
	chatModel, _ := chat["model"].(string)
	route := routeModel(chatModel)
	if route == "reject" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]string{"message": fmt.Sprintf("model %q is a paid zen model; only free zen models are proxied", chatModel), "type": "invalid_request_error"},
		})
		return
	}
	if route == "zen" {
		zm, ok := resolveZenFreeModel(chatModel)
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": map[string]string{"message": fmt.Sprintf("model %q is not a free zen model", chatModel), "type": "invalid_request_error"},
			})
			return
		}
		sid := requestSessionID(chat, r.Header)
		out := maybeCompact(chat, zm, sid)
		if out.changed {
			log.Printf("  responses zen: %s", out.note)
		}
		resp, _, err := callZenAPI(chat, isStream)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{
				"error": map[string]string{"message": err.Error(), "type": "api_error"},
			})
			return
		}
		defer resp.Body.Close()
		if isStream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.WriteHeader(http.StatusOK)
			// zen 走 zen-stats.jsonl 统计，这里不写 SQLite request_log；
			// 断流时函数内部已发 response.failed，只补日志。
			if err := chatStreamToResponses(w, resp, nil); err != nil {
				log.Printf("  responses zen: upstream stream aborted: %v", err)
			}
			return
		}
		var raw map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, chatToResponses(raw))
		return
	}

	// cline 上游
	stream := isStream
	if !isStream && modelNeedsStream(normalizeRequestModel(chatModel)) {
		stream = true
	}
	up, acc, ctx, err := callClineAPI(chat, stream, nil)
	ctx.apiFormat = "openai"
	if err != nil {
		writeUpstreamError(w, ctx, err)
		return
	}
	defer up.Body.Close()

	usageFn := accountUsageFn(acc, chat)
	if isStream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.WriteHeader(http.StatusOK)
		if err := chatStreamToResponses(w, up, usageFn); err != nil {
			insertRequestRecord(ctx, tokenUsage{}, false, http.StatusBadGateway, "upstream stream aborted: "+err.Error())
			return
		}
		insertRequestRecord(ctx, tokenUsage{}, true, 200, "")
		return
	}
	if stream {
		out, _, err := collectStreamResponse(up)
		if err != nil {
			insertRequestRecord(ctx, tokenUsage{}, false, http.StatusInternalServerError, kit.Truncate(err.Error(), 2000))
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		var u tokenUsage
		if usage, ok := out["usage"].(map[string]any); ok {
			extractOpenAIUsage(usage, &u)
			if len(usage) > 0 {
				usageFn(usage)
			}
		}
		writeJSON(w, http.StatusOK, chatToResponses(out))
		insertRequestRecord(ctx, u, true, 200, "")
		return
	}
	var raw map[string]any
	if err := json.NewDecoder(up.Body).Decode(&raw); err != nil {
		insertRequestRecord(ctx, tokenUsage{}, false, http.StatusInternalServerError, kit.Truncate(err.Error(), 2000))
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	var u tokenUsage
	if usage, ok := raw["usage"].(map[string]any); ok {
		extractOpenAIUsage(usage, &u)
		if len(usage) > 0 {
			usageFn(usage)
		}
	}
	out := raw
	if data, ok := raw["data"]; ok {
		if d, ok := data.(map[string]any); ok {
			out = d
		}
	}
	writeJSON(w, http.StatusOK, chatToResponses(out))
	insertRequestRecord(ctx, u, true, 200, "")
}
