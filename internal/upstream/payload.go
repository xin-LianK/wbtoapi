// SPDX-FileCopyrightText: 2026 yreidev 与贡献者（见 NOTICE）
// SPDX-License-Identifier: AGPL-3.0-only
// 复制或改写本文件须保留以上声明，并按 NOTICE 的署名条款注明出处：https://github.com/linguo2625469/workbuddy2api-panel

// payload.go 改写发往上游的 chat 请求体：
//  1. 强制 stream:true（上游拒绝非流式）
//  2. tool_choice 归一化（上游该字段是 string，对象形式会 400 code=11101）
//  3. image_url 归一化（上游只认 OpenAI 对象形态，字符串会 400 code=11101）
package upstream

import (
	"encoding/json"
	"log"
	"strconv"
	"strings"
)

// PrepareBodyOpt 单 pass 改写；sanitize=false 时行为完全还原（仅强制 stream + 归一化 tool_choice）。
func PrepareBodyOpt(src []byte, sanitize bool) []byte {
	return PrepareBodyOptWithEffortsAndDefault(src, sanitize, nil, nil)
}

// PrepareBodyOptWithEfforts 在 PrepareBodyOpt 基础上按模型 supportedEfforts 降级 reasoning_effort：
// 仅当请求显式携带且模型不支持该档位时，改为 ≤请求档位的最高支持档；支持档全部高于请求档时取最低档；
// 未知模型/未知档位/未携带该字段一律透传。efforts 为 nil 表示未知（不降级）。
//
// 向后兼容封装：不传 defaultEfforts（无模型声明默认档），thinking.go 回退硬编码 high。
func PrepareBodyOptWithEfforts(src []byte, sanitize bool, efforts map[string][]string) []byte {
	return PrepareBodyOptWithEffortsAndDefault(src, sanitize, efforts, nil)
}

// PrepareBodyOptWithEffortsAndDefault 完整管线：efforts 降级 + thinking.go 按
// defaultEfforts（模型声明默认档）补档。defaultEfforts 为 nil 时与旧行为一致
// （deepseek 缺档回退硬编码 high）。
func PrepareBodyOptWithEffortsAndDefault(src []byte, sanitize bool, efforts map[string][]string, defaultEfforts map[string]string) []byte {
	if len(src) == 0 {
		return src
	}
	var obj map[string]any
	if err := json.Unmarshal(src, &obj); err != nil {
		return src
	}
	obj["stream"] = true
	// max_completion_tokens → max_tokens 翻译（吸收上游 PR #116，Closes #117）：
	// OpenAI 规范里 max_tokens 已 deprecated、max_completion_tokens 是新字段；
	// DeepSeek Harness 等新客户端只发别名。WorkBuddy 上游（CN /v2 与 global
	// /console 同源）只认 max_tokens——别名透传会被上游忽略后回落默认输出上限
	// （实测 32000），长流任务被截。
	translateMaxCompletionTokens(obj)
	clampGPTMinMaxTokens(obj)
	// stream_options 仅当 body 未显式带时补 {include_usage: true}（D7）：
	// 官方 CLI 流式必发该字段，上游据此在末帧返回 usage 用量；显式带则不覆盖。
	if _, has := obj["stream_options"]; !has {
		obj["stream_options"] = map[string]any{"include_usage": true}
	}
	normalizeToolChoice(obj)
	normalizeToolPatterns(obj)
	hardenToolSchemaKeywords(obj)
	normalizeRoles(obj)
	normalizeImageURL(obj)
	dropReasoningContentParts(obj)
	// tool 配对三步（见 tool_pairing.go）：先合并再重排再清理。所有模型一律执行（独立于
	// deepseek-only 的 sanitize 开关）。这是「让请求通过」的安全网——不完整配对的
	// tool_calls/tool 结果会让上游对之后每条消息都返 400，必须先行剔除；
	// 插在结果中间的非 tool 消息（Codex image_resize_notice）同样判配对断裂，
	// 先 repack 挪后，再 cleanup 删孤儿，两侧同口径。
	//
	// 顺序不能换：mergeAdjacentToolCalls 必须最先跑——它把「背靠背的两条
	// assistant.tool_calls」合成一条（部分 agent 客户端回放并行调用的报文形状），是上游
	// deepseek 系模型 11148 的正面修复；先合并再 repack，repack 才看得到完整的一批调用。
	if msgs, ok := obj["messages"].([]any); ok {
		msgs, _ = mergeAdjacentToolCalls(msgs)
		msgs, _ = repackToolResultBlocks(msgs)
		msgs, _ = cleanupOrphanToolCalls(msgs)
		// 无改动时两步都返回原 slice，这里回写等于零操作；任一步重排/删除
		// （哪怕后续步骤零改动）也必须落到 obj——不能只在「最后一步改动」时回写，
		// 否则 repack 单独生效的结果会被原 slice 覆盖丢失。
		obj["messages"] = msgs
	}
	// DeepSeek 思维链开关（见 thinking.go）：注入 thinking.type=enabled + 缺档补默认档。
	// 先于 normalizeReasoningEffort 执行：补入的默认档也要走既有降级管线，
	// 模型不支持默认档时自动落到 ≤ 默认档的最高支持档（不出站不合规档位）。
	modelName, _ := obj["model"].(string)
	injectThinking(obj, lookupDefaultEffort(defaultEfforts, modelName))
	normalizeReasoningEffort(obj, efforts)
	// DeepSeek 多轮一致性：assistant 消息带 reasoning 痕迹时回填 reasoning_content
	// （requiresReasoningContentOnAssistantMessages，见 thinking.go）。
	backfillReasoningContent(obj)
	if sanitize {
		if msgs, ok := obj["messages"].([]any); ok {
			sanitizeMessages(msgs)
		}
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return src
	}
	return out
}

