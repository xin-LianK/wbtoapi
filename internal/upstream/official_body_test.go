// SPDX-FileCopyrightText: 2026 yreidev 与贡献者（见 NOTICE）
// SPDX-License-Identifier: AGPL-3.0-only
// 复制或改写本文件须保留以上声明，并按 NOTICE 的署名条款注明出处：https://github.com/linguo2625469/workbuddy2api-panel

package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestInjectOfficialBodyFields 带了思考档位才补 reasoning_summary="auto"；cache_retention 只在策略开了
// 且模型放行时补；客户端带了任何值都原样保留；空 body / 坏 body 原样返回。
func TestInjectOfficialBodyFields(t *testing.T) {
	decode := func(b []byte) map[string]any {
		t.Helper()
		var obj map[string]any
		if err := json.Unmarshal(b, &obj); err != nil {
			t.Fatalf("unmarshal %s: %v", b, err)
		}
		return obj
	}
	off := CacheRetentionPolicy{}
	long := CacheRetentionPolicy{Retention: CacheRetentionLong}
	for _, tc := range []struct {
		name, in       string
		policy         CacheRetentionPolicy
		cache, summary any
	}{
		{"都没带、没有思考档位", `{"model":"m"}`, off, nil, nil},
		{"reasoning_effort", `{"model":"m","reasoning_effort":"high"}`, off, nil, "auto"},
		{"reasoning.effort", `{"model":"m","reasoning":{"effort":"low"}}`, off, nil, "auto"},
		{"reasoning 不是对象", `{"model":"m","reasoning":true}`, off, nil, nil},
		{"客户端都带了", `{"model":"m","reasoning_effort":"high","cache_retention":"short","reasoning_summary":"none"}`, off, "short", "none"},
		{"策略开了就补", `{"model":"m"}`, long, "long", nil},
		{"客户端带的不覆盖", `{"model":"m","cache_retention":"short"}`, long, "short", nil},
	} {
		obj := decode(InjectOfficialBodyFields([]byte(tc.in), tc.policy))
		if obj["cache_retention"] != tc.cache || obj["reasoning_summary"] != tc.summary || obj["model"] != "m" {
			t.Errorf("%s: cache_retention=%v reasoning_summary=%v（%v）", tc.name, obj["cache_retention"], obj["reasoning_summary"], obj)
		}
	}
	// 字段都齐了就不改字节：前缀缓存要靠这个。
	keep := `{"model":"m","cache_retention":"short","reasoning_effort":"high","reasoning_summary":"none"}`
	if got := string(InjectOfficialBodyFields([]byte(keep), long)); got != keep {
		t.Fatalf("不需要补时应原样返回: %s", got)
	}
	for _, in := range []string{"", "not json"} {
		if got := string(InjectOfficialBodyFields([]byte(in), long)); got != in {
			t.Fatalf("%q 应原样返回: %q", in, got)
		}
	}
}

// TestCacheRetentionPolicy 配置解析与模型放行：空/off 不补，long/short 认，别的值报错；
// 白名单按前缀、不区分大小写，空白名单 = 所有模型。
func TestCacheRetentionPolicy(t *testing.T) {
	for _, tc := range []struct {
		retention, models string
		want              string
		err               bool
	}{
		{"", "", "", false},
		{"off", "m", "", false},
		{" OFF ", "", "", false},
		{"long", "", CacheRetentionLong, false},
		{"short", "", CacheRetentionShort, false},
		{"1h", "", "", true},
	} {
		p, err := ParseCacheRetentionPolicy(tc.retention, tc.models)
		if (err != nil) != tc.err || p.Retention != tc.want {
			t.Errorf("ParseCacheRetentionPolicy(%q, %q) = %+v %v", tc.retention, tc.models, p, err)
		}
	}
	p, err := ParseCacheRetentionPolicy("long", " DeepSeek-, glm ")
	if err != nil || len(p.Models) != 2 || p.Models[0] != "deepseek-" || p.Models[1] != "glm" {
		t.Fatalf("白名单应去空格转小写: %+v %v", p, err)
	}
	for _, tc := range []struct {
		model string
		want  bool
	}{
		{"deepseek-v4.1-flash", true},
		{"DeepSeek-V4.1", true},
		{"glm-4.6", true},
		{"kimi-k2", false},
		{"", false},
	} {
		if got := p.AppliesTo(tc.model); got != tc.want {
			t.Errorf("AppliesTo(%q) = %v，应为 %v", tc.model, got, tc.want)
		}
	}
	if !(CacheRetentionPolicy{Retention: CacheRetentionLong}).AppliesTo("anything") {
		t.Error("白名单为空时所有模型都补")
	}
	if (CacheRetentionPolicy{}).AppliesTo("anything") {
		t.Error("策略关掉时一个都不补")
	}
}

// TestPrepareBodyInjectsOfficialFields chat 出站 body 经 prepareBody 后默认不带 cache_retention，两个域
// 都一样；打开策略且模型放行才带。deepseek 模型被补了默认思考档位，也就跟着带 reasoning_summary。
func TestPrepareBodyInjectsOfficialFields(t *testing.T) {
	for _, realm := range []string{"cn", "global"} {
		out := string((&Client{}).prepareBody([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`), realm))
		if strings.Contains(out, "cache_retention") || strings.Contains(out, "reasoning_summary") {
			t.Fatalf("%s: 普通模型两个字段都不该补: %s", realm, out)
		}
		out = string((&Client{}).prepareBody([]byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`), realm))
		if !strings.Contains(out, `"reasoning_effort"`) || !strings.Contains(out, `"reasoning_summary":"auto"`) {
			t.Fatalf("%s: deepseek 补了思考档位就应带 reasoning_summary: %s", realm, out)
		}
	}
	c := &Client{}
	c.CacheRetention.Store(&CacheRetentionPolicy{Retention: CacheRetentionLong, Models: []string{"deepseek"}})
	if out := string(c.prepareBody([]byte(`{"model":"deepseek-v4.1-flash"}`), "cn")); !strings.Contains(out, `"cache_retention":"long"`) {
		t.Fatalf("放行的模型应补 long: %s", out)
	}
	if out := string(c.prepareBody([]byte(`{"model":"glm-4.6"}`), "cn")); strings.Contains(out, "cache_retention") {
		t.Fatalf("没放行的模型不该补: %s", out)
	}
}
