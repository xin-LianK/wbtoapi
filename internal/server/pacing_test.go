// SPDX-FileCopyrightText: 2026 yreidev 与贡献者（见 NOTICE）
// SPDX-License-Identifier: AGPL-3.0-only
// 复制或改写本文件须保留以上声明，并按 NOTICE 的署名条款注明出处：https://github.com/linguo2625469/workbuddy2api-panel

// pacing_test.go 出站节奏在 handler 主链路上的端到端回归。
//
// 为什么必须端到端测：pacer.Wait 的单元行为已由 internal/pace 覆盖，但那只证明
// 「Limiter 本身对」。这一层真正的失效模式是**接线位置错了**（放在轮转外、放在
// 成功分支后、或者干脆没传进 Handler）——那种情况下单元测试全绿而线上零效果，
// 属于最难发现的一类问题。
package server

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pace"
)

const chatBodyPacing = `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`

// upstreamStamps 记录每次真实出站时刻，用于断言节奏。
type upstreamStamps struct {
	mu sync.Mutex
	at []time.Time
}

func (s *upstreamStamps) record() {
	s.mu.Lock()
	s.at = append(s.at, time.Now())
	s.mu.Unlock()
}

func (s *upstreamStamps) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.at)
}

func (s *upstreamStamps) gap() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.at) < 2 {
		return 0
	}
	return s.at[1].Sub(s.at[0])
}

// TestPacingSpacesOutboundRequests 同一账号连续两次请求，出站间隔必须 ≥ MinInterval。
//
// 容差说明（这是修 flaky，不是放宽断言）：断言比较的两个 time.Time 都取自
// **upstream 收到请求**的时刻，而 pace.Wait 决定的是「Wait 何时返回」。两者之间
// 还夹着构建请求体 / 选号 / 持锁等处理，且第一次请求往往承担冷路径开销（首次
// 构建、缓存未热），δ₁ 常大于 δ₂：
//
//	间隔 = gap + (δ₂ - δ₁)
//
// 于是间隔会**合理地**略小于 gap（实测 119.6ms vs 120ms），在负载高的机器上
// （全量测试并行跑）这个差值会被放大，导致断言偶发失败——它测的其实是调度抖动，
// 不是 Pacer 有没有接线。
//
// 容差取 gap 的 1/8：Pacer 真没接上时间隔≈0（两个请求连着打），仍在检出范围内；
// 上界用 3×gap 兜「重复计时」（Wait 被调用两次会把间隔推到 240ms）。
func TestPacingSpacesOutboundRequests(t *testing.T) {
	st := &upstreamStamps{}
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		st.record()
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	const gap = 120 * time.Millisecond
	h := NewHandler(Config{Pool: p, Upstream: up, Pacer: pace.New(pace.Config{MinInterval: gap})})

	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(chatBodyPacing)))
		if rec.Code != 200 {
			t.Fatalf("request #%d code=%d body=%s", i, rec.Code, rec.Body)
		}
	}

	if got := st.count(); got != 2 {
		t.Fatalf("期望 2 次出站，got %d", got)
	}
	const tolerance = gap / 8
	if d := st.gap(); d < gap-tolerance {
		t.Fatalf("出站间隔 %v < MinInterval %v（容差 %v）—— Pacer 没接在发请求之前", d, gap, tolerance)
	} else if d > 3*gap {
		t.Fatalf("出站间隔 %v 远超 MinInterval %v —— Wait 可能被重复调用（重复计时）", d, gap)
	}
}

// TestPacingDisabledKeepsLatency 全关（零值 Config）时不得引入额外等待：
// 这是「关掉即回到改动前行为」的保证，防止 Wait 被误放在会阻塞的位置。
func TestPacingDisabledKeepsLatency(t *testing.T) {
	st := &upstreamStamps{}
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		st.record()
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, Pacer: pace.New(pace.Config{})})

	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(chatBodyPacing)))
		if rec.Code != 200 {
			t.Fatalf("request #%d code=%d body=%s", i, rec.Code, rec.Body)
		}
	}
	if d := st.gap(); d > 100*time.Millisecond {
		t.Fatalf("全关时仍有 %v 间隔——Wait 在没有配置的情况下阻塞了", d)
	}
}

// TestPacingWaitAbortStopsUpstream 节奏等待被客户端取消打断时，请求不得继续打到
// 上游（否则等于「客户端早跑了，网关还在替它消耗账号配额」）。
func TestPacingWaitAbortStopsUpstream(t *testing.T) {
	st := &upstreamStamps{}
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		st.record()
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	// 5s 间隔：第二次请求必然落在等待里，用 30ms 的 ctx 把它打断。
	h := NewHandler(Config{Pool: p, Upstream: up, Pacer: pace.New(pace.Config{MinInterval: 5 * time.Second})})

	first := httptest.NewRecorder()
	h.ServeHTTP(first, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(chatBodyPacing)))
	if first.Code != 200 {
		t.Fatalf("first code=%d body=%s", first.Code, first.Body)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	second := httptest.NewRecorder()
	h.ServeHTTP(second, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(chatBodyPacing)).WithContext(ctx))

	if second.Code == 200 {
		t.Fatalf("ctx 已超时仍返回 200：body=%s", second.Body)
	}
	if got := st.count(); got != 1 {
		t.Fatalf("被打断的请求不应再出站，出站次数=%d", got)
	}
}
