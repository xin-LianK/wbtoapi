// SPDX-FileCopyrightText: 2026 yreidev 与贡献者（见 NOTICE）
// SPDX-License-Identifier: AGPL-3.0-only
// 复制或改写本文件须保留以上声明，并按 NOTICE 的署名条款注明出处：https://github.com/linguo2625469/workbuddy2api-panel

package httpauth

import (
	"net/http/httptest"
	"testing"
	"time"
)

// fakeClock 让用例不真的等一分钟：只改 Guard 读的时钟，窗口和封禁时长用生产值。
func fakeClock(g *Guard) *time.Time {
	now := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	g.now = func() time.Time { return now }
	return &now
}

// TestGuardBlocksAfterLimit 窗口内连续失败到阈值就封，封禁期内直接拦；
// 已经在封的时候再失败不算「刚触发」（避免每个请求都推一条告警）。
func TestGuardBlocksAfterLimit(t *testing.T) {
	g := NewGuard()
	now := fakeClock(g)

	for i := 1; i < authFailLimit; i++ {
		if blocked, _ := g.Fail("1.2.3.4"); blocked {
			t.Fatalf("第 %d 次就封了，阈值是 %d", i, authFailLimit)
		}
	}
	if wait, ok := g.Blocked("1.2.3.4"); ok {
		t.Fatalf("没到阈值不该封: %s", wait)
	}
	blocked, fails := g.Fail("1.2.3.4")
	if !blocked || fails != authFailLimit {
		t.Fatalf("第 %d 次应触发封禁: blocked=%v fails=%d", authFailLimit, blocked, fails)
	}
	if wait, ok := g.Blocked("1.2.3.4"); !ok || wait <= 0 || wait > authBlockFor {
		t.Fatalf("封禁期不对: %s %v", wait, ok)
	}
	if blocked, _ := g.Fail("1.2.3.4"); blocked {
		t.Fatal("封禁期内再失败不该重复报「刚触发」")
	}

	// 别的来源不受影响。
	if _, ok := g.Blocked("5.6.7.8"); ok {
		t.Fatal("别的来源不该被连坐")
	}

	// 封禁到期后放行，且计数从头算。
	*now = now.Add(authBlockFor + time.Second)
	if wait, ok := g.Blocked("1.2.3.4"); ok {
		t.Fatalf("到期应解封: %s", wait)
	}
	if blocked, fails := g.Fail("1.2.3.4"); blocked || fails != 1 {
		t.Fatalf("解封后应从 1 重新计: blocked=%v fails=%d", blocked, fails)
	}
}

// TestGuardWindowExpires 失败次数按窗口计：窗口过去后旧计数不算，偶尔敲错不会攒到封禁。
func TestGuardWindowExpires(t *testing.T) {
	g := NewGuard()
	now := fakeClock(g)
	for i := 0; i < authFailLimit-1; i++ {
		g.Fail("9.9.9.9")
	}
	*now = now.Add(authFailWindow + time.Second)
	if blocked, fails := g.Fail("9.9.9.9"); blocked || fails != 1 {
		t.Fatalf("跨窗口应从 1 重新计: blocked=%v fails=%d", blocked, fails)
	}
}

// TestGuardReset 鉴权成功就清掉计数：正常用户敲错几次后不会再被旧账拖累。
func TestGuardReset(t *testing.T) {
	g := NewGuard()
	fakeClock(g)
	for i := 0; i < authFailLimit-1; i++ {
		g.Fail("2.2.2.2")
	}
	g.Reset("2.2.2.2")
	if blocked, fails := g.Fail("2.2.2.2"); blocked || fails != 1 {
		t.Fatalf("Reset 后应从 1 重新计: blocked=%v fails=%d", blocked, fails)
	}
}

// TestGuardNil 没装配限流器时全部放行（测试与未配置场景）。
func TestGuardNil(t *testing.T) {
	var g *Guard
	if _, ok := g.Blocked("1.1.1.1"); ok {
		t.Fatal("nil Guard 不该拦")
	}
	if blocked, _ := g.Fail("1.1.1.1"); blocked {
		t.Fatal("nil Guard 不该封")
	}
	g.Reset("1.1.1.1")
}

// TestClientIP 直连用 RemoteAddr；前面是本机/内网代理时才看 X-Forwarded-For 第一跳。
// 反过来做会让公网直连的请求靠伪造 XFF 换桶绕过限流。
func TestClientIP(t *testing.T) {
	cases := []struct {
		name   string
		remote string
		xff    string
		want   string
	}{
		{"公网直连无视 XFF", "203.0.113.9:51000", "1.2.3.4", "203.0.113.9"},
		{"内网代理解析 XFF", "192.168.1.10:51000", "198.51.100.7, 192.168.1.10", "198.51.100.7"},
		{"本机代理解析 XFF", "127.0.0.1:51000", "198.51.100.8", "198.51.100.8"},
		{"代理没带 XFF 用 RemoteAddr", "127.0.0.1:51000", "", "127.0.0.1"},
		{"XFF 不是 IP 不采信", "192.168.1.10:51000", "not-an-ip", "192.168.1.10"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
			r.RemoteAddr = tc.remote
			if tc.xff != "" {
				r.Header.Set("X-Forwarded-For", tc.xff)
			}
			if got := ClientIP(r); got != tc.want {
				t.Fatalf("ClientIP = %q，应为 %q", got, tc.want)
			}
		})
	}
}

// TestMaskPrefix 日志只留前 6 个字符，短串也不整串露出去。
func TestMaskPrefix(t *testing.T) {
	if got := MaskPrefix("sk-AbCdEfGhIjKlMnOp"); got != "sk-AbC…" {
		t.Fatalf("MaskPrefix = %q", got)
	}
	if got := MaskPrefix(""); got != "<none>" {
		t.Fatalf("空串 = %q", got)
	}
	if got := MaskPrefix("abc"); got != "a…" {
		t.Fatalf("短串 = %q", got)
	}
}
