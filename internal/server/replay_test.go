// SPDX-FileCopyrightText: 2026 yreidev 与贡献者（见 NOTICE）
// SPDX-License-Identifier: AGPL-3.0-only
// 复制或改写本文件须保留以上声明，并按 NOTICE 的署名条款注明出处：https://github.com/linguo2625469/workbuddy2api-panel

package server

import (
	"context"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
)

// TestReplayModelOK 复测走真实流水线：返回通、模型、账号标签与回复片段，
// 并在请求记录里留下一条标「复测」的条目。
func TestReplayModelOK(t *testing.T) {
	h := NewHandler(Config{
		Pool:       testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999, Nickname: "主号"}),
		Upstream:   newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true }),
		RequestLog: reqlog.New(reqlog.Config{}),
	})
	out, err := h.ReplayModel(context.Background(), "glm-5.2")
	if err != nil {
		t.Fatalf("ReplayModel: %v", err)
	}
	if !out.OK {
		t.Fatalf("应判为通: %+v", out)
	}
	if out.Model != "glm-5.2" {
		t.Errorf("model=%q", out.Model)
	}
	if !strings.Contains(out.Reply, "你好") {
		t.Errorf("回复片段=%q 应含上游正文", out.Reply)
	}
	if out.Account == "" {
		t.Error("account 不应为空（复测要能看出走的哪个号）")
	}
	if out.DurationMs <= 0 {
		t.Errorf("duration_ms=%d", out.DurationMs)
	}
	recent := h.cfg.RequestLog.Snapshot().Recent
	if len(recent) == 0 {
		t.Fatal("请求记录里应有这次复测")
	}
	if recent[0].APIKey != replayLabel || recent[0].Path != replayPath {
		t.Errorf("请求记录=%+v，应标复测", recent[0])
	}
}

// TestReplayModelUpstreamFailure 上游失败不算 error：结果里 OK=false，带状态与原因。
func TestReplayModelUpstreamFailure(t *testing.T) {
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: newFakeUpstream(t, func(string) (int, string, bool) { return 402, `{"error":{"code":"402","message":"no credit"}}`, false }),
	})
	out, err := h.ReplayModel(context.Background(), "glm-5.2")
	if err != nil {
		t.Fatalf("上游失败应写在结果里，不应返回 error: %v", err)
	}
	if out.OK {
		t.Fatalf("应判为不通: %+v", out)
	}
	if out.Status == 0 {
		t.Error("status 不应为 0")
	}
}

// TestReplayModelNoModel 空模型名直接报错。
func TestReplayModelNoModel(t *testing.T) {
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true }),
	})
	if _, err := h.ReplayModel(context.Background(), "  "); err == nil {
		t.Fatal("模型名为空应报错")
	}
}
