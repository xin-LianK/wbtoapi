// SPDX-FileCopyrightText: 2026 yreidev 与贡献者（见 NOTICE）
// SPDX-License-Identifier: AGPL-3.0-only
// 复制或改写本文件须保留以上声明，并按 NOTICE 的署名条款注明出处：https://github.com/linguo2625469/workbuddy2api-panel

package server

import (
	"io"
	"net/http"
	"sync"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// identityRecorder 按出站身份决定怎么回，并记下每次请求的形态特征。
// 认身份用 X-IDE-Type（桌面端 WorkBuddy / VSCode 扩展 VSCode）与 acp-connection-id（CLI 没有）。
type identityRecorder struct {
	mu    sync.Mutex
	seen  []string // 每次出站的 X-IDE-Type
	acps  []bool   // 每次出站是否带 acp-connection-id
	rate  int      // 前 rate 次回 429，之后回正常流
	calls int
}

func (r *identityRecorder) rt(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.seen = append(r.seen, req.Header.Get("X-IDE-Type"))
	r.acps = append(r.acps, req.Header.Get("acp-connection-id") != "")
	if r.calls <= r.rate {
		return fullJSONResp(http.StatusTooManyRequests,
			`{"code":6004,"msg":"rate limit exceeded"}`), nil
	}
	return fullSSEResp(sseOK), nil
}

// rateLimitedBody 429 的响应体（6004 模型级限流，Classify → ErrSoftRate）。
func newIdentityHandler(t *testing.T, rec *identityRecorder, a *auth.Auth) *Handler {
	t.Helper()
	fastRotate(t)
	up := fullClient(rec.rt)
	p := testPoolWith(a)
	return NewHandler(Config{Pool: p, Upstream: up})
}

// TestIdentitySwitchesOnRateLimit auto 的号被 429 时限流换身份重发：桌面端 → VSCode，同号同请求，
// 第二次就成功；这个号不能被罚（429 只是这一条配额线打满了）。
func TestIdentitySwitchesOnRateLimit(t *testing.T) {
	a := &auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}
	a.SetProduct(auth.ProductAuto)
	rec := &identityRecorder{rate: 1}
	h := newIdentityHandler(t, rec, a)
	p := h.cfg.Pool

	if got := chatReq(h); got.Code != http.StatusOK {
		t.Fatalf("换身份后应成功: code=%d body=%s", got.Code, got.Body)
	}
	if rec.calls != 2 {
		t.Fatalf("上游应被打 2 次（桌面端 429 → VSCode 成功），实际 %d 次", rec.calls)
	}
	if rec.seen[0] != "WorkBuddy" {
		t.Errorf("第一次应是桌面端形态: %q", rec.seen[0])
	}
	if rec.seen[1] != "VSCode" {
		t.Errorf("429 之后应换 VSCode 扩展形态: %q", rec.seen[1])
	}
	if !rec.acps[0] || rec.acps[1] {
		t.Errorf("acp-connection-id 只在桌面端发: %v", rec.acps)
	}
	if st, _ := p.Status("u1"); st.Cooling {
		t.Errorf("换了身份成功就不该冷却这个号: %+v", st)
	}
}

// TestIdentitySwitchStopsAtLastLine 三条线都 429 就不再换，回到原有的 429 处置（这个号软冷却）。
func TestIdentitySwitchStopsAtLastLine(t *testing.T) {
	a := &auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}
	a.SetProduct(auth.ProductAuto)
	rec := &identityRecorder{rate: 99}
	h := newIdentityHandler(t, rec, a)

	if got := chatReq(h); got.Code != http.StatusTooManyRequests {
		t.Fatalf("三条线都限流应回 429: code=%d body=%s", got.Code, got.Body)
	}
	// 桌面 → VSCode → CLI 三跳，第 3 跳之后没有下一条线了。
	if rec.calls != 3 {
		t.Fatalf("最多试三条线，实际 %d 次", rec.calls)
	}
	want := []string{"WorkBuddy", "VSCode", "WorkBuddy"} // CLI 的 X-IDE-* 与桌面端同值，差别只在 ACP 头
	for i, w := range want {
		if rec.seen[i] != w {
			t.Errorf("第 %d 跳 X-IDE-Type = %q, want %q", i+1, rec.seen[i], w)
		}
	}
	if !rec.acps[0] || rec.acps[1] || rec.acps[2] {
		t.Errorf("只有桌面端带 acp-connection-id: %v", rec.acps)
	}
	if st, _ := h.cfg.Pool.Status("u1"); !st.Cooling {
		t.Errorf("三条线都 429 后这个号应软冷却（原有处置）: %+v", st)
	}
}

// TestIdentityPinnedDoesNotSwitch 钉死身份（desktop）的号不做自动换线：429 直接走原有处置，只打一次上游。
// 用户明确指定了形态，网关不去猜。
func TestIdentityPinnedDoesNotSwitch(t *testing.T) {
	a := &auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}
	a.SetProduct(auth.ProductDesktop)
	rec := &identityRecorder{rate: 99}
	h := newIdentityHandler(t, rec, a)

	if got := chatReq(h); got.Code != http.StatusTooManyRequests {
		t.Fatalf("应回 429: code=%d", got.Code)
	}
	if rec.calls != 1 {
		t.Fatalf("钉死身份的号只该打一次，实际 %d 次", rec.calls)
	}
}

// TestNextAutoIdentity 换线顺序与开闸条件（纯函数口径）。
func TestNextAutoIdentity(t *testing.T) {
	auto := &auth.Auth{UID: "u1"}
	auto.SetProduct(auth.ProductAuto)
	pinned := &auth.Auth{UID: "u2"}
	pinned.SetProduct(auth.ProductVSCode)

	rate := &upstream.Error{Kind: upstream.ErrSoftRate, Status: http.StatusTooManyRequests, Msg: "429"}
	cases := []struct {
		name   string
		acct   *auth.Auth
		meta   upstream.ChatMeta
		status int
		err    error
		want   string
	}{
		{"auto 首跳换 VSCode", auto, upstream.ChatMeta{}, http.StatusTooManyRequests, rate, auth.ProductVSCode},
		{"auto 第二跳换 CLI", auto, upstream.ChatMeta{Identity: auth.ProductVSCode}, http.StatusTooManyRequests, rate, auth.ProductCLI},
		{"auto 最后一跳不再换", auto, upstream.ChatMeta{Identity: auth.ProductCLI}, http.StatusTooManyRequests, rate, ""},
		{"钉死身份不换", pinned, upstream.ChatMeta{}, http.StatusTooManyRequests, rate, ""},
		{"非 429 不换", auto, upstream.ChatMeta{}, http.StatusBadRequest, &upstream.Error{Kind: upstream.ErrImageInvalid, Status: http.StatusBadRequest}, ""},
		{"传输层错误不换", auto, upstream.ChatMeta{}, 0, io.EOF, ""},
		{"无号不换", nil, upstream.ChatMeta{}, http.StatusTooManyRequests, rate, ""},
	}
	for _, c := range cases {
		if got := nextAutoIdentity(c.acct, c.meta, c.status, c.err); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
	// 防御分支：没有 *upstream.Error 信封时按状态码认 429。
	if got := nextAutoIdentity(auto, upstream.ChatMeta{}, http.StatusTooManyRequests, nil); got != auth.ProductVSCode {
		t.Errorf("裸 429 状态码没认出来: %q", got)
	}
	// 200 不算限流。
	if got := nextAutoIdentity(auto, upstream.ChatMeta{}, http.StatusOK, nil); got != "" {
		t.Errorf("200 不该触发换线: %q", got)
	}
}
