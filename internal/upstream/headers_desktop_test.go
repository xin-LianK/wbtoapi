package upstream

import (
	"net/http"
	"regexp"
	"strings"
	"testing"
)

// TestDesktopProfileHeaders 锁定 WorkBuddy 桌面端（5.8.3）的真实出站头形态。
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
		"X-IDE-Version":   defaultClientVersion,
		"X-Private-Data":  "true",
		"X-Agent-Intent":  "craft",
		"X-Agent-Purpose": "conversation",
		"Accept-Encoding": "identity",
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

	// 两个 ID 必须独立：官方 applyGatewayHeaders 是两次 randomUUID()。
	if req.Header.Get("X-Request-ID") == req.Header.Get("X-Conversation-Message-ID") {
		t.Error("X-Request-ID 与 X-Conversation-Message-ID 不应同值")
	}
	// 链路 trace 也必须独立：官方 applyTraceHeaders 每次 randomBytes(16)，
	// 与 conversationRequestId 无关。恒等即暴露反代。
	if trace, conv := req.Header.Get("X-Trace-ID"), strings.ReplaceAll(req.Header.Get("X-Conversation-Request-ID"), "-", ""); trace == conv {
		t.Error("X-Trace-ID 不应等于 X-Conversation-Request-ID")
	}
	// 官方 ID 全部出自 crypto.randomUUID()，必须是 v4：第 13 位 = 4，
	// 第 17 位 ∈ 8/9/a/b。只对齐「带连字符」而版本位乱跳，仍可被一条正则识别。
	// 这里用空 ChatMeta 触发网关自己生成全部 ID（调用方显式传入的会话 ID 不在此列）。
	uuidV4Re := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	gen, _ := http.NewRequest(http.MethodPost, "https://copilot.tencent.com/v2/chat/completions", nil)
	c.ChatHeaders(gen, testAuth(), "", ChatMeta{})
	for _, k := range []string{
		"acp-connection-id", "X-Conversation-ID", "X-Conversation-Request-ID",
		"X-Conversation-Message-ID", "X-Request-ID",
	} {
		if v := gen.Header.Get(k); !uuidV4Re.MatchString(strings.ToLower(v)) {
			t.Errorf("%s = %q, want UUID v4 形态", k, v)
		}
	}
	// 调用方传入的聚合键是 32 位裸 hex（TurnRequestID / RequestIDForKey / sessKey），
	// 官方同字段恒为 randomUUID 的 8-4-4-4-12 形态，必须规范化后再发。
	hexed, _ := http.NewRequest(http.MethodPost, "https://copilot.tencent.com/v2/chat/completions", nil)
	c.ChatHeaders(hexed, testAuth(), "", ChatMeta{
		ConversationID:        strings.Repeat("a", 32),
		ConversationRequestID: strings.Repeat("b", 32),
	})
	for _, k := range []string{"X-Conversation-ID", "X-Conversation-Request-ID"} {
		if v := hexed.Header.Get(k); !uuidV4Re.MatchString(strings.ToLower(v)) {
			t.Errorf("%s = %q，32 hex 入参应规范成 UUID v4 形态", k, v)
		}
	}
	// 入站 X-Trace-ID 一律无视：官方每请求 randomBytes(16)，透传常量即变成固定值。
	fixed, _ := http.NewRequest(http.MethodPost, "https://copilot.tencent.com/v2/chat/completions", nil)
	c.ChatHeaders(fixed, testAuth(), "", ChatMeta{TraceID: strings.Repeat("c", 32)})
	if got := fixed.Header.Get("X-Trace-ID"); strings.Contains(got, "cccc") {
		t.Errorf("X-Trace-ID 不该透传入站值，得到 %q", got)
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
