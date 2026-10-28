package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResponsesInputToChat(t *testing.T) {
	req := &responsesRequest{
		Model:           "glm-5.2",
		Instructions:    "你是助手",
		Stream:          true,
		MaxOutputTokens: 128,
		Input: json.RawMessage(`[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"天气如何"}]},
			{"type":"function_call","name":"get_weather","arguments":"{\"city\":\"深圳\"}","call_id":"call_1"},
			{"type":"function_call_output","call_id":"call_1","output":"晴 28 度"}
		]`),
		Tools: []responsesTool{{
			Type:        "function",
			Name:        "get_weather",
			Description: "查天气",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`),
		}},
		ToolChoice: json.RawMessage(`"auto"`),
		Reasoning:  &struct{ Effort string `json:"effort"` }{Effort: "high"},
	}

	out := responsesToChat(req)
	if out["model"] != "glm-5.2" || asInt(out["max_tokens"]) != 128 || out["stream"] != true {
		t.Fatalf("基础字段=%+v", out)
	}
	if out["reasoning_effort"] != "high" {
		t.Errorf("reasoning_effort=%v want high", out["reasoning_effort"])
	}
	msgs, _ := out["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("messages 数量=%d want 4（system/user/assistant.tool_calls/tool）: %+v", len(msgs), msgs)
	}
	if got := asString(asMap(msgs[0])["role"]); got != "system" {
		t.Errorf("msgs[0].role=%q", got)
	}
	assistant := asMap(msgs[2])
	if asString(assistant["role"]) != "assistant" {
		t.Fatalf("msgs[2]=%+v", assistant)
	}
	calls, _ := assistant["tool_calls"].([]any)
	if len(calls) != 1 || asString(asMap(asMap(calls[0])["function"])["name"]) != "get_weather" {
		t.Errorf("tool_calls=%+v", assistant["tool_calls"])
	}
	toolMsg := asMap(msgs[3])
	if asString(toolMsg["role"]) != "tool" || asString(toolMsg["content"]) != "晴 28 度" {
		t.Errorf("tool 消息=%+v", toolMsg)
	}
	tools, _ := out["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools=%+v", out["tools"])
	}
	// Responses 的工具是扁平结构，转 chat 时必须包进 function 对象
	if fn := asMap(tools[0]); asString(fn["type"]) != "function" || asMap(fn["function"]) == nil {
		t.Errorf("tools[0]=%+v", tools[0])
	}
}

func TestResponsesStringInput(t *testing.T) {
	req := &responsesRequest{Model: "glm-5.2", Input: json.RawMessage(`"你好"`)}
	out := responsesToChat(req)
	msgs, _ := out["messages"].([]any)
	if len(msgs) != 1 || asString(asMap(msgs[0])["content"]) != "你好" {
		t.Fatalf("messages=%+v", out["messages"])
	}
}

func TestResponsesNonStreamShape(t *testing.T) {
	up := sseChunks(
		`{"id":"chatcmpl-1","model":"glm-5.2","choices":[{"index":0,"delta":{"content":"答案是 42"}}]}`,
		`{"id":"chatcmpl-1","model":"glm-5.2","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`{"id":"chatcmpl-1","model":"glm-5.2","choices":[],"usage":{"prompt_tokens":9,"completion_tokens":4,"total_tokens":13}}`,
	)
	h := anthropicHandler(t, up)
	body := `{"model":"glm-5.2","input":"问题"}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader([]byte(body))))
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Object     string `json:"object"`
		Status     string `json:"status"`
		OutputText string `json:"output_text"`
		Output     []struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("body 不是 JSON: %v / %s", err, rec.Body.String())
	}
	if out.Object != "response" || out.Status != "completed" || out.OutputText != "答案是 42" {
		t.Errorf("响应形态=%+v", out)
	}
	if len(out.Output) != 1 || out.Output[0].Type != "message" || out.Output[0].Content[0].Type != "output_text" {
		t.Errorf("output=%+v", out.Output)
	}
	if out.Usage.InputTokens != 9 || out.Usage.OutputTokens != 4 {
		t.Errorf("usage=%+v", out.Usage)
	}
}

func TestResponsesStreamEvents(t *testing.T) {
	up := sseChunks(
		`{"id":"chatcmpl-1","model":"glm-5.2","choices":[{"index":0,"delta":{"content":"你好"}}]}`,
		`{"id":"chatcmpl-1","model":"glm-5.2","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	)
	h := anthropicHandler(t, up)
	body := `{"model":"glm-5.2","input":"hi","stream":true}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader([]byte(body))))
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	events := sseEvents(rec.Body.String())
	must := map[string]bool{
		"response.created":          false,
		"response.output_item.added": false,
		"response.output_text.delta": false,
		"response.completed":        false,
	}
	for _, e := range events {
		if _, ok := must[e]; ok {
			must[e] = true
		}
	}
	for name, seen := range must {
		if !seen {
			t.Errorf("缺少事件 %s，实际序列=%v", name, events)
		}
	}
	// completed 事件里必须带得回完整文本（Codex 只认这一个终态对象）
	var completed map[string]any
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var v map[string]any
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &v) != nil {
			continue
		}
		if asString(v["type"]) == "response.completed" {
			completed = asMap(v["response"])
		}
	}
	if completed == nil {
		t.Fatalf("没有 response.completed: %s", rec.Body.String())
	}
	if asString(completed["output_text"]) != "你好" && !strings.Contains(rec.Body.String(), `"text":"你好"`) {
		t.Errorf("终态缺少正文: %+v", completed)
	}
}

// TestResponsesToChatTextFormat 覆盖「text.format 被静默丢弃」的回归
// （上游 issue #258）：Responses 的扁平 json_schema 必须转成 chat 的嵌套
// response_format，否则结构化输出约束根本到不了上游。
func TestResponsesToChatTextFormat(t *testing.T) {
	reqWithText := func(raw string) *responsesRequest {
		var r responsesRequest
		if err := json.Unmarshal([]byte(`{"model":"m","text":`+raw+`}`), &r); err != nil {
			t.Fatalf("unmarshal text=%s: %v", raw, err)
		}
		return &r
	}

	// json_schema：name/schema/strict 扁平 → 嵌套
	out := responsesToChat(reqWithText(`{"format":{"type":"json_schema","name":"approval","description":"审批结果","strict":true,"schema":{"type":"object","properties":{"ok":{"type":"boolean"}}}}}`))
	rf, ok := out["response_format"].(map[string]any)
	if !ok {
		t.Fatalf("response_format 缺失：%+v", out)
	}
	if rf["type"] != "json_schema" {
		t.Fatalf("type=%v want json_schema", rf["type"])
	}
	js, ok := rf["json_schema"].(map[string]any)
	if !ok {
		t.Fatalf("json_schema 形态错误：%+v", rf)
	}
	if js["name"] != "approval" || js["strict"] != true || js["description"] != "审批结果" {
		t.Errorf("json_schema 字段丢失：%+v", js)
	}
	if _, has := js["schema"]; !has {
		t.Errorf("schema 必须保留：%+v", js)
	}

	// json_schema 缺 name → chat 侧必填，兜底 response
	out = responsesToChat(reqWithText(`{"format":{"type":"json_schema","schema":{"type":"object"}}}`))
	rf, _ = out["response_format"].(map[string]any)
	js, _ = rf["json_schema"].(map[string]any)
	if js["name"] != "response" {
		t.Errorf("缺 name 时应兜底 response，得到 %v", js["name"])
	}

	// json_object 直通
	out = responsesToChat(reqWithText(`{"format":{"type":"json_object"}}`))
	rf, _ = out["response_format"].(map[string]any)
	if rf["type"] != "json_object" {
		t.Errorf("json_object 未直通：%+v", out["response_format"])
	}

	// text（无约束）/ json_schema 缺 schema / 完全没有 text：都不发 response_format
	for _, raw := range []string{
		`{"format":{"type":"text"}}`,
		`{"format":{"type":"json_schema"}}`,
		`{}`,
	} {
		if out := responsesToChat(reqWithText(raw)); out["response_format"] != nil {
			t.Errorf("text=%s 不应产出 response_format，得到 %+v", raw, out["response_format"])
		}
	}
	if out := responsesToChat(&responsesRequest{Model: "m"}); out["response_format"] != nil {
		t.Errorf("无 text 字段不应产出 response_format")
	}
}

// TestResponsesInputToChatMissingCallID 覆盖缺 call_id 的 function_call_output
// （上游 issue #311）：按声明顺序配对前面的 function_call，真孤儿丢弃——绝不让
// 空 tool_call_id 流到上游（chat 侧会被判参数错误整条打回）。
func TestResponsesInputToChatMissingCallID(t *testing.T) {
	msgs := responsesInputToChat(json.RawMessage(`[
		{"type":"function_call","name":"a","arguments":"{}","call_id":"call_a"},
		{"type":"function_call","name":"b","arguments":"{}","call_id":"call_b"},
		{"type":"function_call_output","output":"ra"},
		{"type":"function_call_output","output":"rb"}
	]`))
	if len(msgs) != 3 {
		t.Fatalf("messages=%d want 3（assistant + 两条 tool）: %+v", len(msgs), msgs)
	}
	toolA := asMap(msgs[1])
	toolB := asMap(msgs[2])
	if asString(toolA["tool_call_id"]) != "call_a" || asString(toolB["tool_call_id"]) != "call_b" {
		t.Errorf("按顺序配对失败: A=%+v B=%+v", toolA, toolB)
	}

	// 真孤儿：前面没有任何 function_call，丢弃而不是产出空 id
	msgs = responsesInputToChat(json.RawMessage(`[{"type":"function_call_output","output":"x"}]`))
	if len(msgs) != 0 {
		t.Fatalf("孤儿 output 应被丢弃，得到 %+v", msgs)
	}

	// 显式 call_id 仍优先使用，且不会污染待配对队列
	msgs = responsesInputToChat(json.RawMessage(`[
		{"type":"function_call","name":"a","arguments":"{}","call_id":"call_a"},
		{"type":"function_call_output","call_id":"call_a","output":"ra"}
	]`))
	if len(msgs) != 2 || asString(asMap(msgs[1])["tool_call_id"]) != "call_a" {
		t.Fatalf("显式 call_id 配对失败: %+v", msgs)
	}
}
