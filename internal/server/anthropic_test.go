package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// sseChunks 拼一段标准 chat SSE 流（含 usage 尾帧与 [DONE]）。
func sseChunks(parts ...string) string {
	var b strings.Builder
	for _, p := range parts {
		b.WriteString("data: " + p + "\n\n")
	}
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

// anthropicHandler 构造一个最小可用的 handler（单账号 + 假上游）。
func anthropicHandler(t *testing.T, up string) *Handler {
	t.Helper()
	return NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: newFakeUpstream(t, func(string) (int, string, bool) { return 200, up, true }),
	})
}

// sseEvents 从响应体里抽出具名事件序列。
func sseEvents(body string) []string {
	var out []string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "event: ") {
			out = append(out, strings.TrimPrefix(line, "event: "))
		}
	}
	return out
}

func TestAnthropicToChatConversion(t *testing.T) {
	req := &anthropicRequest{
		Model:     "cn:glm-5.2",
		MaxTokens: 256,
		Stream:    true,
		System:    json.RawMessage(`[{"type":"text","text":"你是助手"}]`),
		Messages: []anthropicMessage{
			{Role: "user", Content: json.RawMessage(`"你好"`)},
			{Role: "assistant", Content: json.RawMessage(`[{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"深圳"}}]`)},
			{Role: "user", Content: json.RawMessage(`[{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"晴 28 度"}]},{"type":"text","text":"谢谢"}]`)},
		},
		Tools:      []anthropicTool{{Name: "get_weather", Description: "查天气", InputSchema: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`)}},
		ToolChoice: &anthropicToolChoice{Type: "auto"},
	}

	out := anthropicToChat(req)
	if out["model"] != "cn:glm-5.2" || out["stream"] != true || asInt(out["max_tokens"]) != 256 {
		t.Fatalf("基础字段转换错误: %+v", out)
	}
	msgs, _ := out["messages"].([]any)
	if len(msgs) != 5 {
		t.Fatalf("messages 数量=%d want 5（system/user/assistant/tool/user）: %+v", len(msgs), msgs)
	}
	if got := asString(asMap(msgs[0])["role"]); got != "system" {
		t.Errorf("msgs[0].role=%q want system", got)
	}
	if got := asString(asMap(msgs[0])["content"]); got != "你是助手" {
		t.Errorf("system 内容=%q", got)
	}
	assistant := asMap(msgs[2])
	if asString(assistant["role"]) != "assistant" {
		t.Errorf("msgs[2].role=%q want assistant", asString(assistant["role"]))
	}
	calls, _ := assistant["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("assistant.tool_calls=%v want 1", assistant["tool_calls"])
	}
	if fn := asMap(asMap(calls[0])["function"]); asString(fn["name"]) != "get_weather" {
		t.Errorf("tool name=%v", fn["name"])
	}
	// tool_result 必须先于同级 user 文本消息出现（否则上一轮 tool_calls 无应答）
	if got := asString(asMap(msgs[3])["role"]); got != "tool" {
		t.Errorf("msgs[3].role=%q want tool", got)
	}
	if got := asString(asMap(msgs[3])["tool_call_id"]); got != "toolu_1" {
		t.Errorf("tool_call_id=%q", got)
	}
	if got := asString(asMap(msgs[4])["content"]); got != "谢谢" {
		t.Errorf("msgs[4].content=%q want 谢谢", got)
	}
	tools, _ := out["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools=%v", out["tools"])
	}
	if fn := asMap(asMap(tools[0])["function"]); asString(fn["name"]) != "get_weather" {
		t.Errorf("tools[0].name=%v", fn["name"])
	}
	if out["tool_choice"] != "auto" {
		t.Errorf("tool_choice=%v want auto", out["tool_choice"])
	}
}

func TestAnthropicStreamEvents(t *testing.T) {
	up := sseChunks(
		`{"id":"chatcmpl-abc","object":"chat.completion.chunk","model":"glm-5.2","choices":[{"index":0,"delta":{"content":"你好"}}]}`,
		`{"id":"chatcmpl-abc","object":"chat.completion.chunk","model":"glm-5.2","choices":[{"index":0,"delta":{"content":"世界"}}]}`,
		`{"id":"chatcmpl-abc","object":"chat.completion.chunk","model":"glm-5.2","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`{"id":"chatcmpl-abc","object":"chat.completion.chunk","model":"glm-5.2","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
	)
	h := anthropicHandler(t, up)
	body := `{"model":"glm-5.2","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	events := sseEvents(rec.Body.String())
	want := []string{"message_start", "content_block_start", "content_block_delta", "content_block_delta", "content_block_stop", "message_delta", "message_stop"}
	if strings.Join(events, ",") != strings.Join(want, ",") {
		t.Fatalf("事件序列=%v\nwant %v\n原始响应:\n%s", events, want, rec.Body.String())
	}

	// 逐帧解析：delta 文本累积、message_delta 的 usage 必须带上上游真实值。
	var text strings.Builder
	var outputTokens int
	var stopReason string
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var v map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &v); err != nil {
			t.Fatalf("data 行不是合法 JSON: %q", line)
		}
		if asString(v["type"]) == "" {
			t.Fatalf("data 缺少 type: %q", line)
		}
		if d := asMap(v["delta"]); d != nil {
			if asString(d["type"]) == "text_delta" {
				text.WriteString(asString(d["text"]))
			}
			if s := asString(d["stop_reason"]); s != "" {
				stopReason = s
			}
		}
		if u := asMap(v["usage"]); u != nil {
			if n := asInt(u["output_tokens"]); n > 0 {
				outputTokens = n
			}
		}
	}
	if text.String() != "你好世界" {
		t.Errorf("正文=%q want 你好世界", text.String())
	}
	if stopReason != "end_turn" {
		t.Errorf("stop_reason=%q want end_turn", stopReason)
	}
	if outputTokens != 5 {
		t.Errorf("output_tokens=%d want 5（usage 尾帧必须被翻译）", outputTokens)
	}
}

func TestAnthropicToolCallStream(t *testing.T) {
	up := sseChunks(
		`{"id":"c1","model":"glm-5.2","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":""}}]}}]}`,
		`{"id":"c1","model":"glm-5.2","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":\"深圳\"}"}}]}}]}`,
		`{"id":"c1","model":"glm-5.2","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	)
	h := anthropicHandler(t, up)
	body := `{"model":"glm-5.2","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"天气"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(body))))

	out := rec.Body.String()
	for _, frag := range []string{`"type":"tool_use"`, `"id":"call_1"`, `"type":"input_json_delta"`, `"stop_reason":"tool_use"`} {
		if !strings.Contains(out, frag) {
			t.Errorf("缺少 %s\n%s", frag, out)
		}
	}
}

func TestAnthropicNonStreamJSON(t *testing.T) {
	up := sseChunks(
		`{"id":"chatcmpl-xyz","model":"glm-5.2","choices":[{"index":0,"delta":{"content":"结果"}}]}`,
		`{"id":"chatcmpl-xyz","model":"glm-5.2","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`,
	)
	h := anthropicHandler(t, up)
	body := `{"model":"glm-5.2","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(body))))

	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("body 不是 JSON: %v / %s", err, rec.Body.String())
	}
	if out.Type != "message" || out.Role != "assistant" || out.StopReason != "end_turn" {
		t.Errorf("响应形态=%+v", out)
	}
	if len(out.Content) != 1 || out.Content[0].Text != "结果" {
		t.Errorf("content=%+v", out.Content)
	}
	if out.Usage.InputTokens != 7 || out.Usage.OutputTokens != 3 {
		t.Errorf("usage=%+v", out.Usage)
	}
}

func TestAnthropicErrorTranslation(t *testing.T) {
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: newFakeUpstream(t, func(string) (int, string, bool) { return 500, `{"error":"boom"}`, false }),
	})
	body := `{"model":"glm-5.2","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(body))))

	var out struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("错误体不是 JSON: %v / %s", err, rec.Body.String())
	}
	if out.Type != "error" || out.Error.Type == "" || out.Error.Message == "" {
		t.Errorf("Anthropic 错误信封=%+v", out)
	}
}

