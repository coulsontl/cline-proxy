package app

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// /v1/responses 的转换层以前完全没有单测，这里把纯函数和流式转换的行为钉住。

func mustJSON(t *testing.T, raw string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("unmarshal %s: %v", raw, err)
	}
	return m
}

func TestResponsesToChatMapsFields(t *testing.T) {
	out := responsesToChat(mustJSON(t, `{
		"model":"cline-free/deepseek-v4.1-flash",
		"stream":true,
		"max_output_tokens":1234,
		"temperature":0.5,
		"instructions":"你是助手",
		"input":"你好",
		"tools":[{"type":"function","name":"read_file","description":"读文件","parameters":{"type":"object"}}],
		"tool_choice":"auto",
		"unknown_field":1
	}`))

	if out["model"] != "cline-free/deepseek-v4.1-flash" || out["stream"] != true {
		t.Fatalf("model/stream not forwarded: %v", out)
	}
	if out["max_tokens"] != 1234 {
		t.Fatalf("max_output_tokens → max_tokens failed: %v", out["max_tokens"])
	}
	if out["temperature"] != 0.5 {
		t.Fatalf("temperature not forwarded: %v", out["temperature"])
	}
	messages, _ := out["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("messages = %v, want system + user", messages)
	}
	first, _ := messages[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "你是助手" {
		t.Fatalf("instructions must become a system message: %v", first)
	}
	second, _ := messages[1].(map[string]any)
	if second["role"] != "user" || second["content"] != "你好" {
		t.Fatalf("string input must become a user message: %v", second)
	}
	tools, _ := out["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %v", tools)
	}
	tool, _ := tools[0].(map[string]any)
	fn, _ := tool["function"].(map[string]any)
	if tool["type"] != "function" || fn["name"] != "read_file" {
		t.Fatalf("tools must be converted to chat shape: %v", tool)
	}
}

func TestResponsesInputToMessagesHandlesToolItems(t *testing.T) {
	messages := responsesInputToMessages([]any{
		map[string]any{"type": "message", "role": "user", "content": []any{
			map[string]any{"type": "input_text", "text": "第一行"},
			map[string]any{"type": "input_text", "text": "第二行"},
		}},
		map[string]any{"type": "function_call", "call_id": "call_1", "name": "read_file", "arguments": `{"path":"a.txt"}`},
		map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "文件内容"},
		map[string]any{"type": "reasoning", "summary": []any{}},
	})

	if len(messages) != 3 {
		t.Fatalf("reasoning items must be dropped, got %d messages: %v", len(messages), messages)
	}
	first, _ := messages[0].(map[string]any)
	if first["content"] != "第一行\n第二行" {
		t.Fatalf("content blocks must be joined: %v", first["content"])
	}
	call, _ := messages[1].(map[string]any)
	if call["role"] != "assistant" {
		t.Fatalf("function_call must become an assistant message: %v", call)
	}
	tc, _ := call["tool_calls"].([]any)
	if len(tc) != 1 {
		t.Fatalf("tool_calls = %v", tc)
	}
	callFn, _ := tc[0].(map[string]any)
	fn, _ := callFn["function"].(map[string]any)
	if callFn["id"] != "call_1" || fn["name"] != "read_file" || fn["arguments"] != `{"path":"a.txt"}` {
		t.Fatalf("function_call mapping wrong: %v", callFn)
	}
	toolMsg, _ := messages[2].(map[string]any)
	if toolMsg["role"] != "tool" || toolMsg["tool_call_id"] != "call_1" || toolMsg["content"] != "文件内容" {
		t.Fatalf("function_call_output mapping wrong: %v", toolMsg)
	}
}

func TestChatToResponsesCarriesUsage(t *testing.T) {
	out := chatToResponses(mustJSON(t, `{
		"model":"m",
		"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18,
		         "prompt_tokens_details":{"cached_tokens":3},"completion_tokens_details":{"reasoning_tokens":2}}
	}`))

	usage, _ := out["usage"].(map[string]any)
	if usage["input_tokens"] != float64(11) || usage["output_tokens"] != float64(7) || usage["total_tokens"] != float64(18) {
		t.Fatalf("usage not mapped: %v", usage)
	}
	details, _ := usage["input_tokens_details"].(map[string]any)
	if details["cached_tokens"] != float64(3) {
		t.Fatalf("cached_tokens not mapped: %v", details)
	}
	od, _ := usage["output_tokens_details"].(map[string]any)
	if od["reasoning_tokens"] != float64(2) {
		t.Fatalf("reasoning_tokens not mapped: %v", od)
	}
}

// 回归：usage 缺字段时不能写出 null（旧实现直接取 map 下标）
func TestChatToResponsesUsageWithoutDetailsIsZeroNotNil(t *testing.T) {
	out := chatToResponses(mustJSON(t, `{"model":"m","choices":[{"message":{"content":"hi"}}],"usage":{"total_tokens":5}}`))
	usage, _ := out["usage"].(map[string]any)
	if usage["input_tokens"] != 0 || usage["output_tokens"] != 0 {
		t.Fatalf("missing usage fields must be 0, got %v", usage)
	}
	if usage["total_tokens"] != float64(5) {
		t.Fatalf("total_tokens = %v", usage["total_tokens"])
	}
}

