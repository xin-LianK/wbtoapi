// SPDX-FileCopyrightText: 2026 yreidev 与贡献者（见 NOTICE）
// SPDX-License-Identifier: AGPL-3.0-only
// 复制或改写本文件须保留以上声明，并按 NOTICE 的署名条款注明出处：https://github.com/linguo2625469/workbuddy2api-panel

package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestHarnessMetadataStepsWithinTurn 同一对话轮（同 X-Conversation-Request-ID）内连续两次
// 模型调用：第一步 stepIndex=0 不带 previous，第二步 stepIndex=1 且 previous 指向上一步的
// X-Request-ID（= X-Conversation-Message-ID），两次的消息 id 各自不同。
// 覆盖 handler 的 Peek/Commit 接线（字段本身见 upstream.TestInjectHarnessMetadata）。
func TestHarnessMetadataStepsWithinTurn(t *testing.T) {
	type call struct {
		messageID string
		body      []byte
	}
	var calls []call
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			calls = append(calls, call{messageID: r.Header.Get("X-Request-ID"), body: raw})
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN: "https://fake.example",
	}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "passthrough"})

	const turnID = "019d0c1a-0000-7000-8000-000000000001"
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("X-Conversation-Request-ID", turnID)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("第 %d 次 code=%d body=%s", i+1, rec.Code, rec.Body)
		}
	}
	if len(calls) != 2 {
		t.Fatalf("应向上游发两次，实际 %d", len(calls))
	}

	step := func(i int) (int, string) {
		var obj struct {
			ExtraVars struct {
				HarnessMetadata string `json:"harnessMetadata"`
			} `json:"extra_vars"`
		}
		if err := json.Unmarshal(calls[i].body, &obj); err != nil {
			t.Fatalf("第 %d 次出站 body 不可解析: %v (%s)", i+1, err, calls[i].body)
		}
		if obj.ExtraVars.HarnessMetadata == "" {
			t.Fatalf("第 %d 次缺 extra_vars.harnessMetadata: %s", i+1, calls[i].body)
		}
		var meta struct {
			StepIndex int `json:"stepIndex"`
			Previous  *struct {
				ConversationMessageID string `json:"conversationMessageId"`
			} `json:"previous"`
		}
		if err := json.Unmarshal([]byte(obj.ExtraVars.HarnessMetadata), &meta); err != nil {
			t.Fatalf("harnessMetadata 不是 JSON: %v", err)
		}
		prev := ""
		if meta.Previous != nil {
			prev = meta.Previous.ConversationMessageID
		}
		return meta.StepIndex, prev
	}

	if idx, prev := step(0); idx != 0 || prev != "" {
		t.Errorf("首步应为 stepIndex=0 且无 previous: idx=%d prev=%q", idx, prev)
	}
	if idx, prev := step(1); idx != 1 || prev != calls[0].messageID {
		t.Errorf("次步应为 stepIndex=1 且 previous=%q: idx=%d prev=%q", calls[0].messageID, idx, prev)
	}
	if calls[0].messageID == "" || calls[0].messageID == calls[1].messageID {
		t.Errorf("两条消息应各有自己的 id: %q / %q", calls[0].messageID, calls[1].messageID)
	}
}

// TestHarnessMetadataRetryReusesStep 同一请求内的换号重试（首跳 500 → 换号重发）复用同一
// stepIndex 与消息 id：重试不是新的一步（官方 startRetriedModelAttempt 语义）。
func TestHarnessMetadataRetryReusesStep(t *testing.T) {
	var bodies [][]byte
	var ids []string
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			bodies = append(bodies, raw)
			ids = append(ids, r.Header.Get("X-Request-ID"))
			if len(bodies) == 1 {
				return &http.Response{
					StatusCode: 500,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"code":500,"msg":"boom"}`)),
				}, nil
			}
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN: "https://fake.example",
	}
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "passthrough"})

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("X-Conversation-Request-ID", "019d0c1a-0000-7000-8000-000000000002")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if len(bodies) != 2 {
		t.Fatalf("应换号重发一次，实际 %d 次", len(bodies))
	}
	if ids[0] == "" || ids[0] != ids[1] {
		t.Errorf("重试应复用同一消息 id: %q / %q", ids[0], ids[1])
	}
	stepOf := func(raw []byte) int {
		var obj struct {
			ExtraVars struct {
				HarnessMetadata string `json:"harnessMetadata"`
			} `json:"extra_vars"`
		}
		if err := json.Unmarshal(raw, &obj); err != nil {
			t.Fatalf("body 不可解析: %v", err)
		}
		var meta struct {
			StepIndex int `json:"stepIndex"`
		}
		if err := json.Unmarshal([]byte(obj.ExtraVars.HarnessMetadata), &meta); err != nil {
			t.Fatalf("harnessMetadata 不是 JSON: %v (%q)", err, obj.ExtraVars.HarnessMetadata)
		}
		return meta.StepIndex
	}
	if a, b := stepOf(bodies[0]), stepOf(bodies[1]); a != 0 || b != 0 {
		t.Errorf("重试不应推进步数: %d / %d", a, b)
	}
}
