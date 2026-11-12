// SPDX-FileCopyrightText: 2026 yreidev 与贡献者（见 NOTICE）
// SPDX-License-Identifier: AGPL-3.0-only
// 复制或改写本文件须保留以上声明，并按 NOTICE 的署名条款注明出处：https://github.com/linguo2625469/workbuddy2api-panel

package server

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// completionsHandler 单账号 + 假上游的最小 handler。
func completionsHandler(t *testing.T, up string) *Handler {
	t.Helper()
	return NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: newFakeUpstream(t, func(string) (int, string, bool) { return 200, up, true }),
	})
}

func TestCompletionsToChatConversion(t *testing.T) {
	n := 3
	temp := 0.5
	cases := []struct {
		name       string
		req        completionsRequest
		wantMsgs   int
		wantMax    int
		wantPrompt string
		wantBad    string
	}{
		{
			name:       "字符串 prompt 用 legacy 缺省 max_tokens",
			req:        completionsRequest{Model: "glm-5.2", Prompt: json.RawMessage(`"你好"`)},
			wantMsgs:   1,
			wantMax:    legacyCompletionMaxTokens,
			wantPrompt: "你好",
		},
		{
			name:       "单元素数组 prompt 等价于字符串",
			req:        completionsRequest{Model: "glm-5.2", Prompt: json.RawMessage(`["你好"]`), MaxTokens: &n},
			wantMsgs:   1,
			wantMax:    3,
			wantPrompt: "你好",
		},
		{
			name:       "参数与 temperature 透传",
			req:        completionsRequest{Model: "glm-5.2", Prompt: json.RawMessage(`"x"`), Temperature: &temp},
			wantMsgs:   1,
			wantMax:    legacyCompletionMaxTokens,
			wantPrompt: "x",
		},
		{
			name:    "多 prompt 明确拒绝",
			req:     completionsRequest{Model: "m", Prompt: json.RawMessage(`["a","b"]`)},
			wantBad: "more than one",
		},
		{
			name:    "token id prompt 明确拒绝",
			req:     completionsRequest{Model: "m", Prompt: json.RawMessage(`[1,2,3]`)},
			wantBad: "token-id",
		},
		{
			name:    "suffix 明确拒绝",
			req:     completionsRequest{Model: "m", Prompt: json.RawMessage(`"x"`), Suffix: "!"},
			wantBad: "suffix",
		},
		{
			name:    "logprobs 明确拒绝",
			req:     completionsRequest{Model: "m", Prompt: json.RawMessage(`"x"`), Logprobs: &n},
			wantBad: "logprobs",
		},
		{
			name:    "best_of 明确拒绝",
			req:     completionsRequest{Model: "m", Prompt: json.RawMessage(`"x"`), BestOf: &n},
			wantBad: "best_of",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, prompt, bad := completionsToChat(&tc.req)
			if tc.wantBad != "" {
				if !strings.Contains(bad, tc.wantBad) {
					t.Fatalf("bad=%q 应包含 %q", bad, tc.wantBad)
				}
				return
			}
			if bad != "" {
				t.Fatalf("不支持的原因: %s", bad)
			}
			if prompt != tc.wantPrompt {
				t.Errorf("prompt=%q want %q", prompt, tc.wantPrompt)
			}
			if got := asInt(out["max_tokens"]); got != tc.wantMax {
				t.Errorf("max_tokens=%d want %d", got, tc.wantMax)
			}
			msgs, _ := out["messages"].([]any)
			if len(msgs) != tc.wantMsgs {
				t.Fatalf("messages 数量=%d want %d", len(msgs), tc.wantMsgs)
			}
			if got := asString(asMap(msgs[0])["content"]); got != tc.wantPrompt {
				t.Errorf("messages[0].content=%q want %q", got, tc.wantPrompt)
			}
			if tc.req.Temperature != nil {
				if got := out["temperature"]; got != *tc.req.Temperature {
					t.Errorf("temperature=%v want %v", got, *tc.req.Temperature)
				}
			}
		})
	}
}

