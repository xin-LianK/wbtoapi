// SPDX-FileCopyrightText: 2026 yreidev 与贡献者（见 NOTICE）
// SPDX-License-Identifier: AGPL-3.0-only
// 复制或改写本文件须保留以上声明，并按 NOTICE 的署名条款注明出处：https://github.com/linguo2625469/workbuddy2api-panel

package upstream

import (
	"net/http"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// vscodeAuth 一个身份被钉成 VSCode 扩展的号。
func vscodeAuth() *auth.Auth {
	a := testAuth()
	a.SetProduct(auth.ProductVSCode)
	return a
}

// TestVSCodeIdentityHeaders 锁定 VSCode 扩展形态的出站头：和桌面端差在 UA / X-IDE-* /
// X-Product-Version / X-Env-ID / 会话 id 形态 / 没有 X-Stainless-* 与 X-Agent-Runtime。
//
// 取值依据是 Open VSX 上 Tencent-Cloud.coding-copilot 4.12.39997873 的解包源码，
// **没有用真扩展抓包核对过**（见 identity.go 文件头），所以这里锁的是「网关自己的口径」，
// 不是「官方实测值」——将来真机抓包发现差异，改这个测试就是。
func TestVSCodeIdentityHeaders(t *testing.T) {
	c := &Client{}
	req, _ := http.NewRequest(http.MethodPost, "https://www.workbuddy.cn/v2/chat/completions", nil)
	c.ChatHeaders(req, vscodeAuth(), "", ChatMeta{
		ConversationID:        "0123456789abcdef0123456789abcdef",
		ConversationRequestID: "fedcba9876543210fedcba9876543210",
		Model:                 "claude-sonnet-4-5",
	})

	want := map[string]string{
		"User-Agent":              "VSCode/" + defaultVSCodeIDEVersion + " CodeBuddy/" + defaultVSCodeProductVersion,
		"X-IDE-Type":              "VSCode",
		"X-IDE-Name":              "VSCode",
		"X-IDE-Version":           defaultVSCodeIDEVersion,
		"X-Product-Version":       defaultVSCodeProductVersion,
		"X-Env-ID":                "production",
		"X-Model-ID":              "claude-sonnet-4-5",
		"X-Conversation-ID":       "0123456789abcdef0123456789abcdef",
		"X-Conversation-Request-": "fedcba9876543210fedcba9876543210",
		// 官方 model-provider 把轮级 id 原样写进 X-Request-ID（同值，不是巧合）。
		"X-Request-ID": "fedcba9876543210fedcba9876543210",
		// X-Private-Data 只在国际站发：testAuth 是国内号。
		"X-Private-Data": "",
	}
	for k, v := range want {
		if k == "X-Conversation-Request-" {
			if got := req.Header.Get("X-Conversation-Request-ID"); got != v {
				t.Errorf("X-Conversation-Request-ID = %q, want %q", got, v)
			}
			continue
		}
		if got := req.Header.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}

	// 扩展不经 OpenAI SDK、也不是 ACP 客户端：这些头一个都不能带。
	for _, k := range []string{
		"X-Stainless-Retry-Count", "X-Stainless-Timeout", "X-Stainless-Lang",
		"X-Stainless-Package-Version", "X-Stainless-OS", "X-Stainless-Arch",
		"X-Stainless-Runtime", "X-Stainless-Runtime-Version",
		"X-Agent-Runtime", "X-Agent-Purpose", "acp-connection-id",
	} {
		if got := req.Header.Get(k); got != "" {
			t.Errorf("%s = %q, want absent（VSCode 身份）", k, got)
		}
	}
	// 会话 id 一律 32 位无连字符（BaseAgent 的 uuidv4().replace(/-/g,"")）。
	for _, k := range []string{"X-Conversation-ID", "X-Conversation-Request-ID"} {
		if v := req.Header.Get(k); strings.Contains(v, "-") {
			t.Errorf("%s = %q, want 无连字符", k, v)
		}
	}
	// X-Request-Trace-Id 是带连字符的 UUID（扩展自己生成的 trace）。
	if v := req.Header.Get("X-Request-Trace-Id"); len(v) != 36 || strings.Count(v, "-") != 4 {
		t.Errorf("X-Request-Trace-Id = %q, want 带连字符的 UUID", v)
	}
	// 两个 monitor 时间戳是毫秒，成对出现。
	for _, k := range []string{"monitor_promptPrepareStartTime", "monitor_httpSendTime"} {
		if v := req.Header.Get(k); len(v) != 13 {
			t.Errorf("%s = %q, want 13 位毫秒时间戳", k, v)
		}
	}
}

// TestVSCodeIdentityKeepsAccountHeaders 换身份只换形态，账号头必须还在。
func TestVSCodeIdentityKeepsAccountHeaders(t *testing.T) {
	c := &Client{}
	req, _ := http.NewRequest(http.MethodPost, "https://www.workbuddy.cn/v2/chat/completions", nil)
	c.ChatHeaders(req, vscodeAuth(), "", ChatMeta{ConversationID: "c1"})

	for k, v := range map[string]string{
		"Authorization":  "Bearer at",
		"X-User-Id":      "u1",
		"X-Product":      "SaaS",
		"X-Agent-Intent": "craft",
		"X-Agent-Type":   "main",
	} {
		if got := req.Header.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if req.Header.Get("X-Conversation-Message-ID") == "" {
		t.Error("X-Conversation-Message-ID 缺失")
	}
}

// TestCLIIdentityDropsACP CLI 身份与桌面端只差一个 acp-connection-id（见 identity.go 文件头）。
func TestCLIIdentityDropsACP(t *testing.T) {
	a := testAuth()
	a.SetProduct(auth.ProductCLI)
	c := &Client{}
	req, _ := http.NewRequest(http.MethodPost, "https://www.workbuddy.cn/v2/chat/completions", nil)
	c.ChatHeaders(req, a, "", ChatMeta{ConversationID: "c1"})

	if got := req.Header.Get("acp-connection-id"); got != "" {
		t.Errorf("acp-connection-id = %q, want absent（CLI 是纯 CLI 进程）", got)
	}
	// 其余形态不变：还是桌面端内置 CLI 的 UA 与 X-IDE-*。
	if got, want := req.Header.Get("User-Agent"), c.defaultCLIUA(a); got != want {
		t.Errorf("User-Agent = %q, want %q", got, want)
	}
	if got := req.Header.Get("X-IDE-Type"); got != "WorkBuddy" {
		t.Errorf("X-IDE-Type = %q, want WorkBuddy", got)
	}
}

// TestIdentityDefaultsToDesktop 缺省（product 空 / auto）就是桌面端，且以后加身份不改变默认。
func TestIdentityDefaultsToDesktop(t *testing.T) {
	c := &Client{}
	for _, p := range []string{"", auth.ProductAuto, auth.ProductDesktop, "nonsense"} {
		a := testAuth()
		a.SetProduct(p)
		req, _ := http.NewRequest(http.MethodPost, "https://www.workbuddy.cn/v2/chat/completions", nil)
		c.ChatHeaders(req, a, "", ChatMeta{ConversationID: "c1"})
		if got := c.effectiveIdentity(a, ChatMeta{}); got != auth.ProductDesktop {
			t.Errorf("product=%q effectiveIdentity = %q, want desktop", p, got)
		}
		if req.Header.Get("acp-connection-id") == "" {
			t.Errorf("product=%q 应保留桌面端的 acp-connection-id", p)
		}
		if req.Header.Get("X-Stainless-Lang") != "js" {
			t.Errorf("product=%q 应保留 X-Stainless-*（桌面端内置 CLI 经 SDK 发）", p)
		}
	}
	// meta.Identity 显式指定时压过账号设置（服务端换身份重发走这条路）。
	a := vscodeAuth()
	if got := c.effectiveIdentity(a, ChatMeta{Identity: auth.ProductCLI}); got != auth.ProductCLI {
		t.Errorf("meta.Identity 未生效：%q", got)
	}
}

// TestVSCodeVersionOverrides 配置项能覆盖两个版本段（面板/环境变量都落在这里）。
func TestVSCodeVersionOverrides(t *testing.T) {
	c := &Client{VSCodeIDEVersion: "1.96.0", VSCodeProductVersion: "4.11.1"}
	if got, want := c.vscodeUA(), "VSCode/1.96.0 CodeBuddy/4.11.1"; got != want {
		t.Errorf("vscodeUA = %q, want %q", got, want)
	}
	blank := &Client{VSCodeIDEVersion: "   ", VSCodeProductVersion: ""}
	if got, want := blank.vscodeUA(),
		"VSCode/"+defaultVSCodeIDEVersion+" CodeBuddy/"+defaultVSCodeProductVersion; got != want {
		t.Errorf("空白覆盖应回落内置默认：%q, want %q", got, want)
	}
}