// translateMaxCompletionTokens 把 OpenAI 别名 max_completion_tokens 翻译为上游
// 认的 max_tokens（吸收上游 PR #116）。规则：显式 max_tokens 优先（别名只删）；
// 别名非正数值（0/null/负数）不翻译（0/null 语义是「未设置」，负数是非法值，
// 翻译等于把垃圾搬进 max_tokens）；非数值别名（字符串等畸形）不翻译（原样
// 透传由上游报 11101 参数错）。两域同口径：CN /v2 与 global /console 是同一套
// API，翻译不分 realm。
func translateMaxCompletionTokens(obj map[string]any) {
	alias, has := obj["max_completion_tokens"]
	delete(obj, "max_completion_tokens") // 无论翻译与否，别名一律删（减少 body 体积与排障噪音）
	if !has {
		return
	}
	if _, explicit := obj["max_tokens"]; explicit {
		return // 显式 max_tokens 优先：别名只删不译
	}
	// json.Unmarshal 数字 → float64（整数去整后回写，避免 1.28e5 科学计数法/小数
	// 尾巴进上游 body）；其他数值类型防御性兼容（int 家族——手构造 map 的调用方）。
	switch v := alias.(type) {
	case float64:
		if v > 0 && v == float64(int64(v)) {
			obj["max_tokens"] = int64(v)
		}
	case int64:
		if v > 0 {
			obj["max_tokens"] = v
		}
	case int:
		if v > 0 {
			obj["max_tokens"] = int64(v)
		}
	}
}

// gptMinMaxTokens GPT 系上游接受的 max_tokens 下限。
const gptMinMaxTokens = 16

