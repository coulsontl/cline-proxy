package app

import (
	"encoding/json"
	"strings"
	"sync"

	"github.com/tiktoken-go/tokenizer"
)

// 用 OpenAI 的 O200kBase 编码器本地估算输入 token 数（参考 cli-proxy-api 做法）。
// 对 GLM/OpenAI 系英文较准，中文略偏低但足够用于"明显超限"拦截。
var (
	tokenizerOnce  sync.Once
	tokenizerCodec tokenizer.Codec
	tokenizerErr   error
)

func getTokenizer() (tokenizer.Codec, error) {
	tokenizerOnce.Do(func() {
		tokenizerCodec, tokenizerErr = tokenizer.Get(tokenizer.O200kBase)
	})
	return tokenizerCodec, tokenizerErr
}

// countRequestTokens 估算 OpenAI 格式请求的输入 token 数。
// 把 messages 的 role+content、tools 的 name+description+parameters 拼接后计数。
func countRequestTokens(params map[string]any) int {
	enc, err := getTokenizer()
	if err != nil {
		return 0
	}
	segments := collectRequestTokenSegments(params)
	if len(segments) == 0 {
		return 0
	}
	n, err := enc.Count(strings.Join(segments, "\n"))
	if err != nil {
		return 0
	}
	return n
}

func collectRequestTokenSegments(params map[string]any) []string {
	segments := make([]string, 0, 16)

	// messages: [{role, content}, ...]
	if msgs, ok := params["messages"].([]any); ok {
		for _, m := range msgs {
			if mm, ok := m.(map[string]any); ok {
				if role, ok := mm["role"].(string); ok {
					segments = append(segments, role)
				}
				collectContentSegments(mm["content"], &segments)
			}
		}
	}

	// tools: [{function: {name, description, parameters}}, ...]
	if tools, ok := params["tools"].([]any); ok {
		for _, t := range tools {
			if tm, ok := t.(map[string]any); ok {
				if fn, ok := tm["function"].(map[string]any); ok {
					segments = append(segments, toString(fn["name"]))
					segments = append(segments, toString(fn["description"]))
					if params, ok := fn["parameters"]; ok {
						if b, err := json.Marshal(params); err == nil {
							segments = append(segments, string(b))
						}
					}
				}
			}
		}
	}

	return segments
}

func collectContentSegments(content any, segments *[]string) {
	if content == nil {
		return
	}
	switch c := content.(type) {
	case string:
		*segments = append(*segments, c)
	case []any:
		for _, part := range c {
			if pm, ok := part.(map[string]any); ok {
				if t, ok := pm["type"].(string); ok && t == "text" {
					*segments = append(*segments, toString(pm["text"]))
				}
				if t, ok := pm["type"].(string); ok && (t == "tool_use" || t == "tool_result") {
					*segments = append(*segments, toString(pm["id"]))
					*segments = append(*segments, toString(pm["name"]))
					collectContentSegments(pm["content"], segments)
				}
			}
		}
	case map[string]any:
		if t, ok := c["text"]; ok {
			*segments = append(*segments, toString(t))
		}
	}
}

func toString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	b, _ := json.Marshal(v)
	return string(b)
}
