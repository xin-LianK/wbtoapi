// SPDX-FileCopyrightText: 2026 yreidev 与贡献者（见 NOTICE）
// SPDX-License-Identifier: AGPL-3.0-only
// 复制或改写本文件须保留以上声明，并按 NOTICE 的署名条款注明出处：https://github.com/linguo2625469/workbuddy2api-panel

package server

import (
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestShouldRetryChat401 401 刷新重发的判定边界：
//   - 401 且非 12153 → 值得先刷新再原位重发一次（APISIX 提前吊销 accessToken 的典型形态）；
//   - 12153 会话终态、有 refreshToken 才有得刷，否则只能禁用换号；
//   - 403/429 与号本身无关的刷新救不回来。
func TestShouldRetryChat401(t *testing.T) {
	acct := &auth.Auth{UID: "u-1", RefreshToken: "rt-1"}
	noRT := &auth.Auth{UID: "u-2"}
	apisix401 := `<html><head><title>401 Authorization Required</title></head><body>openresty</body></html>`

	cases := []struct {
		name   string
		status int
		body   string
		err    error
		acct   *auth.Auth
		want   bool
	}{
		{"401 HTML 可刷新", 401, apisix401, nil, acct, true},
		{"401 空体也可刷新", 401, "", nil, acct, true},
		{"401 + 12153 会话终态不刷新", 401, `{"code":12153,"msg":"invalid refresh token"}`, nil, acct, false},
		{"无 refreshToken 不刷新", 401, apisix401, nil, noRT, false},
		{"403 不刷新", 403, `{"msg":"forbidden"}`, nil, acct, false},
		{"429 不刷新", 429, `{"code":6020}`, nil, acct, false},
		{"已分类的 401 错误", 0, "", &upstream.Error{Kind: upstream.ErrWafBlock, Status: 401}, acct, true},
		{"已分类的 12153 错误", 0, "", &upstream.Error{Kind: upstream.ErrSessionDead, Status: 401}, acct, false},
	}
	for _, tc := range cases {
		if got := shouldRetryChat401(tc.status, []byte(tc.body), tc.err, tc.acct); got != tc.want {
			t.Errorf("%s: shouldRetryChat401 = %v, want %v", tc.name, got, tc.want)
		}
	}
}