// clampGPTMinMaxTokens 把 GPT 系模型过小的 max_tokens 抬到下限。
//
// 背景：上游 GPT 系（实测 gpt-6-sol / gpt-6-luna / gpt-5.6-sol）对 max_tokens < 16
// 一律 400 code=11133 model_param_invalid（15 拒、16 过，同号同 body 对照）；hy4 等
// 非 GPT 模型无此限制。Claude Code 切模型时发 max_tokens 极小的探针，全号轮转同样
// 被拒 → 客户端 503，模型永远切不过去。账号与 body 其余部分无关，换号无用，只能
// 在发送前修。抬到下限只放宽输出上限、不改语义；未携带字段 / 非数值 / 已达下限一律不动。
func clampGPTMinMaxTokens(obj map[string]any) {
	model, _ := obj["model"].(string)
	if !strings.Contains(strings.ToLower(model), "gpt-") {
		return
	}
	var v int64
	switch n := obj["max_tokens"].(type) {
	case float64:
		v = int64(n)
	case int64:
		v = n
	case int:
		v = int64(n)
	default:
		return
	}
	if v < gptMinMaxTokens {
		obj["max_tokens"] = int64(gptMinMaxTokens)
		log.Printf("max_tokens clamped model=%s %d -> %d", model, v, gptMinMaxTokens)
	}
}

// effortRank 档位从低到高。
var effortRank = map[string]int{"off": 0, "minimal": 1, "low": 2, "medium": 3, "high": 4, "xhigh": 5, "max": 6}

// normalizeReasoningEffort 按模型 supportedEfforts 降级 reasoning_effort（snake/camel 双字段兼容）。
//   - 请求档位模型支持 → 原样透传
//   - 请求档位不支持 → 改为 ≤请求档位的最高支持档（降级）
//   - 支持档全部高于请求档 → 取最低支持档（偏离最小）
//   - 未知模型/未知档位/未携带字段/模型未缓存 → 一律透传
func normalizeReasoningEffort(obj map[string]any, efforts map[string][]string) {
	if len(efforts) == 0 {
		return
	}
	model, _ := obj["model"].(string)
	if model == "" {
		return
	}
	supported, ok := efforts[model]
	if !ok || len(supported) == 0 {
		return
	}
	key := ""
	if _, present := obj["reasoning_effort"]; present {
		key = "reasoning_effort"
	} else if _, present := obj["reasoningEffort"]; present {
		key = "reasoningEffort"
	} else {
		return
	}
	reqStr, ok := obj[key].(string)
	if !ok {
		return
	}
	reqStr = strings.TrimSpace(strings.ToLower(reqStr))
	reqIdx, known := effortRank[reqStr]
	if !known {
		return
	}
	// 在 ≤请求档位的支持档里选最高档；命中且与请求不同才改写。
	best, bestIdx := "", -1
	for _, s := range supported {
		idx, k := effortRank[strings.TrimSpace(strings.ToLower(s))]
		if k && idx <= reqIdx && idx > bestIdx {
			best, bestIdx = s, idx
		}
	}
	if best != "" {
		if !strings.EqualFold(best, reqStr) {
			obj[key] = best
			log.Printf("reasoning_effort downgraded model=%s %s -> %s", model, reqStr, best)
		}
		return
	}
	// 支持档全部高于请求档：取最低支持档。
	lowest, lowestIdx := "", 1<<30
	for _, s := range supported {
		idx, k := effortRank[strings.TrimSpace(strings.ToLower(s))]
		if k && idx < lowestIdx {
			lowest, lowestIdx = s, idx
		}
	}
	if lowest != "" {
		obj[key] = lowest
		log.Printf("reasoning_effort floored model=%s %s -> %s", model, reqStr, lowest)
	}
}

