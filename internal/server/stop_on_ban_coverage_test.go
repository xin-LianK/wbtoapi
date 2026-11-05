// SPDX-FileCopyrightText: 2026 yreidev 与贡献者（见 NOTICE）
// SPDX-License-Identifier: AGPL-3.0-only
// 复制或改写本文件须保留以上声明，并按 NOTICE 的署名条款注明出处：https://github.com/linguo2625469/workbuddy2api-panel

// stop_on_ban_coverage_test.go 补齐「封号即停轮转」在三处处置路径上的盲区。
//
// 已有覆盖（handler_test.go）只打了 chat 的**非流式 HTTP 错误**路径：
//   - TestChatAccountBannedStopsRotation（11014 字段）
//   - TestChatAccountRestrictedStopsRotation（11140 英文）
//   - TestChatAccountRestrictedCnStopsRotation（11140 中文）
//   - TestChatAccountBannedContinueRotationWhenStopDisabled（开关逃生门）
//
// 但代码里还有两个独立的封禁处置口，此前没有任何测试：
//  1. 流式 200 + error 帧（handler.go 的 errFrame 分支）——回包已开始、无法换号，
//     处置应只落在「硬禁用 + 告警」上，且不得把账号判成「健康」（旧 bug 形态）。
//  2. search.go 的 runSearch（/v1/search/messages）——注释声称「与 chat 完全同口径」，
//     实际从未被验证过；若这条路径漏掉 stopRotationOnBan，同一份搜索请求会把
//     整池号逐个打成封禁，而搜索流量通常是 agent 自动发起的，连坐更隐蔽。
package server

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestChatStreamErrorFrameBanDisablesAccount 流式路径的封禁：上游 200 已开流，
// 用 error 帧下发「account is disabled」。
//
// 该路径无法换号（响应头已发、字节已透传），所以断言落在三件事上：
//   - 账号被硬禁用（退出后续选号），不能只当 6004 类软限流处理；
//   - error 帧原文照常透传给客户端（error-passthrough，客户端要能看到 code/msg）；
//   - 另一个号零调用——不是为了「终止轮转」，而是确认没有走成换号路径。
func TestChatStreamErrorFrameBanDisablesAccount(t *testing.T) {
	const banFrame = `{"error":{"code":11014,"message":"account is disabled"}}`
	stream := "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hi\"}}]}\n\n" +
		"data: " + banFrame + "\n\n" +
		"data: [DONE]\n\n"

	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		return 200, stream, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "s-banned", AccessToken: "at-s-banned", ExpiresAt: 9999999999},
		&auth.Auth{UID: "s-good", AccessToken: "at-s-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("s-banned", 2000, 0) // 确定性源 r=0 → 先选 s-banned
	p.SetCredits("s-good", 1000, 0)
	h := NewHandler(Config{Pool: p, Upstream: up, StopOnBan: true})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[]}`)))

	// 硬禁用：流式 error 帧封禁与 HTTP 403 封禁同族，必须退出池子。
	if st, _ := p.Status("s-banned"); !st.Disabled {
		t.Errorf("流式 error 帧封禁必须硬禁用该号: %+v", st)
	}
	// 不得记成功：否则粘性会把会话钉在这个已死的号上（旧 bug 形态）。
	// 封禁判定走的是硬禁用（BanSuspect），不靠 ErrTotal 计数，这里断言禁用原因可辨识。
	if st, _ := p.Status("s-banned"); !st.BanSuspect {
		t.Errorf("流式封禁应标记 BanSuspect（面板展示「疑似误封·可解封」）: %+v", st)
	}
	if calls["Bearer at-s-good"] != 0 {
		t.Errorf("calls=%v want s-good 0 次（回包已开始，不应再换号）", calls)
	}
	if st, _ := p.Status("s-good"); st.Disabled || st.Cooling || st.ErrTotal != 0 || st.BreakerFails != 0 {
		t.Errorf("流式封禁不得连坐未参与请求的号: %+v", st)
	}
	// error-passthrough：上游 code/msg 必须到达客户端。
	if body := rec.Body.String(); !strings.Contains(body, "account is disabled") {
		t.Errorf("error 帧原文未透传: %s", body)
	}
}

// TestSearchAccountBannedStopsRotation search 链路（/v1/search/messages 的内核
// runSearch）的封禁 fail-fast：与 chat 同口径——封禁号只打一次、另一个号零调用、
// 未参与的号不被连坐，出口 403 透传上游原文。
//
// 回归的是「注释声称同口径、实现却是两套」的风险：search 与 chat 不共享轮转代码，
// 任一侧漏掉 stopRotationOnBan 都只有测试能发现。
func TestSearchAccountBannedStopsRotation(t *testing.T) {
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		return 403, `{"code":11014,"msg":"account is disabled"}`, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "x-banned", AccessToken: "at-x-banned", ExpiresAt: 9999999999},
		&auth.Auth{UID: "x-good", AccessToken: "at-x-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("x-banned", 2000, 0) // 确定性源 r=0 → 先选 x-banned
	p.SetCredits("x-good", 1000, 0)
	h := NewHandler(Config{Pool: p, Upstream: up, StopOnBan: true})

	out, fail := h.runSearch(context.Background(), upstream.SearchQuery{Query: "golang context cancellation"}, "", upstream.ChatMeta{}, newSearchStat(nil, false))
	if out != nil {
		t.Fatalf("封禁场景不应返回搜索结果: %+v", out)
	}
	if fail == nil {
		t.Fatal("封禁场景应返回失败状态")
	}
	if calls["Bearer at-x-banned"] != 1 || calls["Bearer at-x-good"] != 0 {
		t.Errorf("calls=%v want x-banned 1 次、x-good 0 次（搜索封禁也必须停止轮转）", calls)
	}
	if st, _ := p.Status("x-banned"); !st.Disabled {
		t.Errorf("搜索命中的封禁号必须硬禁用: %+v", st)
	}
	if st, _ := p.Status("x-good"); st.Disabled || st.Cooling || st.ErrTotal != 0 || st.BreakerFails != 0 {
		t.Errorf("搜索封禁 fail-fast 不得惩罚未参与轮转的号: %+v", st)
	}
	// 403 而非 503：503 会被 SDK 当可重试，重试是新请求、会重新选号。
	if fail.status != 403 {
		t.Errorf("status=%d want 403（503 会让 SDK 跨请求重试，等于把连坐搬出单请求）", fail.status)
	}
	if !strings.Contains(fail.msg, "account is disabled") {
		t.Errorf("应透传上游原文: %s", fail.msg)
	}
}

// TestSearchAccountRestrictedCnStopsRotation search 链路的 11140 中文形态：
// 同一 banKind 判定的第二个来源（ErrAccountFault + 文案），确认 search 侧
// 不是只判了 ErrAccountBanned 而漏了 11140。
func TestSearchAccountRestrictedCnStopsRotation(t *testing.T) {
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		return 403, `{"code":11140,"msg":"账号存在异常行为，已被限制使用"}`, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "x-restr", AccessToken: "at-x-restr", ExpiresAt: 9999999999},
		&auth.Auth{UID: "x-good", AccessToken: "at-x-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("x-restr", 2000, 0)
	p.SetCredits("x-good", 1000, 0)
	h := NewHandler(Config{Pool: p, Upstream: up, StopOnBan: true})

	_, fail := h.runSearch(context.Background(), upstream.SearchQuery{Query: "golang context cancellation"}, "", upstream.ChatMeta{}, newSearchStat(nil, false))
	if calls["Bearer at-x-restr"] != 1 || calls["Bearer at-x-good"] != 0 {
		t.Errorf("calls=%v want x-restr 1 次、x-good 0 次（11140 中文同样停止搜索轮转）", calls)
	}
	if st, _ := p.Status("x-restr"); !st.Disabled {
		t.Errorf("search 11140 中文必须硬禁用: %+v", st)
	}
	if fail == nil || fail.status != 403 {
		t.Errorf("want 403 account_restricted, got %+v", fail)
	}
}

// TestAllInFlightReturns503NoHealthyAccount 单号池名额占满时的末端行为：
// 直接 503 no_healthy_account，**不排队等待**。
//
// 这对「3 个 agent 同步跑」是决定性的一条：并发数超过「号数 × max_in_flight」时，
// 多出来的请求不会排队变慢，而是立刻失败。据此可以判断要不要加号或调高上限。
func TestAllInFlightReturns503NoHealthyAccount(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "only", AccessToken: "at-only", ExpiresAt: 9999999999})
	p.SetMaxInFlight(3)
	// 模拟 3 个在途请求已占满全部名额（等价于 3 个 agent 正在跑长对话）。
	for i := 0; i < 3; i++ {
		if !p.Acquire("only") {
			t.Fatalf("第 %d 个名额应可占用", i+1)
		}
	}
	h := NewHandler(Config{Pool: p, Upstream: up, StopOnBan: true})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))

	if rec.Code != 503 {
		t.Fatalf("code=%d want 503（名额占满应快速失败，不排队）body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "no_healthy_account") {
		t.Errorf("应为 no_healthy_account: %s", rec.Body)
	}
}
