package app

import "strings"

// Protocol 标识上游 API 协议。借鉴自 opencode2api。
type Protocol string

const (
	ProtocolChat      Protocol = "chat"      // OpenAI Chat Completions
	ProtocolResponses Protocol = "responses" // OpenAI Responses API
	ProtocolAnthropic Protocol = "anthropic" // Anthropic Messages
)

// Tier 标识上游渠道层级（zen / go）。借鉴自 opencode2api。
type Tier string

const (
	TierZen Tier = "zen"
	TierGo  Tier = "go"
)

// ---- 通用 JSON map helpers（借鉴自 opencode2api convert.go）----
// 这些函数沿 path 在嵌套 map[string]any 中取值，类型断言失败返回零值。

func anyAt(object map[string]any, path ...string) any {
	var current any = object
	for _, key := range path {
		next, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = next[key]
	}
	return current
}

func stringAt(object map[string]any, path ...string) string {
	value, _ := anyAt(object, path...).(string)
	return value
}

func sliceAt(object map[string]any, path ...string) []any {
	values, _ := anyAt(object, path...).([]any)
	return values
}

func mapAt(object map[string]any, path ...string) map[string]any {
	value, _ := anyAt(object, path...).(map[string]any)
	return value
}

func boolAt(object map[string]any, path ...string) bool {
	value, _ := anyAt(object, path...).(bool)
	return value
}

// firstString 返回 trim 后首个非空字符串。借鉴自 opencode2api ids.go。
func firstString(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