// normalizeRoles 把 messages 里的 developer 角色归一为 system。
//
// 背景：上游对 messages 的 role 字段做白名单校验，developer 不在白名单内，
// 命中即 HTTP 400 code=11128。developer 是 OpenAI 新规范里 system 的别名
// （Codex / Cursor 等新客户端用它承载 system 级指令），改写为 system 不丢语义。
//
// 此归一化是「协议兼容」（补上游 role 白名单），不是「内容脱敏」，
// 因此有意与 SanitizeFingerprints / sanitize 参数解耦：即使 sanitize=false 也照常归一。
//
// 只认 developer 这一个值：其余 role（system/user/assistant/tool/任意未知值）一律原样保留，
// 不合并、不重排、不删除任何消息（上游对多 system 的行为尚未实测，合并会引入新变量）。
func normalizeRoles(obj map[string]any) {
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return
	}
	for i, m := range msgs {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		role, ok := msg["role"].(string)
		if !ok {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(role), "developer") {
			msg["role"] = "system"
			log.Printf("role normalized developer->system idx=%d", i)
		}
	}
}

// normalizeImageURL 兼容 OpenAI chat 多模态内容的两种 image_url 写法。
//
// OpenAI Chat Completions 规范使用对象形态 {"url":"...","detail":"..."}，
// 部分客户端（以及 Responses -> Chat 转换器）会发送字符串形态 "data:..." 或
// "https://..."。WorkBuddy 上游只接受对象形态，字符串会返回 400 code=11101
// "cannot unmarshal string into ... ImageContent"。
//
// 这里只做形状转换：字符串转 {"url": 原值}；已有对象及其中 url/detail/mime_type
// 原样保留；空字符串、缺失值、对象内非法 url 一律不补默认值，让上游返回真实错误。
func normalizeImageURL(obj map[string]any) {
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return
	}
	for _, rawMsg := range msgs {
		msg, ok := rawMsg.(map[string]any)
		if !ok {
			continue
		}
		parts, ok := msg["content"].([]any)
		if !ok {
			continue
		}
		for _, rawPart := range parts {
			part, ok := rawPart.(map[string]any)
			if !ok || part["type"] != "image_url" {
				continue
			}
			imageURL, ok := part["image_url"].(string)
			if !ok || imageURL == "" {
				continue
			}
			part["image_url"] = map[string]any{"url": imageURL}
		}
	}
}

// dropReasoningContentParts 剔除 chat content 数组里的 reasoning_text / reasoning 片段。
//
// Codex 只走 /v1/responses：new-api 的 responses→chat 转换器
// （openai_responses_to_openai_chat_completions）会把助手历史里的思考片段原样透传进
// /v1/chat/completions。上游 content 类型白名单不含 reasoning_text，命中即 HTTP 400
// code=11101 "unsupported content type at index 0: reasoning_text"。
//
// 思考片段只是模型私有推理，下游续写不需要，剔除无副作用：
//   - 删除 type 为 reasoning_text / reasoning 的片段（TrimSpace 后比较）
//   - 若删除后 content 为空且该消息无 tool_calls，整条消息一并删除
func dropReasoningContentParts(obj map[string]any) {
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return
	}
	out := make([]any, 0, len(msgs))
	for _, rawMsg := range msgs {
		msg, ok := rawMsg.(map[string]any)
		if !ok {
			out = append(out, rawMsg)
			continue
		}
		parts, ok := msg["content"].([]any)
		if !ok {
			out = append(out, rawMsg)
			continue
		}
		kept := make([]any, 0, len(parts))
		for _, rawPart := range parts {
			part, ok := rawPart.(map[string]any)
			if !ok {
				kept = append(kept, rawPart)
				continue
			}
			ptype, _ := part["type"].(string)
			if strings.TrimSpace(ptype) == "reasoning_text" || strings.TrimSpace(ptype) == "reasoning" {
				continue
			}
			kept = append(kept, rawPart)
		}
		if len(kept) == 0 {
			if _, hasToolCalls := msg["tool_calls"]; !hasToolCalls {
				continue
			}
		}
		msg["content"] = kept
		out = append(out, rawMsg)
	}
	obj["messages"] = out
}

