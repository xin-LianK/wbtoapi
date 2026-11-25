// SPDX-FileCopyrightText: 2026 yreidev 与贡献者（见 NOTICE）
// SPDX-License-Identifier: AGPL-3.0-only
// 复制或改写本文件须保留以上声明，并按 NOTICE 的署名条款注明出处：https://github.com/linguo2625469/workbuddy2api-panel

// official_body.go 出站 chat body 补官方 CLI 会补的字段（WorkBuddy 桌面端安装包内置的
// @tencent-ai/codebuddy-code，dist/codebuddy-lite-wb.mjs 的 injectReasoningSummaryIfNeeded、
// resolveCacheRetention、buildHarnessMetadata 与 MessageInitiatorRequestProcessor；10-20 读
// 5.8.3 / 5.8.4 内置的 2.161.0，10-31 读 5.9.x 的 2.165.0，11-16 读 5.11.3 内置的 2.176.0，
// 11-24 读两域 5.11.3 / 5.11.4 仍为 2.176.0）。
package upstream

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
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
// CODEBUDDY_DISABLE_PROMPT_CACHING_1H 才用 "short"（npm 上 2.169.0 起；桌面端内置的 2.176.0 现在
// 也会补，2.165.0 只补 reasoning_summary）。也就是 UA 报的这个版本本身就会补这个字段，网关默认仍
// 不补是刻意的偏差：1 小时缓存在部分模型上写入更贵（实测只覆盖过 deepseek），要补就按模型白名单放行。
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

// OfficialBodyOpts 控制出站请求体按官方 CLI 2.176.0 补齐的字段参数。
type OfficialBodyOpts struct {
	// CliVersion 内置 CLI 版本（写入 extra_vars.harnessMetadata.harnessVersion）；空则取国内版默认。
	CliVersion string
	// HarnessStep 当前对话轮内的步骤位置（stepIndex 与上一步 X-Conversation-Message-ID）；
	// nil 时按首步（stepIndex=0，无 previous）处理。见 session.HarnessTracker。
	HarnessStep *session.HarnessStep
}

type harnessPrevious struct {
	ConversationMessageID string `json:"conversationMessageId"`
}

type harnessAgent struct {
	Depth int `json:"depth"`
}

type harnessMetadataPayload struct {
	SchemaVersion  int              `json:"schemaVersion"`
	HarnessVersion string           `json:"harnessVersion"`
	StepIndex      int              `json:"stepIndex"`
	Previous       *harnessPrevious `json:"previous,omitempty"`
	Agent          harnessAgent     `json:"agent"`
}

// InjectOfficialBodyFields 补官方 CLI 发请求前会补的字段，客户端自己带了就原样保留：
//   - reasoning_summary：带了思考档位（reasoning_effort 或 reasoning.effort 是字符串）时补 "auto"。
//   - cache_retention：策略开了、且这个模型在放行范围里时补 long / short。
//   - extra_vars.harnessMetadata / extra_vars.initiator：按官方 CLI 2.176.0 补齐（见 InjectOfficialBodyFieldsWithOpts），
//     首步（stepIndex=0、无 previous）。
//
// body 不可解析、或者字段都已经齐了时原样返回（不改字节，前缀缓存要靠这个）。
func InjectOfficialBodyFields(body []byte, cr CacheRetentionPolicy) []byte {
	return InjectOfficialBodyFieldsWithOpts(body, cr, OfficialBodyOpts{})
}

// InjectOfficialBodyFieldsWithOpts 同 InjectOfficialBodyFields，额外按官方 CLI 2.176.0 的
// buildHarnessMetadata 与 MessageInitiatorRequestProcessor 补齐对话请求体字段，客户端自己带了的原样保留：
//   - cache_retention：策略开了、且这个模型在放行范围里时补 long / short；
//   - reasoning_summary：带了思考档位时补 "auto"；
//   - extra_vars.harnessMetadata：带 messages 数组时补 JSON 字符串，含 schemaVersion(1)、
//     harnessVersion(CLI 版本)、stepIndex、stepIndex>0 且有上一步消息 id 时的 previous、agent.depth(0)，
//     保留客户端原有的其他 extra_vars 键；
//   - extra_vars.initiator：带 messages 数组且末尾消息为普通 role=="user" 时补 "user"，末尾为 tool 等角色时不写。
func InjectOfficialBodyFieldsWithOpts(body []byte, cr CacheRetentionPolicy, opts OfficialBodyOpts) []byte {
	if len(body) == 0 {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil || obj == nil {
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
	if msgs, ok := obj["messages"].([]any); ok {
		extra, _ := obj["extra_vars"].(map[string]any)
		extraChanged := false

		if extra == nil || !hasKey(extra, "harnessMetadata") {
			cliVer := opts.CliVersion
			if cliVer == "" {
				cliVer = defaultCliVersionCN
			}
			var step session.HarnessStep
			if opts.HarnessStep != nil {
				step = *opts.HarnessStep
			}
			meta := harnessMetadataPayload{
				SchemaVersion:  1,
				HarnessVersion: cliVer,
				StepIndex:      step.Index,
				Agent:          harnessAgent{Depth: 0},
			}
			if step.Index > 0 && step.PreviousMessageID != "" {
				meta.Previous = &harnessPrevious{ConversationMessageID: step.PreviousMessageID}
			}
			if rawMeta, err := json.Marshal(meta); err == nil {
				if extra == nil {
					extra = map[string]any{}
				}
				extra["harnessMetadata"] = string(rawMeta)
				extraChanged = true
			}
		}

		if (extra == nil || !hasKey(extra, "initiator")) && trailingMessageIsUser(msgs) {
			if extra == nil {
				extra = map[string]any{}
			}
			extra["initiator"] = "user"
			extraChanged = true
		}

		if extraChanged {
			obj["extra_vars"] = extra
			changed = true
		}
	}
	if !changed {
		return body
	}
	out, err := marshalJSONNoEscape(obj)
	if err != nil {
		return body
	}
	return out
}

func hasKey(m map[string]any, k string) bool {
	_, ok := m[k]
	return ok
}

// trailingMessageIsUser 报告 messages 末尾消息是否为普通 user 消息（对应官方
// MessageInitiatorRequestProcessor 的 resolveTrailingMessageInitiator：末尾为 tool 等非 user 角色时不写 initiator）。
func trailingMessageIsUser(msgs []any) bool {
	if len(msgs) == 0 {
		return false
	}
	last, ok := msgs[len(msgs)-1].(map[string]any)
	if !ok {
		return false
	}
	role, _ := last["role"].(string)
	return role == "user"
}

// marshalJSONNoEscape 同 JS JSON.stringify，不把 <、>、& 转义为 \u003c 等形式。
func marshalJSONNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
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
