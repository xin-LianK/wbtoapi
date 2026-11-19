// SPDX-FileCopyrightText: 2026 yreidev 与贡献者（见 NOTICE）
// SPDX-License-Identifier: AGPL-3.0-only
// 复制或改写本文件须保留以上声明，并按 NOTICE 的署名条款注明出处：https://github.com/linguo2625469/workbuddy2api-panel

package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"

	"github.com/linguo2625469/workbuddy2api-panel/internal/apikeys"
)

// TestAuthFailRateLimited 同一来源连续猜错密钥：前 10 次 401（最后一次触发封禁），
// 之后直接 429 并带 Retry-After；别的来源不受影响；正确密钥在封禁期内也进不来。
func TestAuthFailRateLimited(t *testing.T) {
	h := NewHandler(Config{APIKey: "sk-good"})
	reached := false
	h.mux.HandleFunc("POST /v1/probe", h.withKeyAuth(sdktranslator.FormatOpenAI, false, func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))

	call := func(ip, key string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/v1/probe", nil)
		r.RemoteAddr = ip
		if key != "" {
			r.Header.Set("Authorization", "Bearer "+key)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}

	const badIP = "203.0.113.5:50000"
	const otherIP = "203.0.113.6:50000"

	// 正确密钥先走一次：200，且不该被计入失败。
	if rec := call(badIP, "sk-good"); rec.Code != http.StatusOK || !reached {
		t.Fatalf("正确密钥应放行: code=%d body=%s", rec.Code, rec.Body)
	}

	for i := 1; i <= 10; i++ {
		if rec := call(badIP, "sk-wrong"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("第 %d 次猜错应 401: code=%d body=%s", i, rec.Code, rec.Body)
		}
	}
	rec := call(badIP, "sk-wrong")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("超过阈值应 429: code=%d body=%s", rec.Code, rec.Body)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("429 应带 Retry-After")
	}
	// 封禁期内拿着正确密钥也进不来：闸门在鉴权之前。
	reached = false
	if rec := call(badIP, "sk-good"); rec.Code != http.StatusTooManyRequests || reached {
		t.Fatalf("封禁期内正确密钥也应被拦: code=%d", rec.Code)
	}

	// 别的来源照旧：猜错是 401，不是 429。
	if rec := call(otherIP, "sk-wrong"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("别的来源不该被连坐: code=%d", rec.Code)
	}
}

// TestAuthSuccessResetsFailures 猜错几次后带上正确密钥，计数清零：
// 正常用户偶尔敲错不会在窗口里攒到被封。
func TestAuthSuccessResetsFailures(t *testing.T) {
	h := NewHandler(Config{APIKey: "sk-good"})
	h.mux.HandleFunc("POST /v1/probe", h.withKeyAuth(sdktranslator.FormatOpenAI, false, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	call := func(key string) int {
		r := httptest.NewRequest("POST", "/v1/probe", nil)
		r.RemoteAddr = "198.51.100.9:50000"
		r.Header.Set("Authorization", "Bearer "+key)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec.Code
	}
	for i := 0; i < 9; i++ {
		if code := call("sk-wrong"); code != http.StatusUnauthorized {
			t.Fatalf("第 %d 次应 401: %d", i+1, code)
		}
	}
	if code := call("sk-good"); code != http.StatusOK {
		t.Fatalf("正确密钥应放行: %d", code)
	}
	// 计数已清：再错 9 次仍是 401，第 10 次才触发。
	for i := 0; i < 9; i++ {
		if code := call("sk-wrong"); code != http.StatusUnauthorized {
			t.Fatalf("重置后再错 %d 次应仍是 401: %d", i+1, code)
		}
	}
}

// TestAuthDisabledKeyNotCounted 停用/过期的密钥不算「猜错」：它们是已知密钥的状态问题，
// 客户端重试会反复打到同一句说明，不该把整个来源拦掉、把真正的错误盖住。
func TestAuthDisabledKeyNotCounted(t *testing.T) {
	store, err := apikeys.Open("")
	if err != nil {
		t.Fatal(err)
	}
	k, err := store.Create(apikeys.Input{Name: "停用的"}, time.Now()) // Enabled 缺省 false = 已停用
	if err != nil {
		t.Fatal(err)
	}

	h := NewHandler(Config{APIKey: "sk-good", Keys: store})
	h.mux.HandleFunc("POST /v1/probe", h.withKeyAuth(sdktranslator.FormatOpenAI, false, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	for i := 0; i < 12; i++ {
		r := httptest.NewRequest("POST", "/v1/probe", nil)
		r.RemoteAddr = "198.51.100.10:50000"
		r.Header.Set("Authorization", "Bearer "+k.Secret)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("第 %d 次应 401（停用）: code=%d body=%s", i+1, rec.Code, rec.Body)
		}
	}
}
