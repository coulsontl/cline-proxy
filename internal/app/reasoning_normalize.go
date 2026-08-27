package app

import "strings"

// 借鉴自 opencode2api convert.go：DeepSeek/Kimi/MiMo 等兼容端点要求
// assistant 工具调用回合携带 thinking/reasoning 历史，否则拒绝请求。
// 以下函数对已解码的请求体 map[string]any 做就地规范化，纯标准库自包含。

const (
	toolReasoningPlaceholder              = "tool call"
	anthropicRedactedThinkingPlaceholder  = "[redacted thinking]"
)

var reasoningVendorHints = [...]string{"moonshot", "kimi", "deepseek", "mimo", "xiaomimimo"}

// normalizeToolReasoningHistory 按协议规范化工具调用回合的 thinking 历史。
// 返回是否改动。仅当模型/上游属于 reasoning 厂商或请求显式启用 reasoning 时触发。
func normalizeToolReasoningHistory(protocol Protocol, model, upstreamURL string, input map[string]any) bool {
	if !shouldNormalizeToolReasoningHistory(model, upstreamURL, input) {
		return false
	}
	switch protocol {
	case ProtocolChat:
		return normalizeChatToolReasoningHistory(input)
	case ProtocolAnthropic:
		return normalizeAnthropicToolThinkingHistory(input)
	default:
		return false
	}
}

// shouldNormalizeToolReasoningHistory 判定是否需要规范化：
// 模型名或上游 URL 含 reasoning 厂商 hint，或请求体显式启用 reasoning。
func shouldNormalizeToolReasoningHistory(model, upstreamURL string, input map[string]any) bool {
	return isReasoningVendorIdentifier(model) || isReasoningVendorIdentifier(upstreamURL) || requestEnablesReasoning(input)
}

func isReasoningVendorIdentifier(value string) bool {
	value = strings.ToLower(value)
	for _, hint := range reasoningVendorHints {
		if strings.Contains(value, hint) {
			return true
		}
	}
	return false
}

// requestEnablesReasoning 检查请求体是否显式启用 reasoning/thinking。
// 支持 reasoning_effort/reasoning/thinking/effort 四种键，string/bool/map 三种形态。
func requestEnablesReasoning(input map[string]any) bool {
	for _, key := range []string{"reasoning_effort", "reasoning", "thinking", "effort"} {
		value, exists := input[key]
		if !exists || value == nil {
			continue
		}
		switch typed := value.(type) {
		case string:
			mode := strings.ToLower(strings.TrimSpace(typed))
			if mode != "" && mode != "none" && mode != "disabled" {
				return true
			}
		case bool:
			if typed {
				return true
			}
		case map[string]any:
			mode := strings.ToLower(strings.TrimSpace(firstString(stringAt(typed, "type"), stringAt(typed, "effort"))))
			if mode == "none" || mode == "disabled" {
				continue
			}
			return true
		default:
			return true
		}
	}
	return false
}

// normalizeChatToolReasoningHistory 确保每个 assistant 工具调用回合都带 reasoning_content。
// 客户端可能丢弃这个非标准字段但保留 tool_calls，导致 thinking 模式请求无效。
// legacy reasoning 字符串会被提升；都没有则填占位。
func normalizeChatToolReasoningHistory(input map[string]any) bool {
	messages, ok := input["messages"].([]any)
	if !ok {
		return false
	}

	changed := false
	for _, rawMessage := range messages {
		message, ok := rawMessage.(map[string]any)
		if !ok || stringAt(message, "role") != "assistant" || len(sliceAt(message, "tool_calls")) == 0 {
			continue
		}
		if reasoning, ok := message["reasoning_content"].(string); ok && strings.TrimSpace(reasoning) != "" {
			continue
		}
		reasoning, _ := message["reasoning"].(string)
		if strings.TrimSpace(reasoning) == "" {
			reasoning = toolReasoningPlaceholder
		}
		message["reasoning_content"] = reasoning
		changed = true
	}
	return changed
}

// normalizeAnthropicToolThinkingHistory 修复含 tool_use 的 assistant 回合：
// 删 thinking.signature、redacted_thinking→普通 thinking、补缺失的 thinking 块。
func normalizeAnthropicToolThinkingHistory(input map[string]any) bool {
	messages, ok := input["messages"].([]any)
	if !ok {
		return false
	}

	changed := false
	for _, rawMessage := range messages {
		message, ok := rawMessage.(map[string]any)
		if !ok || stringAt(message, "role") != "assistant" {
			continue
		}
		content, ok := message["content"].([]any)
		if !ok || !anthropicContentHasType(content, "tool_use") {
			continue
		}

		hasThinking := false
		for i, rawBlock := range content {
			block, ok := rawBlock.(map[string]any)
			if !ok {
				continue
			}
			switch stringAt(block, "type") {
			case "thinking":
				hasThinking = true
				if _, exists := block["signature"]; exists {
					delete(block, "signature")
					changed = true
				}
				thinking, ok := block["thinking"].(string)
				if !ok || strings.TrimSpace(thinking) == "" {
					block["thinking"] = toolReasoningPlaceholder
					changed = true
				}
			case "redacted_thinking":
				hasThinking = true
				content[i] = map[string]any{
					"type":     "thinking",
					"thinking": anthropicRedactedThinkingPlaceholder,
				}
				changed = true
			}
		}
		if !hasThinking {
			content = append([]any{map[string]any{
				"type":     "thinking",
				"thinking": toolReasoningPlaceholder,
			}}, content...)
			message["content"] = content
			changed = true
		}
	}
	return changed
}

func anthropicContentHasType(content []any, kind string) bool {
	for _, rawBlock := range content {
		block, _ := rawBlock.(map[string]any)
		if stringAt(block, "type") == kind {
			return true
		}
	}
	return false
}
