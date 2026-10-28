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
