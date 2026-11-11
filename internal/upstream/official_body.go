// SPDX-FileCopyrightText: 2026 yreidev 与贡献者（见 NOTICE）
// SPDX-License-Identifier: AGPL-3.0-only
// 复制或改写本文件须保留以上声明，并按 NOTICE 的署名条款注明出处：https://github.com/linguo2625469/workbuddy2api-panel

// official_body.go 出站 chat body 补官方 CLI 会补的字段（WorkBuddy 桌面端安装包内置的
// @tencent-ai/codebuddy-code，dist/codebuddy-lite-wb.mjs 的 injectReasoningSummaryIfNeeded 与
// resolveCacheRetention；10-20 读 5.8.3 / 5.8.4 内置的 2.161.0，两域现在的 2.165.0 也一样）。
package upstream

import (
	"encoding/json"
	"strings"
)

// cache_retention 的两个取值（官方 CLI 的 resolveCacheRetention）。
const (
	// CacheRetentionLong 1 小时提示缓存：多轮长会话命中的更多，但在部分模型上写入更贵。
	CacheRetentionLong = "long"
	// CacheRetentionShort 5 分钟提示缓存：官方在被 CODEBUDDY_DISABLE_PROMPT_CACHING_1H 关掉 1 小时
	// 缓存时用的值。
	CacheRetentionShort = "short"
	// CacheRetentionOff 不补这个字段（网关默认）。
	CacheRetentionOff = ""
)

// CacheRetentionPolicy 出站对话请求体补 cache_retention 的规则。
//
// 官方口径：模型请求处理器发现 body 里没有 cache_retention 就补上，默认 "long"，只有设了
// CODEBUDDY_DISABLE_PROMPT_CACHING_1H 才用 "short"（npm 上 2.169.0 起；网关 UA 报的 2.165.0 只补
// reasoning_summary，不补这个）。所以「补」本身是官方行为，只是要按 CLI 版本走，且 1 小时缓存
// 在部分模型上写入更贵（实测只覆盖过 deepseek），因此默认关、按模型白名单放行。
type CacheRetentionPolicy struct {
	// Retention 空 = 不补；CacheRetentionLong / CacheRetentionShort。
	Retention string
	// Models 只给这些模型补，按前缀匹配（不区分大小写），空 = 所有模型都补。
	Models []string
}

// AppliesTo 报告这个模型要不要补 cache_retention。
func (p CacheRetentionPolicy) AppliesTo(model string) bool {
	if p.Retention == "" {
		return false
	}
	if len(p.Models) == 0 {
		return true
	}
	model = strings.ToLower(strings.TrimSpace(model))
	for _, m := range p.Models {
		if m != "" && strings.HasPrefix(model, m) {
			return true
		}
	}
	return false
}

// ParseCacheRetentionPolicy 解析配置里的 cache_retention（""/off/long/short）与
// cache_retention_models（逗号分隔的模型名前缀）。返回 error 时调用方应按「不补」处理。
func ParseCacheRetentionPolicy(retention, models string) (CacheRetentionPolicy, error) {
	p := CacheRetentionPolicy{}
	switch v := strings.ToLower(strings.TrimSpace(retention)); v {
	case "", "off":
		return p, nil
	case CacheRetentionLong:
		p.Retention = CacheRetentionLong
	case CacheRetentionShort:
		p.Retention = CacheRetentionShort
	default:
		return CacheRetentionPolicy{}, &UnknownCacheRetentionError{Value: retention}
	}
	for _, m := range strings.Split(models, ",") {
		if m = strings.ToLower(strings.TrimSpace(m)); m != "" {
			p.Models = append(p.Models, m)
		}
	}
	return p, nil
}

// UnknownCacheRetentionError cache_retention 配置了不认识的值。
type UnknownCacheRetentionError struct{ Value string }

func (e *UnknownCacheRetentionError) Error() string {
	return "upstream.cache_retention 只能是 long / short / off（留空），现在是「" + e.Value + "」"
}

// InjectOfficialBodyFields 补官方 CLI 发请求前会补的字段，客户端自己带了就原样保留：
//   - reasoning_summary：带了思考档位（reasoning_effort 或 reasoning.effort 是字符串）时补 "auto"。
//   - cache_retention：策略开了、且这个模型在放行范围里时补 long / short。
//
// body 不可解析、或者两个字段都已经齐了时原样返回（不改字节，前缀缓存要靠这个）。
func InjectOfficialBodyFields(body []byte, cr CacheRetentionPolicy) []byte {
	if len(body) == 0 {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	changed := false
	if _, ok := obj["reasoning_summary"]; !ok && hasReasoningEffort(obj) {
		obj["reasoning_summary"] = "auto"
		changed = true
	}
	if _, ok := obj["cache_retention"]; !ok {
		model, _ := obj["model"].(string)
		if cr.AppliesTo(model) {
			obj["cache_retention"] = cr.Retention
			changed = true
		}
	}
	if !changed {
		return body
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// hasReasoningEffort 报告 body 是否带了思考档位：官方判定是 reasoning_effort 或 reasoning.effort 为字符串。
func hasReasoningEffort(obj map[string]any) bool {
	if _, ok := obj["reasoning_effort"].(string); ok {
		return true
	}
	r, _ := obj["reasoning"].(map[string]any)
	_, ok := r["effort"].(string)
	return ok
}
