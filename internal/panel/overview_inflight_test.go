// SPDX-FileCopyrightText: 2026 yreidev 与贡献者（见 NOTICE）
// SPDX-License-Identifier: AGPL-3.0-only
// 复制或改写本文件须保留以上声明，并按 NOTICE 的署名条款注明出处：https://github.com/linguo2625469/workbuddy2api-panel

// overview_inflight_test.go 面板「在途/上限」的接线测试。
//
// 背景：账号表的在途列原来只有分子，运维看不出「这个号还剩几个名额」。5 个号跑
// 3 个 agent 时，判断余量必须靠分母，否则只能去翻 config.json 手动核对。
// 本测试钉住 overview 的 accounts[] 里 in_flight / in_flight_limit 两个字段——
// 后端少透一个，前端就退化成裸数字（分母显示为空或 0，被误读成已满）。
package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

func TestOverviewExposesInFlightWithLimit(t *testing.T) {
	pl := pool.New("")
	pl.SetMaxInFlight(3) // 与生产 config.json 的 pool.max_in_flight 一致
	pl.Add(&auth.Auth{UID: "u1", Nickname: "n1"})
	pl.Add(&auth.Auth{UID: "u2", Nickname: "n2"})

	// u1 占了 2 个在途名额（模拟该号正被两个请求使用），u2 空闲。
	for i := 0; i < 2; i++ {
		if !pl.Acquire("u1") {
			t.Fatalf("第 %d 个在途名额应可占用", i+1)
		}
	}

	p := New(Config{Version: "test", Pool: pl})
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/panel/api/overview", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("overview status=%d body=%s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Accounts []struct {
			UID           string `json:"uid"`
			InFlight      int    `json:"in_flight"`
			InFlightLimit int    `json:"in_flight_limit"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode overview: %v", err)
	}
	if len(resp.Accounts) != 2 {
		t.Fatalf("accounts=%d want 2", len(resp.Accounts))
	}

	// List 按 UID 排序 → u1 在前。
	got := resp.Accounts[0]
	if got.UID != "u1" {
		t.Fatalf("首个账号=%s want u1（List 应按 UID 排序）", got.UID)
	}
	if got.InFlight != 2 {
		t.Errorf("u1 in_flight=%d want 2（分子：当前在途数）", got.InFlight)
	}
	// 分母必须等于配置上限：前端靠它渲染「2/3」并判断是否已满。
	if got.InFlightLimit != 3 {
		t.Errorf("u1 in_flight_limit=%d want 3（分母缺失则前端只能显示裸数字）", got.InFlightLimit)
	}
	if resp.Accounts[1].InFlight != 0 {
		t.Errorf("u2 in_flight=%d want 0（在途是账号维度，不应串到别的号）", resp.Accounts[1].InFlight)
	}
	if resp.Accounts[1].InFlightLimit != 3 {
		t.Errorf("u2 in_flight_limit=%d want 3（上限按配置全账号生效）", resp.Accounts[1].InFlightLimit)
	}
}

// TestOverviewInFlightLimitZeroMeansUnlimited max_in_flight=0（不限）时，分母必须是
// 0 而不是被省略——前端据此显示「x/不限」；若字段被 omitempty 吃掉，前端会当成
// 「上限 0 = 已满」或显示 undefined。
func TestOverviewInFlightLimitZeroMeansUnlimited(t *testing.T) {
	pl := pool.New("")
	pl.SetMaxInFlight(0) // 不限
	pl.Add(&auth.Auth{UID: "u1", Nickname: "n1"})

	p := New(Config{Version: "test", Pool: pl})
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/panel/api/overview", nil))

	var raw struct {
		Accounts []map[string]any `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode overview: %v", err)
	}
	if len(raw.Accounts) != 1 {
		t.Fatalf("accounts=%d want 1", len(raw.Accounts))
	}
	// 用 map 读：确认键**存在**（omitempty 会让它整个消失）。
	v, ok := raw.Accounts[0]["in_flight_limit"]
	if !ok {
		t.Fatal("in_flight_limit 字段被省略；前端会把 0 误读成「已满」或显示 undefined")
	}
	if f, _ := v.(float64); f != 0 {
		t.Errorf("in_flight_limit=%v want 0（0 = 不限）", v)
	}
}