// ensureConsoleSystem global realm 兜底 system 注入（吸收 PR #45，防 console 域上游 code 11-128）：
// 首条消息非 system 时在 messages 最前补一条 fallback system（"You are a helpful assistant."）。
// 仅对 global 请求调用（CN 现状不动；即使首条就是 system 也不重复注入）。
// body 不可解析时原样返回（与 prepareBody 语义一致：坏 body 不在这里二次错误化）。
func ensureConsoleSystem(body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return body
	}
	first, ok := msgs[0].(map[string]any)
	if ok {
		if role, _ := first["role"].(string); strings.EqualFold(strings.TrimSpace(role), "system") {
			return body // 首条已是 system：不注入
		}
	}
	obj["messages"] = append([]any{map[string]any{"role": "system", "content": "You are a helpful assistant."}}, msgs...)
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// normalizeToolChoice 按上游 Go struct（string 类型）改写 OpenAI tool_choice。
//   - "none"            → 删 tool_choice + 删 tools/functions
//   - {"type":"none"}   → 同上
//   - {"type":"auto"/"required"} → 字符串 "auto"/"required"
//   - {"type":"function","function":{"name":"x"}} → 字符串 "x"
//   - 其他对象/非标量 → 删 tool_choice
func normalizeToolChoice(obj map[string]any) {
	suppress := func() {
		delete(obj, "tools")
		delete(obj, "functions")
	}
	tc, present := obj["tool_choice"]
	if !present {
		return
	}
	switch v := tc.(type) {
	case string:
		if strings.EqualFold(strings.TrimSpace(v), "none") {
			delete(obj, "tool_choice")
			suppress()
		}
	case map[string]any:
		typ, _ := v["type"].(string)
		typ = strings.ToLower(strings.TrimSpace(typ))
		switch typ {
		case "none":
			delete(obj, "tool_choice")
			suppress()
		case "auto", "required":
			obj["tool_choice"] = typ
		case "function":
			name := ""
			if fn, ok := v["function"].(map[string]any); ok {
				name, _ = fn["name"].(string)
			}
			if name == "" {
				name, _ = v["name"].(string)
			}
			if name = strings.TrimSpace(name); name != "" {
				obj["tool_choice"] = name
			} else {
				obj["tool_choice"] = "auto"
			}
		default:
			delete(obj, "tool_choice")
		}
	default:
		delete(obj, "tool_choice")
	}
}

// normalizeToolPatterns 归一化 tools 子树里 pattern 的非标准转义 `\_`（→ `_`）。
//
// 上游对 tools[].function.parameters 做严格 JSON Schema/正则文法校验，pattern 含
// `\_`（转义的字面量下划线）会整体拒收：400 code=11129 invalid_function_call_
// parameters（displayMsg「工具定义不合规」）。`\_` 不是任何正则文法的合法转义，
// 但所有主流引擎（RE2/PCRE/JS Annex B）都宽容地视为 `_` 本身——上游校验器比它们
// 全部更严（对照 V8 严格文法 u 标志，唯一同样拒绝的实现）。实案：ZCode 的 exa 插件
// agent_run 工具 runId/previousRunId 带 `^agent\_run\_`，deepseek 系全家确定性 400
// → 网关侧归 ErrClient 只换号不罚但喂连败计数 → 轮转烧满 5 连败触发连败降权、
// 客户端 503（2026-09-29/30 两次实案）。schema 级拒绝换账号无用，只能在发送前修。
//
// 归一无损：`\_` 与 `_` 在所有引擎匹配语义相同（各引擎实测 + 上游对照探针：归一后
// 200），工具方功能不变。只动 tools 子树（pattern 值 + patternProperties 键）；
// 消息正文里的 `\_`（如 Windows 路径 C:\_x）不碰。其余非标转义（`\:` 等）未证实
// 触发，不扩面——有实案再议。独立于 sanitize 开关：这是「让请求通过」，不是脱敏。
func normalizeToolPatterns(obj map[string]any) {
	rawTools, ok := obj["tools"].([]any)
	if !ok {
		return
	}
	for _, raw := range rawTools {
		tool, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		// OpenAI 形态 tools[].function.parameters；裸 tools[].parameters 兼容。
		if fn, ok := tool["function"].(map[string]any); ok {
			unescapePatternLiteralEscapes(fn["parameters"])
		}
		unescapePatternLiteralEscapes(tool["parameters"])
	}
}

