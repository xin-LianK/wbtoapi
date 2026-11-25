// SPDX-FileCopyrightText: 2026 yreidev 与贡献者（见 NOTICE）
// SPDX-License-Identifier: AGPL-3.0-only
// 复制或改写本文件须保留以上声明，并按 NOTICE 的署名条款注明出处：https://github.com/linguo2625469/workbuddy2api-panel

package upstream

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
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

// TestInjectHarnessMetadata harnessMetadata / initiator 按 CLI 2.176.0 补：首步不带 previous，
// 后续步带 previous，extra_vars 其他键保留，客户端带了的字段不覆盖，末尾不是 user 时不写 initiator。
func TestInjectHarnessMetadata(t *testing.T) {
	decode := func(b []byte) map[string]any {
		t.Helper()
		var obj map[string]any
		if err := json.Unmarshal(b, &obj); err != nil {
			t.Fatalf("unmarshal %s: %v", b, err)
		}
		return obj
	}
	extra := func(obj map[string]any) map[string]any {
		t.Helper()
		ev, _ := obj["extra_vars"].(map[string]any)
		return ev
	}
	harness := func(t *testing.T, obj map[string]any) map[string]any {
		t.Helper()
		raw, _ := extra(obj)["harnessMetadata"].(string)
		if raw == "" {
			return nil
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			t.Fatalf("harnessMetadata 不是 JSON 字符串: %q", raw)
		}
		return m
	}

	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	obj := decode(InjectOfficialBodyFieldsWithOpts([]byte(body), CacheRetentionPolicy{}, OfficialBodyOpts{}))
	meta := harness(t, obj)
	if meta == nil {
		t.Fatalf("应补 harnessMetadata: %v", obj)
	}
	if meta["schemaVersion"] != float64(1) || meta["harnessVersion"] != defaultCliVersionCN || meta["stepIndex"] != float64(0) {
		t.Errorf("首步字段不对: %v", meta)
	}
	if _, ok := meta["previous"]; ok {
		t.Errorf("首步不该带 previous: %v", meta)
	}
	if agent, _ := meta["agent"].(map[string]any); agent["depth"] != float64(0) {
		t.Errorf("agent.depth 应为 0: %v", meta)
	}
	if extra(obj)["initiator"] != "user" {
		t.Errorf("末尾是 user 应补 initiator=user: %v", obj)
	}

	// 后续步：stepIndex 与 previous 同源；CLI 版本可指定（国际版）。
	obj = decode(InjectOfficialBodyFieldsWithOpts([]byte(body), CacheRetentionPolicy{}, OfficialBodyOpts{
		CliVersion:  "9.9.9",
		HarnessStep: &session.HarnessStep{Index: 2, PreviousMessageID: "prev-id"},
	}))
	meta = harness(t, obj)
	if meta["stepIndex"] != float64(2) || meta["harnessVersion"] != "9.9.9" {
		t.Errorf("后续步字段不对: %v", meta)
	}
	if prev, _ := meta["previous"].(map[string]any); prev["conversationMessageId"] != "prev-id" {
		t.Errorf("previous 应指向上一步消息 id: %v", meta)
	}

	// 客户端带了 extra_vars 的其他键：保留；已带 harnessMetadata / initiator：不覆盖。
	in := `{"model":"m","messages":[{"role":"user","content":"hi"}],"extra_vars":{"a":1,"harnessMetadata":"{}","initiator":"tool"}}`
	obj = decode(InjectOfficialBodyFieldsWithOpts([]byte(in), CacheRetentionPolicy{}, OfficialBodyOpts{}))
	if extra(obj)["a"] != float64(1) || extra(obj)["harnessMetadata"] != "{}" || extra(obj)["initiator"] != "tool" {
		t.Errorf("客户端已带的字段应原样保留: %v", obj)
	}

	// 末尾是 tool（工具回传）：不写 initiator，但仍补 harnessMetadata。
	in = `{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"tool","content":"r"}]}`
	obj = decode(InjectOfficialBodyFieldsWithOpts([]byte(in), CacheRetentionPolicy{}, OfficialBodyOpts{}))
	if _, ok := extra(obj)["initiator"]; ok {
		t.Errorf("末尾不是 user 不该写 initiator: %v", obj)
	}
	if harness(t, obj) == nil {
		t.Errorf("仍应补 harnessMetadata: %v", obj)
	}
}

// TestInjectHarnessMetadataSkipsNonChat 没有 messages 数组的 body（模型目录之类的非对话请求）不补。
func TestInjectHarnessMetadataSkipsNonChat(t *testing.T) {
	in := `{"model":"m"}`
	if got := string(InjectOfficialBodyFields([]byte(in), CacheRetentionPolicy{})); got != in {
		t.Fatalf("没有 messages 时不应改字节: %s", got)
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