// 回归：多个工具调用以前会被揉成一个（名字取最后一个、参数拼在一起）。
func TestChatStreamToResponsesKeepsToolCallsSeparate(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"看看文件"}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","function":{"name":"read_file","arguments":"{\"pa"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"th\":\"a.txt\"}"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_b","function":{"name":"write_file","arguments":"{\"path\":\"b.txt\"}"}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":5,"completion_tokens":9,"total_tokens":14}}`,
		`data: [DONE]`,
	}, "\n\n") + "\n\n"

	rec := httptest.NewRecorder()
	upstream := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(stream))}
	if err := chatStreamToResponses(rec, upstream, nil); err != nil {
		t.Fatalf("chatStreamToResponses: %v", err)
	}

	events := parseSSEEvents(t, rec.Body.String())

	// 两个 function_call 各自 added/done，参数不串台
	var addedArgs []string
	var doneItems []map[string]any
	for _, e := range events {
		switch e.Name {
		case "response.output_item.added":
			if item, ok := e.Data["item"].(map[string]any); ok && item["type"] == "function_call" {
				addedArgs = append(addedArgs, "")
			}
		case "response.output_item.done":
			if item, ok := e.Data["item"].(map[string]any); ok && item["type"] == "function_call" {
				doneItems = append(doneItems, item)
			}
		}
	}
	if len(addedArgs) != 2 {
		t.Fatalf("expected 2 function_call items, got %d", len(addedArgs))
	}
	if len(doneItems) != 2 {
		t.Fatalf("expected 2 function_call done events, got %d", len(doneItems))
	}
	byName := map[string]string{}
	ids := map[string]bool{}
	for _, item := range doneItems {
		name, _ := item["name"].(string)
		args, _ := item["arguments"].(string)
		byName[name] = args
		id, _ := item["id"].(string)
		if ids[id] {
			t.Fatalf("duplicate item id %q", id)
		}
		ids[id] = true
		if callID, _ := item["call_id"].(string); callID == "" {
			t.Fatalf("call_id must be carried through: %v", item)
		}
	}
	if byName["read_file"] != `{"path":"a.txt"}` {
		t.Fatalf("first call arguments wrong: %q", byName["read_file"])
	}
	if byName["write_file"] != `{"path":"b.txt"}` {
		t.Fatalf("second call arguments wrong: %q", byName["write_file"])
	}

	// 文本项占 output_index 0，两个工具调用依次 1、2
	indexes := map[string]float64{}
	for _, e := range events {
		if e.Name != "response.output_item.done" {
			continue
		}
		item, _ := e.Data["item"].(map[string]any)
		name, _ := item["name"].(string)
		if name == "" {
			name = "message"
		}
		indexes[name], _ = e.Data["output_index"].(float64)
	}
	if indexes["message"] != 0 || indexes["read_file"] != 1 || indexes["write_file"] != 2 {
		t.Fatalf("output_index assignment wrong: %v", indexes)
	}

	// response.completed 必须带真实 usage 与 output 列表
	completed := lastEvent(t, events, "response.completed")
	resp, _ := completed.Data["response"].(map[string]any)
	usage, _ := resp["usage"].(map[string]any)
	if usage["input_tokens"] != float64(5) || usage["output_tokens"] != float64(9) || usage["total_tokens"] != float64(14) {
		t.Fatalf("stream usage must be the upstream usage, got %v", usage)
	}
	output, _ := resp["output"].([]any)
	if len(output) != 3 {
		t.Fatalf("completed.output should list message + 2 calls, got %v", output)
	}
	if resp["output_text"] != "看看文件" {
		t.Fatalf("output_text = %v", resp["output_text"])
	}
}

// 参数分片要发出 arguments.delta，客户端才不用等 done 才知道参数
func TestChatStreamToResponsesEmitsArgumentDeltas(t *testing.T) {
	stream := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"function\":{\"name\":\"read_file\",\"arguments\":\"{}\"}}]}}]}\n\n" +
		"data: [DONE]\n\n"
	rec := httptest.NewRecorder()
	upstream := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(stream))}
	if err := chatStreamToResponses(rec, upstream, nil); err != nil {
		t.Fatalf("chatStreamToResponses: %v", err)
	}
	if !strings.Contains(rec.Body.String(), "response.function_call_arguments.delta") {
		t.Fatalf("missing arguments delta events: %s", rec.Body.String())
	}
}

// 纯文本流：没有工具调用时形状不变（文本项仍是 output_index 0，正常 completed）
func TestChatStreamToResponsesTextOnlyUnchanged(t *testing.T) {
	stream := "data: {\"choices\":[{\"delta\":{\"content\":\"你好\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	rec := httptest.NewRecorder()
	upstream := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(stream))}
	if err := chatStreamToResponses(rec, upstream, nil); err != nil {
		t.Fatalf("chatStreamToResponses: %v", err)
	}
	events := parseSSEEvents(t, rec.Body.String())
	completed := lastEvent(t, events, "response.completed")
	if completed == nil {
		t.Fatal("missing response.completed")
	}
	resp, _ := completed.Data["response"].(map[string]any)
	if resp["output_text"] != "你好" {
		t.Fatalf("output_text = %v", resp["output_text"])
	}
	usage, _ := resp["usage"].(map[string]any)
	if usage["input_tokens"] != float64(0) || usage["output_tokens"] != float64(0) {
		t.Fatalf("no upstream usage → zeros, got %v", usage)
	}
}

// ---------- SSE 解析小工具 ----------

type sseEvent struct {
	Name string
	Data map[string]any
}

func parseSSEEvents(t *testing.T, body string) []sseEvent {
	t.Helper()
	var (
		events []sseEvent
		name   string
	)
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case strings.HasPrefix(line, "event: "):
			name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			var data map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &data); err != nil {
				t.Fatalf("bad event payload %q: %v", line, err)
			}
			events = append(events, sseEvent{Name: name, Data: data})
		}
	}
	return events
}

func lastEvent(t *testing.T, events []sseEvent, name string) *sseEvent {
	t.Helper()
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Name == name {
			return &events[i]
		}
	}
	return nil
}