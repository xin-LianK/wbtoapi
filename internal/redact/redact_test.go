// SPDX-FileCopyrightText: 2026 yreidev 与贡献者（见 NOTICE）
// SPDX-License-Identifier: AGPL-3.0-only
// 复制或改写本文件须保留以上声明，并按 NOTICE 的署名条款注明出处：https://github.com/linguo2625469/workbuddy2api-panel

package redact

import (
	"strings"
	"testing"
)

func TestTextMasksCredentials(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string // 必须出现在结果里的片段（脱敏后的样子）
		deny []string // 不许出现在结果里的原文
	}{
		{
			name: "bearer 头",
			in:   "GET /v1/chat Authorization: Bearer abcdefghijklmnopqrstuvwxyz012345",
			want: []string{"Bearer abcd***2345"},
			deny: []string{"abcdefghijklmnopqrstuvwxyz012345"},
		},
		{
			name: "sk 密钥",
			in:   "upstream key sk-abcdefghijklmnopqrstuvwxyz rejected",
			want: []string{"sk-a***wxyz"},
			deny: []string{"sk-abcdefghijklmnopqrstuvwxyz"},
		},
		{
			name: "JWT",
			in:   "cookie jwt=eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dBjftJeZ4CVPmB92K27uhbUJU1p1r_wW1gFWFOEjXk",
			want: []string{"eyJh***"},
			deny: []string{"dBjftJeZ4CVPmB92K27uhbUJU1p1r_wW1gFWFOEjXk"},
		},
		{
			name: "query 参数",
			in:   "GET /callback?access_token=abcdefghijklmnop&x=1",
			want: []string{"access_token=ab***op"},
			deny: []string{"abcdefghijklmnop&"},
		},
		{
			name: "JSON 字段",
			in:   `{"refresh_token":"abcdefghijklmnop","uid":"u_1"}`,
			want: []string{`"refresh_token":"ab***op"`},
			deny: []string{"abcdefghijklmnop"},
		},
		{
			name: "代理 URL 带密码",
			in:   "proxy socks5://alice:s3cretpass@10.0.0.2:1080 连不上",
			want: []string{"socks5://alice:***@10.0.0.2:1080"},
			deny: []string{"s3cretpass"},
		},
		{
			name: "Cookie 头",
			in:   "Cookie: uid=abc123; access_token=abcdefghijklmn; theme=dark",
			want: []string{"uid=***; access_token=***; theme=***"},
			deny: []string{"abcdefghijklmn"},
		},
		{
			name: "邮箱",
			in:   "账号 zhang.san@example.com 登录失败",
			want: []string{"zh***@example.com"},
			deny: []string{"zhang.san@"},
		},
		{
			name: "手机号",
			in:   "验证码已发往 13800138000",
			want: []string{"138****8000"},
			deny: []string{"13800138000"},
		},
		{
			name: "私钥块",
			in:   "-----BEGIN RSA PRIVATE KEY-----\nMIIEow\n-----END RSA PRIVATE KEY-----",
			want: []string{"[私钥已脱敏]"},
			deny: []string{"MIIEow"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Text(c.in)
			for _, w := range c.want {
				if !strings.Contains(got, w) {
					t.Errorf("结果应包含 %q，实际 %q", w, got)
				}
			}
			for _, d := range c.deny {
				if strings.Contains(got, d) {
					t.Errorf("结果不应包含 %q，实际 %q", d, got)
				}
			}
		})
	}
}

func TestTextLeavesPlainTextAlone(t *testing.T) {
	for _, s := range []string{
		"checkin u_abc: 签到成功 +100",
		"chat model=glm-5.2 status=200 dur=1234ms",
		"upstream 14017 trial not activated",
		"",
	} {
		if got := Text(s); got != s {
			t.Errorf("普通日志不该被改写: %q -> %q", s, got)
		}
	}
}

func TestMaskKeepsEndsIdentifiable(t *testing.T) {
	if got, want := Mask("abcdefghijklmnop"), "ab***op"; got != want {
		t.Errorf("Mask = %q, want %q", got, want)
	}
	if got, want := Mask("abcdefghij"), "ab***ij"; got != want {
		t.Errorf("短值 Mask = %q, want %q", got, want)
	}
	if got, want := Mask("short"), "***"; got != want {
		t.Errorf("超短值 Mask = %q, want %q", got, want)
	}
}
