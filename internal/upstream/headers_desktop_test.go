// SPDX-FileCopyrightText: 2026 yreidev 与贡献者（见 NOTICE）
// SPDX-License-Identifier: AGPL-3.0-only
// 复制或改写本文件须保留以上声明，并按 NOTICE 的署名条款注明出处：https://github.com/linguo2625469/workbuddy2api-panel

package upstream

import (
	"net/http"
	"regexp"
	"strings"
	"testing"
)

// TestDesktopProfileHeaders 锁定 WorkBuddy 桌面端（国内版 5.9.3）的真实出站头形态。
//
// 依据：2026-10-21 从 C:\Program Files\WorkBuddy\resources\app.asar 提取的
// buildVlmGatewayHeaders + applyGatewayHeaders + applyTraceHeaders，
// 并用本机安装的 CodeBuddy IDE 4.12.2（genie 扩展）交叉验证。
// 任何一项回归都意味着网关又变回了「能被指纹识别的非官方客户端」。
func TestDesktopProfileHeaders(t *testing.T) {
	c := &Client{} // Profile 空 = desktop
	req, _ := http.NewRequest(http.MethodPost, "https://copilot.tencent.com/v2/chat/completions", nil)
	c.ChatHeaders(req, testAuth(), "", ChatMeta{ConversationID: "conv-1"})

	want := map[string]string{
		"X-Product":  "SaaS",
		"X-IDE-Type": "WorkBuddy",
		"X-IDE-Name": "WorkBuddy",
		// buildVlmGatewayHeaders: X-IDE-Version = appVersion（真实版本）;
		// applyGatewayHeaders 的 "0.0.0" 只是兜底表，不能当默认值用。
		// testAuth() 无 Domain = 国内号，取 CN 内置版本。
		"X-IDE-Version":   defaultClientVersionCN,
		"X-Private-Data":  "true",
		"X-Agent-Intent":  "craft",
		"X-Agent-Purpose": "conversation",
		// 官方对话经 axios 的 http 适配器发出，压缩协商就是 axios 默认那组
		// （docs/official-client.md 第 136 行：官方没有在哪个请求上改写）。
		"Accept-Encoding": acceptEncodingAxios,
		// 官方 applyGatewayHeaders 里是小写 x-requested-with，HTTP 头名大小写不敏感。
		"X-Requested-With": "XMLHttpRequest",
		// 官方 chat 的 Accept 恒为 application/json，流式也一样。
		"Accept":            "application/json",
		"X-Conversation-ID": "conv-1",
	}
	for k, v := range want {
		if got := req.Header.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}

	// 官方客户端完全不发的头：带了就是可识别的反代特征。
	for _, k := range []string{
		"Origin", "Referer", "Accept-Language",
		"X-Machine-ID", "X-Session-ID", "X-Root-Request-ID",
		// 插件域的「显式声明缺省」约定，桌面端 chat 从不发（有值才设）。
		"X-No-User-Id", "X-No-Enterprise-Id", "X-No-Department-Info",
		// auth-state / 签到专用，chat 链路的注入钩子（getRequestHeaders →
		// buildVlmGatewayHeaders）根本不含它们。
		"X-Device-Token", "X-Device-Id-List", "X-Custom-Channel-Id",
	} {
		if v := req.Header.Get(k); v != "" {
			t.Errorf("%s = %q，桌面端 chat 不发该头", k, v)
		}
	}

	for _, k := range []string{"acp-connection-id", "traceparent", "b3", "X-B3-ParentSpanId"} {
		if req.Header.Get(k) == "" {
			t.Errorf("%s missing，桌面端每个 chat 请求都发", k)
		}
	}

	// 官方 withModelMessageCorrelation 用同一个 uuidv7()（去连字符）填
	// X-Conversation-Message-ID 与 X-Request-ID——两者同值是官方形态，不是复用偷懒。
	if req.Header.Get("X-Request-ID") != req.Header.Get("X-Conversation-Message-ID") {
		t.Error("X-Request-ID 应与 X-Conversation-Message-ID 同值（官方同一消息 id）")
	}
	// 入站没给 trace id 时以轮次 id 作 trace id（docs/official-client.md 只写了
	// 「链路头 traceparent、b3 与 X-B3-*」，未细化来源，沿用网关一直以来的口径）。
	if trace, conv := req.Header.Get("X-Trace-ID"), req.Header.Get("X-Conversation-Request-ID"); trace != conv {
		t.Errorf("X-Trace-ID = %q，应为轮次 id %q", trace, conv)
	}
	// 形状（docs/official-client.md「对话请求另有这些头和字段」）：
	//   会话 id = uuidv7() 带连字符的 36 位；轮次/消息 id = uuidv7() 去掉连字符的 32 hex。
	// 这里用空 ChatMeta 触发网关自己生成全部 ID（调用方显式传入的会话 ID 不在此列）。
	v7Dashed := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	hex32Re := regexp.MustCompile(`^[0-9a-f]{32}$`)
	anyUUID := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	gen, _ := http.NewRequest(http.MethodPost, "https://copilot.tencent.com/v2/chat/completions", nil)
	c.ChatHeaders(gen, testAuth(), "", ChatMeta{})
	if v := strings.ToLower(gen.Header.Get("X-Conversation-ID")); !v7Dashed.MatchString(v) {
		t.Errorf("X-Conversation-ID = %q，应为带连字符的 UUID v7 形态", v)
	}
	for _, k := range []string{
		"X-Conversation-Request-ID", "X-Conversation-Message-ID", "X-Request-ID",
	} {
		if v := gen.Header.Get(k); !hex32Re.MatchString(strings.ToLower(v)) {
			t.Errorf("%s = %q，应为去连字符的 UUID v7（32 hex）", k, v)
		}
	}
	// ACP 连接标识是网关按账号稳定派生的，形态必须仍是合法 UUID（不能是裸 hex）。
	if v := strings.ToLower(gen.Header.Get("acp-connection-id")); !anyUUID.MatchString(v) {
		t.Errorf("acp-connection-id = %q，应为带连字符的 UUID 形态", v)
	}
	// 调用方传入的会话键常是 32 位裸 hex（sessKey / RequestIDForKey），官方同字段恒为
	// 带连字符的 36 位，必须补上横线再发。
	hexed, _ := http.NewRequest(http.MethodPost, "https://copilot.tencent.com/v2/chat/completions", nil)
	c.ChatHeaders(hexed, testAuth(), "", ChatMeta{
		ConversationID:        strings.Repeat("a", 32),
		ConversationRequestID: strings.Repeat("b", 32),
	})
	if v := strings.ToLower(hexed.Header.Get("X-Conversation-ID")); !anyUUID.MatchString(v) {
		t.Errorf("X-Conversation-ID = %q，32 hex 入参应规范成带连字符的 36 位形态", v)
	}
	// 轮次 id 是 32 hex 的聚合主键，原样发（官方就是去连字符形态）。
	if v := hexed.Header.Get("X-Conversation-Request-ID"); v != strings.Repeat("b", 32) {
		t.Errorf("X-Conversation-Request-ID = %q，应为原样的 32 hex", v)
	}
	// 入站 X-Trace-ID 有值时透传（官方链路头带的是调用方给的 trace）。
	fixed, _ := http.NewRequest(http.MethodPost, "https://copilot.tencent.com/v2/chat/completions", nil)
	c.ChatHeaders(fixed, testAuth(), "", ChatMeta{TraceID: strings.Repeat("c", 32)})
	if got := fixed.Header.Get("X-Trace-ID"); got != strings.Repeat("c", 32) {
		t.Errorf("X-Trace-ID = %q，应透传入站值", got)
	}
}

// TestLegacyProfileRestoresOldHeaders 确认 "legacy" 能完整回到停更前的形态，
// 便于在 desktop 形态被上游拒绝时快速对照。
func TestLegacyProfileRestoresOldHeaders(t *testing.T) {
	c := &Client{Profile: "legacy"}
	req, _ := http.NewRequest(http.MethodPost, "https://copilot.tencent.com/v2/chat/completions", nil)
	c.ChatHeaders(req, testAuth(), "", ChatMeta{})

	if req.Header.Get("Origin") == "" {
		t.Error("legacy 应发 Origin")
	}
	if req.Header.Get("X-Machine-ID") == "" {
		t.Error("legacy 应发 X-Machine-ID")
	}
	if got := req.Header.Get("X-Product"); got != "WorkBuddy" {
		t.Errorf("legacy X-Product = %q, want WorkBuddy", got)
	}
	if req.Header.Get("X-Private-Data") != "" {
		t.Error("legacy 不应发 X-Private-Data")
	}
}