// unescapePatternLiteralEscapes 递归改写 schema 树里 pattern 值与 patternProperties
// 键中的 `\_` → `_`（patternProperties 的键也是正则；map 键不可原地改，命中时重建
// 该层）。
func unescapePatternLiteralEscapes(node any) {
	switch n := node.(type) {
	case map[string]any:
		if p, ok := n["pattern"].(string); ok && strings.Contains(p, `\_`) {
			n["pattern"] = strings.ReplaceAll(p, `\_`, `_`)
		}
		if props, ok := n["patternProperties"].(map[string]any); ok {
			rebuilt := false
			fixed := make(map[string]any, len(props))
			for k, v := range props {
				if strings.Contains(k, `\_`) {
					k = strings.ReplaceAll(k, `\_`, `_`)
					rebuilt = true
				}
				fixed[k] = v
			}
			if rebuilt {
				n["patternProperties"] = fixed
			}
		}
		for _, v := range n {
			unescapePatternLiteralEscapes(v)
		}
	case []any:
		for _, v := range n {
			unescapePatternLiteralEscapes(v)
		}
	}
}

// 工具 schema 里这些关键字上游要求固定类型（数字 / 布尔 / 字符串 / 数组）。客户端
// 把数字序列化成字符串（"minimum":"1"）、布尔写成字符串（"uniqueItems":"true"）、
// 或给 enum 一个非数组值时，上游 Go 校验器直接 400 code=11129 invalid function call
// parameters，整条请求被拒——换账号无用（schema 级拒绝），只能在发送前修。
//
// 与 normalizeToolPatterns 同一立场：这是「让请求通过」，不是脱敏，故与 sanitize
// 开关无关。纠正口径只做**类型回正**，不做语义猜测：解析不出来的值直接删（保留
// 错误值只会继续 400，删掉等于放弃这条约束、请求可以通过）。
var (
	// schemaNumericKeywords 必须是数字的约束关键字。
	schemaNumericKeywords = []string{
		"minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum", "multipleOf",
		"minLength", "maxLength", "minItems", "maxItems", "minProperties", "maxProperties",
	}
	// schemaBoolKeywords 必须是布尔的关键字。
	schemaBoolKeywords = []string{"uniqueItems", "deprecated", "readOnly", "writeOnly"}
	// schemaTextKeywords 必须是字符串的关键字（数字/布尔写法无害可转，对象数组只能删）。
	schemaTextKeywords = []string{"description", "title", "format", "pattern"}
	// schemaChildMapKeywords 值是「名字 → 子 schema」映射的关键字（名字本身不是关键字，
	// 只往下递归值，不对属性名做任何纠正）。
	schemaChildMapKeywords = []string{"properties", "patternProperties", "$defs", "definitions"}
	// schemaChildKeywords 值是子 schema 或子 schema 数组的关键字。
	schemaChildKeywords = []string{
		"items", "additionalItems", "not", "if", "then", "else",
		"contains", "propertyNames", "unevaluatedProperties", "unevaluatedItems",
		"allOf", "anyOf", "oneOf", "prefixItems",
	}
)

