// SPDX-FileCopyrightText: 2026 yreidev 与贡献者（见 NOTICE）
// SPDX-License-Identifier: AGPL-3.0-only
// 复制或改写本文件须保留以上声明，并按 NOTICE 的署名条款注明出处：https://github.com/linguo2625469/workbuddy2api-panel

package panel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestRequestReplay 复测接口：把模型名交给注入的实现，结果原样回给前端。
func TestRequestReplay(t *testing.T) {
	var gotModel string
	p, _ := newPanelFor(t, Config{
		Replay: func(_ context.Context, model string) (ReplayResult, error) {
			gotModel = model
			return ReplayResult{OK: true, Status: 200, Model: model, Account: "主号(u1)", DurationMs: 12}, nil
		},
	})
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("POST", "/panel/api/requests/replay", strings.NewReader(`{"model":" glm-5.2 "}`)))
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if gotModel != "glm-5.2" {
		t.Errorf("传入模型=%q（应去空白）", gotModel)
	}
	var out struct {
		Result ReplayResult `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if !out.Result.OK || out.Result.Account != "主号(u1)" || out.Result.DurationMs != 12 {
		t.Fatalf("结果=%+v", out.Result)
	}
}

// TestRequestReplayGuards 缺模型名 400、未注入 501。
func TestRequestReplayGuards(t *testing.T) {
	p, _ := newPanelFor(t, Config{Replay: func(context.Context, string) (ReplayResult, error) { return ReplayResult{}, nil }})
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("POST", "/panel/api/requests/replay", strings.NewReader(`{"model":"  "}`)))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("空模型名 status=%d want 400", rec.Code)
	}

	off, _ := newPanelFor(t, Config{})
	rec = httptest.NewRecorder()
	off.ServeHTTP(rec, httptest.NewRequest("POST", "/panel/api/requests/replay", strings.NewReader(`{"model":"m"}`)))
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("未注入 status=%d want 501", rec.Code)
	}
}