func TestAnthropicCountTokens(t *testing.T) {
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})})
	body := `{"model":"glm-5.2","system":"系统提示","messages":[{"role":"user","content":"帮我统计一下这段文字有多少 token"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", bytes.NewReader([]byte(body))))
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		InputTokens int `json:"input_tokens"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("body: %v", err)
	}
	if out.InputTokens <= 0 {
		t.Errorf("input_tokens=%d want > 0", out.InputTokens)
	}
}

// TestAnthropicAcceptsXAPIKey 回归：Anthropic SDK 用 x-api-key，不发 Bearer。
func TestAnthropicAcceptsXAPIKey(t *testing.T) {
	h := NewHandler(Config{
		APIKey:   "secret",
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true }),
	})
	body := `{"model":"glm-5.2","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`

	// 正确 key（x-api-key）
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(body)))
	req.Header.Set("x-api-key", "secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == http.StatusUnauthorized {
		t.Fatalf("x-api-key 被拒: %s", rec.Body.String())
	}

	// 错误 key
	req2 := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(body)))
	req2.Header.Set("x-api-key", "wrong")
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusUnauthorized {
		t.Errorf("错误 key code=%d want 401", rec2.Code)
	}
}

// TestModelAliasRewrite 回归：客户端硬编码的模型名必须被改写成实际模型，
// 且改写要落到真正发给上游的请求体里（不是只改内存变量）。
func TestModelAliasRewrite(t *testing.T) {
	var gotModel string
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			var v struct {
				Model string `json:"model"`
			}
			_ = json.Unmarshal(raw, &v)
			gotModel = v.Model
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	h := NewHandler(Config{
		Pool:       testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream:   up,
		ModelAlias: map[string]string{"claude-sonnet-4-5": "glm-5.2", "*": "glm-5.2"},
	})
	body := `{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(body))))
	if gotModel != "glm-5.2" {
		t.Errorf("出站 model=%q want glm-5.2（别名未落到请求体）", gotModel)
	}

	// 兜底别名：未显式命中的名字走 "*"
	body2 := `{"model":"gpt-5-codex","messages":[{"role":"user","content":"hi"}]}`
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(body2))))
	if gotModel != "glm-5.2" {
		t.Errorf("兜底别名 model=%q want glm-5.2", gotModel)
	}
}