// hardenToolSchemaKeywords 纠正 tools 子树里 schema 约束关键字的类型（11129 实案：
// 客户端把 "minimum" 写成 "1" 这类字符串，整条请求被上游 Go 校验器拒收）。
//
// 只动 tools[].function.parameters 与裸 tools[].parameters；幂等（已合规的节点一个
// 字节都不改），不碰消息正文、不碰属性名、不碰 default（default:null 是合法写法）。
func hardenToolSchemaKeywords(obj map[string]any) {
	rawTools, ok := obj["tools"].([]any)
	if !ok {
		return
	}
	for _, raw := range rawTools {
		tool, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if fn, ok := tool["function"].(map[string]any); ok {
			hardenSchemaNode(fn["parameters"])
		}
		hardenSchemaNode(tool["parameters"])
	}
}

// hardenSchemaNode 递归纠正单个 schema 节点的关键字类型。
func hardenSchemaNode(node any) {
	m, ok := node.(map[string]any)
	if !ok {
		return
	}
	for _, k := range schemaNumericKeywords {
		if v, has := m[k]; has {
			if nv, ok := coerceSchemaNumber(v); ok {
				m[k] = nv
			} else {
				delete(m, k)
			}
		}
	}
	for _, k := range schemaBoolKeywords {
		if v, has := m[k]; has {
			if nv, ok := coerceSchemaBool(v); ok {
				m[k] = nv
			} else {
				delete(m, k)
			}
		}
	}
	for _, k := range schemaTextKeywords {
		if v, has := m[k]; has {
			if nv, ok := coerceSchemaText(v); ok {
				m[k] = nv
			} else {
				delete(m, k)
			}
		}
	}
	// enum / examples 只认数组；写成逗号串或对象时上游同样按 11129 拒。
	for _, k := range []string{"enum", "examples"} {
		if v, has := m[k]; has {
			if _, ok := v.([]any); !ok {
				delete(m, k)
			}
		}
	}
	// additionalProperties 合法形态是布尔或 schema 对象；字符串化布尔回正，对象递归，
	// 其余（null / 数字）删。
	if v, has := m["additionalProperties"]; has {
		switch n := v.(type) {
		case map[string]any:
			hardenSchemaNode(n)
		case string:
			if nv, ok := coerceSchemaBool(n); ok {
				m["additionalProperties"] = nv
			} else {
				delete(m, "additionalProperties")
			}
		case bool:
			// 已合规
		default:
			delete(m, "additionalProperties")
		}
	}
	// 结构级形态：properties / required / items / type 写成非对象、非数组时，上游
	// 反序列化 schema 就失败（同样 400/11129），而且不像标量那样「转不了就删」就完事——
	// 删掉 properties 等于放弃整个对象的字段约束，只能删；type 删掉只是约束变宽。
	hardenSchemaStructure(m)
	for _, k := range schemaChildMapKeywords {
		children, ok := m[k].(map[string]any)
		if !ok {
			continue
		}
		for _, child := range children {
			hardenSchemaNode(child)
		}
	}
	for _, k := range schemaChildKeywords {
		switch n := m[k].(type) {
		case map[string]any:
			hardenSchemaNode(n)
		case []any:
			for _, child := range n {
				hardenSchemaNode(child)
			}
		}
	}
}

// schemaTypes 是 JSON Schema 认得的类型名（大小写敏感，和上游校验器一致）。
var schemaTypes = map[string]bool{
	"object": true, "array": true, "string": true, "number": true,
	"integer": true, "boolean": true, "null": true,
}

