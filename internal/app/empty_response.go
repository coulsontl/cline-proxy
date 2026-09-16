package app

import (
	"bytes"
	"errors"
	"net/http"
	"strings"
)

// 空回检测：借鉴 axonhub 的 llm/pipeline/empty_response.go。
// 上游「HTTP 200 但内容为空」对 agent 客户端是致命的静默空答——只有
// finish_reason/[DONE] 的空流无法与「模型真的没话说」区分，工具调用回合会卡死。
// 这里只放与上游无关的纯判定函数，供流式 / 非流式 / 聚合三条出口复用。

// errUpstreamEmptyResponse 标记「上游成功响应但内容为空」这一类失败，
// 供重试层用 errors.Is 分类（区别于普通 5xx 与网络错误）。
var errUpstreamEmptyResponse = errors.New("empty response content")

// isEmptyUpstreamError 判定上游把空回直接当错误返回的形态：
// Cline 通道对长输入曾返回 HTTP 500 + {"error":"empty response content","success":false}。
func isEmptyUpstreamError(status int, body []byte) bool {
	return status == http.StatusInternalServerError &&
		bytes.Contains(body, []byte("empty response content"))
}

// isDonePayload 判定 SSE payload 是否为终止标记（非 JSON，需在解析前判断）。
func isDonePayload(payload string) bool {
	return strings.TrimSpace(payload) == "[DONE]"
}

// hasMessageContent 判定 message/delta 对象是否含「有意义内容」。
// 许可清单与 axonhub 的 hasMessageContent 对齐：role-only、空字符串、空数组都不算。
// 注意 reasoning / reasoning_details 算内容——真实流先出 reasoning 再出 content，
// 让提交点落在第一个 reasoning delta 上，因此缓冲不会增加首字延迟。
func hasMessageContent(msg map[string]any) bool {
	if msg == nil {
		return false
	}
	if s, ok := msg["content"].(string); ok && s != "" {
		return true
	}
	// 多模态 content 数组形态（与 axonhub 的 MultipleContent 一致）
	if parts, ok := msg["content"].([]any); ok && len(parts) > 0 {
		return true
	}
	if s, ok := msg["reasoning"].(string); ok && s != "" {
		return true
	}
	if s, ok := msg["reasoning_content"].(string); ok && s != "" {
		return true
	}
	if s, ok := msg["refusal"].(string); ok && s != "" {
		return true
	}
	if tc, ok := msg["tool_calls"].([]any); ok && len(tc) > 0 {
		return true
	}
	// reasoning_details[*].text 非空也算：本通道转发保留该字段，
	// 若只认 reasoning 字符串，只有 reasoning_details 的上游会被误判为空回。
	if details, ok := msg["reasoning_details"].([]any); ok {
		for _, raw := range details {
			if d, ok := raw.(map[string]any); ok {
				if s, ok := d["text"].(string); ok && s != "" {
					return true
				}
			}
		}
	}
	return false
}

// choiceMessage 从 choice 中取内容载体：优先 delta（流式），
// 其次是 message（非流式/聚合），都没有才退化到 choice 本身。
func choiceMessage(choice map[string]any) map[string]any {
	if delta, ok := choice["delta"].(map[string]any); ok {
		return delta
	}
	if msg, ok := choice["message"].(map[string]any); ok {
		return msg
	}
	return choice
}

// hasChunkContent 判定一个已解析的流式 chunk 是否携带内容。
func hasChunkContent(obj map[string]any) bool {
	return anyChoice(obj, func(choice map[string]any) bool {
		return hasMessageContent(choiceMessage(choice))
	})
}

// hasResponseContent 判定非流式响应体或流式聚合结果是否携带内容。
func hasResponseContent(out map[string]any) bool {
	return anyChoice(out, func(choice map[string]any) bool {
		return hasMessageContent(choiceMessage(choice))
	})
}

func anyChoice(obj map[string]any, pred func(map[string]any) bool) bool {
	if obj == nil {
		return false
	}
	// 兼容 {data:{...}} 包装的上游响应
	if data, ok := obj["data"].(map[string]any); ok {
		obj = data
	}
	choices, _ := obj["choices"].([]any)
	for _, raw := range choices {
		if choice, ok := raw.(map[string]any); ok && pred(choice) {
			return true
		}
	}
	return false
}

// isTerminalChunk 判定 chunk 是否携带终止信号（finish_reason 非空）。
func isTerminalChunk(obj map[string]any) bool {
	return anyChoice(obj, func(choice map[string]any) bool {
		fr, ok := choice["finish_reason"].(string)
		return ok && fr != ""
	})
}
