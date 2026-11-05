// SPDX-FileCopyrightText: 2026 yreidev 与贡献者（见 NOTICE）
// SPDX-License-Identifier: AGPL-3.0-only
// 复制或改写本文件须保留以上声明，并按 NOTICE 的署名条款注明出处：https://github.com/linguo2625469/workbuddy2api-panel

// responses.go OpenAI Responses API 兼容层：POST /v1/responses。
//
// 存在理由：Codex CLI 与新版 OpenAI Agent SDK 走的是 Responses API（`input` /
// `output` / `response.*` 事件），而不是 chat/completions。网关只有 chat 形态时，
// 这类客户端要么接不进来，要么得改代码。
//
// 与 anthropic.go 同一取向：请求转成 chat 形态后交给既有流水线，响应经 sseTap
// 翻译回来；不复制选号/租约/记账逻辑。
package server

import (
	"encoding/json"
	"strings"
)

// responsesRequest Responses 请求里本网关关心的字段。
type responsesRequest struct {
	Model           string          `json:"model"`
	Input           json.RawMessage `json:"input"`
	Instructions    string          `json:"instructions"`
	Stream          bool            `json:"stream"`
	MaxOutputTokens int             `json:"max_output_tokens"`
	Temperature     *float64        `json:"temperature"`
	TopP            *float64        `json:"top_p"`
	Tools           []responsesTool `json:"tools"`
	ToolChoice      json.RawMessage `json:"tool_choice"`
	Reasoning       *struct {
		Effort string `json:"effort"`
	} `json:"reasoning"`
	// Text 承载结构化输出约束（text.format）。此前该字段缺失，json.Unmarshal 静默
	// 丢弃 → 客户端要的 JSON Schema 约束根本到不了上游，模型自由发挥，Codex 一类
	// 依赖严格 JSON 的审批/工具链必然失败（上游 issue #258 同款）。
	Text *responsesText `json:"text"`
}

// responsesText Responses 的 text 配置，本网关只关心 format。
type responsesText struct {
	Format *responsesTextFormat `json:"format"`
}

// responsesTextFormat 结构化输出声明。json_schema 的 name/schema/strict 在 Responses
// 里是**扁平**的，chat 侧要收进 json_schema 对象内，转换见 responsesResponseFormat。
type responsesTextFormat struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"schema"`
	Strict      *bool           `json:"strict"`
}

// responsesTool Responses 的工具声明是扁平结构（name 在顶层）。
type responsesTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// responsesItem input 数组里的单项（message / function_call / function_call_output）。
type responsesItem struct {
	Type      string          `json:"type"`
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	Name      string          `json:"name"`
	Arguments string          `json:"arguments"`
	CallID    string          `json:"call_id"`
	Output    json.RawMessage `json:"output"`
}

// responsesPart message content 数组里的单项。
type responsesPart struct {
	Type     string          `json:"type"`
	Text     string          `json:"text"`
	ImageURL json.RawMessage `json:"image_url"`
}

// responsesImageURL 图片地址既可能是字符串，也可能是 {url: "..."}。
func responsesImageURL(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var obj struct {
		URL string `json:"url"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		return obj.URL
	}
	return ""
}

// responsesPartsToChat content 数组 → OpenAI content（字符串或 part 数组）。
func responsesPartsToChat(raw json.RawMessage) any {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []responsesPart
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	out := make([]any, 0, len(parts))
	for _, p := range parts {
		switch p.Type {
		case "input_text", "output_text", "text", "":
			if p.Text != "" {
				out = append(out, map[string]any{"type": "text", "text": p.Text})
			}
		case "input_image", "image_url":
			if u := responsesImageURL(p.ImageURL); u != "" {
				out = append(out, map[string]any{"type": "image_url", "image_url": map[string]any{"url": u}})
			}
		}
	}
	if len(out) == 0 {
		return ""
	}
	return partsOrString(out)
}