// hardenSchemaStructure 纠正「结构位置写错类型」的关键字：type 给了数组/数字、
// properties 给了数组、required 写成逗号串、items 写成类型名。只做确定性的形态回正，
// 认不出来的直接删（错误值留着只会继续 400）。
func hardenSchemaStructure(m map[string]any) {
	if v, has := m["type"]; has {
		if nv, ok := normalizeSchemaType(v); ok {
			m["type"] = nv
		} else {
			delete(m, "type")
		}
	}
	// properties / patternProperties / $defs / definitions 必须是「名字 → 子 schema」
	// 的对象；给了数组或字符串没法还原，删掉（子 schema 也就没有递归对象了）。
	for _, k := range schemaChildMapKeywords {
		if v, has := m[k]; has {
			if _, ok := v.(map[string]any); !ok {
				delete(m, k)
			}
		}
	}
	if v, has := m["required"]; has {
		if nv, ok := normalizeSchemaRequired(v); ok {
			m["required"] = nv
		} else {
			delete(m, "required")
		}
	}
	// items 合法形态是单个 schema 或 schema 数组（元组写法）；"object" 这种类型名
	// 写法回正成 {"type":"object"}，能从意图上看出来，比删掉更保守。
	if v, has := m["items"]; has {
		if nv, ok := normalizeSchemaItems(v); ok {
			m["items"] = nv
		} else {
			delete(m, "items")
		}
	}
}

// normalizeSchemaType 把 type 回正成合法类型名或类型名数组；认不出报 false（删）。
func normalizeSchemaType(v any) (any, bool) {
	switch n := v.(type) {
	case string:
		if t := strings.TrimSpace(n); schemaTypes[t] {
			return t, true
		}
	case []any:
		out := make([]any, 0, len(n))
		seen := make(map[string]bool, len(n))
		for _, e := range n {
			s, ok := e.(string)
			if !ok {
				continue
			}
			if t := strings.TrimSpace(s); schemaTypes[t] && !seen[t] {
				seen[t] = true
				out = append(out, t)
			}
		}
		if len(out) > 0 {
			return out, true
		}
	}
	return nil, false
}

// normalizeSchemaRequired 把 required 回正成去重后的字符串数组（"a,b" 与单名写法都认）。
func normalizeSchemaRequired(v any) (any, bool) {
	var names []string
	switch n := v.(type) {
	case []any:
		names = make([]string, 0, len(n))
		for _, e := range n {
			if s, ok := e.(string); ok {
				names = append(names, s)
			}
		}
	case string:
		names = strings.Split(n, ",")
	}
	out := make([]any, 0, len(names))
	seen := make(map[string]bool, len(names))
	for _, s := range names {
		t := strings.TrimSpace(s)
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

// normalizeSchemaItems 把 items 回正成 schema 或 schema 数组；给了类型名就包成
// {"type": ...}，其余认不出的报 false（删）。
func normalizeSchemaItems(v any) (any, bool) {
	switch n := v.(type) {
	case map[string]any:
		return n, true
	case string:
		if t := strings.TrimSpace(n); schemaTypes[t] {
			return map[string]any{"type": t}, true
		}
	case []any:
		out := make([]any, 0, len(n))
		for _, e := range n {
			if m, ok := e.(map[string]any); ok {
				out = append(out, m)
			}
		}
		if len(out) > 0 {
			return out, true
		}
	}
	return nil, false
}

// coerceSchemaNumber 把字符串化的数字回正；bool 不算数字（true 不是 1），解析不了报 false。
func coerceSchemaNumber(v any) (any, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(n), 64); err == nil {
			return f, true
		}
	}
	return nil, false
}

// coerceSchemaBool 把字符串化的布尔回正（大小写不敏感），0/1 也认（客户端常见的整数写法）。
func coerceSchemaBool(v any) (bool, bool) {
	switch n := v.(type) {
	case bool:
		return n, true
	case string:
		switch strings.ToLower(strings.TrimSpace(n)) {
		case "true":
			return true, true
		case "false":
			return false, true
		}
	case float64:
		switch n {
		case 1:
			return true, true
		case 0:
			return false, true
		}
	}
	return false, false
}

// coerceSchemaText 把标量值转成字符串；对象/数组/null 转不了，报 false 由调用方删。
func coerceSchemaText(v any) (string, bool) {
	switch n := v.(type) {
	case string:
		return n, true
	case bool:
		if n {
			return "true", true
		}
		return "false", true
	case float64:
		return strconv.FormatFloat(n, 'f', -1, 64), true
	}
	return "", false
}