// TestCompletionsNonStream 非流式：聊天聚合结果翻成 text_completion。
func TestCompletionsNonStream(t *testing.T) {
	h := completionsHandler(t, sseOK)
	body := []byte(`{"model":"glm-5.2","prompt":"你好"}`)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/completions", bytes.NewReader(body)))

	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Model   string `json:"model"`
		Created int64  `json:"created"`
		Choices []struct {
			Text         string `json:"text"`
			Index        int    `json:"index"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("解析响应失败: %v body=%s", err, rec.Body.String())
	}
	if got.Object != "text_completion" {
		t.Errorf("object=%q want text_completion", got.Object)
	}
	if !strings.HasPrefix(got.ID, "cmpl-") {
		t.Errorf("id=%q 应以 cmpl- 开头", got.ID)
	}
	if got.Model != "glm-5.2" {
		t.Errorf("model=%q", got.Model)
	}
	if got.Created == 0 {
		t.Error("created 不应为 0")
	}
	if len(got.Choices) != 1 || got.Choices[0].Text != "你好" {
		t.Fatalf("choices=%+v", got.Choices)
	}
	if got.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason=%q want stop", got.Choices[0].FinishReason)
	}
	if got.Usage.TotalTokens != 2 {
		t.Errorf("usage.total_tokens=%d want 2", got.Usage.TotalTokens)
	}
}

// TestCompletionsEcho 非流式 echo：prompt 原文拼在正文前。
func TestCompletionsEcho(t *testing.T) {
	h := completionsHandler(t, sseOK)
	body := []byte(`{"model":"glm-5.2","prompt":"前缀","echo":true}`)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/completions", bytes.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Choices []struct {
			Text string `json:"text"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(got.Choices) != 1 || got.Choices[0].Text != "前缀你好" {
		t.Fatalf("echo 结果=%+v want 前缀你好", got.Choices)
	}
}

// TestCompletionsStream 流式：逐帧翻成 text_completion 帧，[DONE] 保留。
func TestCompletionsStream(t *testing.T) {
	h := completionsHandler(t, sseOK)
	body := []byte(`{"model":"glm-5.2","prompt":"你好","stream":true}`)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/completions", bytes.NewReader(body)))

	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type=%q want text/event-stream", ct)
	}
	out := rec.Body.String()
	if !strings.Contains(out, `"object":"text_completion"`) {
		t.Fatalf("流里没有 text_completion 帧: %s", out)
	}
	if !strings.Contains(out, `"text":"你好"`) {
		t.Fatalf("流里缺少正文分片: %s", out)
	}
	if !strings.Contains(out, `"finish_reason":"stop"`) {
		t.Fatalf("流里缺少 finish_reason: %s", out)
	}
	if !strings.Contains(out, "data: [DONE]") {
		t.Fatalf("流里缺少 [DONE]: %s", out)
	}
	// 每帧都应是合法 JSON（除 [DONE]）。
	for _, line := range strings.Split(out, "\n") {
		payload, ok := strings.CutPrefix(line, "data: ")
		if !ok || payload == "[DONE]" || payload == "" {
			continue
		}
		var v any
		if err := json.Unmarshal([]byte(payload), &v); err != nil {
			t.Fatalf("帧不是合法 JSON: %v %q", err, payload)
		}
	}
}

// TestCompletionsRejectsBadPrompt 入口校验：缺 model / 坏 JSON / 不支持的参数都当场 400。
func TestCompletionsRejectsBadPrompt(t *testing.T) {
	h := completionsHandler(t, sseOK)
	cases := []struct{ name, body, wantCode string }{
		{"缺 model", `{"prompt":"x"}`, "invalid_request"},
		{"坏 JSON", `{`, "invalid_request"},
		{"多 prompt", `{"model":"m","prompt":["a","b"]}`, "unsupported_parameter"},
		{"suffix", `{"model":"m","prompt":"x","suffix":"!"}`, "unsupported_parameter"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/completions", strings.NewReader(tc.body)))
			if rec.Code != 400 {
				t.Fatalf("status=%d want 400 body=%s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.wantCode) {
				t.Errorf("错误码应为 %s: %s", tc.wantCode, rec.Body.String())
			}
		})
	}
}