// responsesInputToChat 把 input（字符串或 item 数组）转成 OpenAI messages。
//
// function_call / function_call_output 是 Responses 把「工具调用轮」拆成独立 item
// 的形态；chat 侧必须还原成 assistant.tool_calls + role:tool 消息对。连续多个
// function_call 会合并进同一条 assistant 消息（并行工具调用）。
func responsesInputToChat(raw json.RawMessage) []any {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []any{map[string]any{"role": "user", "content": s}}
	}
	var items []responsesItem
	if json.Unmarshal(raw, &items) != nil {
		return nil
	}
	out := make([]any, 0, len(items))
	// pendingCalls 收集连续 function_call，直到遇到非调用 item 才落成一条 assistant 消息。
	var pendingCalls []any
	// openCallIDs 记录「已声明但还没收到 output」的调用 id（FIFO）。用于给缺 call_id
	// 的 function_call_output 回填：上游 chat 侧 role:tool 必须带非空 tool_call_id，
	// 空串会被判参数错误（11101）整条打回。有些客户端（含 Codex 的部分版本）
	// 只在 output 里省了 call_id，但前面的 function_call 是带 id 的，按顺序配对即可。
	var openCallIDs []string
	flushCalls := func() {
		if len(pendingCalls) == 0 {
			return
		}
		out = append(out, map[string]any{
			"role":       "assistant",
			"content":    nil,
			"tool_calls": pendingCalls,
		})
		pendingCalls = nil
	}
	for _, it := range items {
		switch it.Type {
		case "function_call":
			args := strings.TrimSpace(it.Arguments)
			if args == "" {
				args = "{}"
			}
			id := it.CallID
			if id == "" {
				id = it.Name
			}
			pendingCalls = append(pendingCalls, map[string]any{
				"id":       id,
				"type":     "function",
				"function": map[string]any{"name": it.Name, "arguments": args},
			})
			openCallIDs = append(openCallIDs, id)
			continue
		case "function_call_output":
			flushCalls()
			outText := ""
			if len(it.Output) > 0 {
				var str string
				if json.Unmarshal(it.Output, &str) == nil {
					outText = str
				} else {
					outText = string(it.Output)
				}
			}
			callID := it.CallID
			if callID != "" {
				openCallIDs = removeCallID(openCallIDs, callID)
			} else if len(openCallIDs) > 0 {
				// 缺 call_id：按声明顺序取最早那个未消费的调用，配对后出队。
				callID = openCallIDs[0]
				openCallIDs = openCallIDs[1:]
			}
			if callID == "" {
				// 真·孤儿（既没带 id，前面也没有未配对的 function_call）：丢弃。
				// 编一个 id 只会让上游认为「调用不存在」，同样是 400；留空更糟。
				continue
			}
			out = append(out, map[string]any{
				"role":         "tool",
				"tool_call_id": callID,
				"content":      outText,
			})
			continue
		}
		// message item（type 可能为空的老形态）
		flushCalls()
		role := it.Role
		if role == "" {
			role = "user"
		}
		out = append(out, map[string]any{
			"role":    role,
			"content": responsesPartsToChat(it.Content),
		})
	}
	flushCalls()
	return out
}

// removeCallID 从待配对队列里摘掉已消费的调用 id（保持其余顺序）。找不到是无害的：
// 说明该 output 是自带 id 的补充/重复消息，队列本来就没有它。
func removeCallID(ids []string, id string) []string {
	for i, v := range ids {
		if v == id {
			return append(ids[:i], ids[i+1:]...)
		}
	}
	return ids
}

// responsesToolChoice Responses 的 tool_choice → chat 形态。
func responsesToolChoice(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		switch s {
		case "auto", "none":
			return s
		case "required":
			return "required"
		}
		return nil
	}
	var obj struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &obj) == nil && obj.Name != "" {
		return map[string]any{"type": "function", "function": map[string]any{"name": obj.Name}}
	}
	return nil
}

// responsesToChat 组装 chat 请求体。
func responsesToChat(req *responsesRequest) map[string]any {
	out := map[string]any{
		"model":  req.Model,
		"stream": req.Stream,
	}
	if req.MaxOutputTokens > 0 {
		out["max_tokens"] = req.MaxOutputTokens
	}
	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		out["top_p"] = *req.TopP
	}
	msgs := make([]any, 0, 4)
	if strings.TrimSpace(req.Instructions) != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": req.Instructions})
	}
	msgs = append(msgs, responsesInputToChat(req.Input)...)
	out["messages"] = msgs

	if len(req.Tools) > 0 {
		tools := make([]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			if t.Type != "" && t.Type != "function" {
				continue // web_search / file_search 等内置工具上游没有对应能力，跳过
			}
			fn := map[string]any{"name": t.Name}
			if t.Description != "" {
				fn["description"] = t.Description
			}
			if len(t.Parameters) > 0 && string(t.Parameters) != "null" {
				fn["parameters"] = t.Parameters
			} else {
				fn["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			tools = append(tools, map[string]any{"type": "function", "function": fn})
		}
		if len(tools) > 0 {
			out["tools"] = tools
		}
	}
	if tc := responsesToolChoice(req.ToolChoice); tc != nil {
		out["tool_choice"] = tc
	}
	if req.Reasoning != nil && req.Reasoning.Effort != "" {
		out["reasoning_effort"] = req.Reasoning.Effort
	}
	// text.format → response_format：不映射就等于客户端的结构化输出要求被静默吞掉。
	// 只在能构成合法 chat 形态时才发——畸形/不认识的 format 宁可不发，避免上游按
	// 参数错误（11101）直接 400，把「没约束」升级成「请求失败」。
	if rf := responsesResponseFormat(req.Text); rf != nil {
		out["response_format"] = rf
	}
	return out
}

// responsesResponseFormat 把 Responses 的 text.format 转成 chat 的 response_format。
//
// 形态差异（Responses 扁平 / chat 嵌套）：
//
//	{type:"json_schema", name, schema, strict}  →  {type:"json_schema", json_schema:{name, schema, strict}}
//	{type:"json_object"}                        →  {type:"json_object"}
//	{type:"text"} / 缺 schema / 空              →  nil（不约束）
func responsesResponseFormat(t *responsesText) map[string]any {
	if t == nil || t.Format == nil {
		return nil
	}
	f := t.Format
	switch f.Type {
	case "json_object":
		return map[string]any{"type": "json_object"}
	case "json_schema":
		if len(f.Schema) == 0 || string(f.Schema) == "null" {
			return nil
		}
		name := f.Name
		if name == "" {
			// chat 侧 json_schema.name 必填；官方 SDK 缺省时也自填，这里同口径兜底。
			name = "response"
		}
		js := map[string]any{"name": name, "schema": f.Schema}
		if f.Description != "" {
			js["description"] = f.Description
		}
		if f.Strict != nil {
			js["strict"] = *f.Strict
		}
		return map[string]any{"type": "json_schema", "json_schema": js}
	}
	return nil
}
