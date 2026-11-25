// SPDX-FileCopyrightText: 2026 yreidev 与贡献者（见 NOTICE）
// SPDX-License-Identifier: AGPL-3.0-only
// 复制或改写本文件须保留以上声明，并按 NOTICE 的署名条款注明出处：https://github.com/linguo2625469/workbuddy2api-panel

package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// stalledKeepaliveBody 模拟 issue #485 的上游形态：连接不断、保活帧一直在发，但没有正文帧。
// 底流照真实 http body 的行为盯请求 context —— 看门狗 cancel 后下一次 Read 返回 ctx 错误，
// 由 monitorBody 换成 upstream.ErrStreamIdle。
type stalledKeepaliveBody struct {
	ctx context.Context
}

func (b *stalledKeepaliveBody) Read(p []byte) (int, error) {
	select {
	case <-b.ctx.Done():
		return 0, b.ctx.Err()
	default:
	}
	// 注释行保活帧：SSE 里不算内容（见 upstream.isContentLine）。
	return copy(p, ": keepalive\n\n"), nil
}

func (b *stalledKeepaliveBody) Close() error { return nil }

// TestUpstreamStalledReturns504 上游只发保活帧、idle 窗口内没有内容帧 → 非流式请求回
// 504 upstream_idle（而不是把停顿当成「客户端断连」按成功记账）。
func TestUpstreamStalledReturns504(t *testing.T) {
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       &stalledKeepaliveBody{ctx: r.Context()},
			}, nil
		})},
		ChatBaseCN:  "https://fake.example",
		IdleTimeout: 60 * time.Millisecond,
	}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "passthrough"})

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("code=%d want 504（body=%s）", rec.Code, rec.Body)
	}
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, rec.Body.String())
	}
	if env.Error.Code != "upstream_idle" {
		t.Errorf("error.code=%q want upstream_idle", env.Error.Code)
	}
}
