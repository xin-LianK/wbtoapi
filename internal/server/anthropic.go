// SPDX-FileCopyrightText: 2026 yreidev 与贡献者（见 NOTICE）
// SPDX-License-Identifier: AGPL-3.0-only
// 复制或改写本文件须保留以上声明，并按 NOTICE 的署名条款注明出处：https://github.com/linguo2625469/workbuddy2api-panel

// anthropic.go Anthropic Messages API 兼容层：POST /v1/messages。
//
// 为什么需要它：Claude Code、cc-switch、以及任何用 @anthropic-ai/sdk 的工具，
// 说的是 Anthropic Messages 协议（`/v1/messages` + 具名 SSE 事件 + `x-api-key`），
// 而本网关此前只有 OpenAI 形态。缺这一层，这些客户端一个都接不进来。
//
// 实现取向：**不做第二条转发流水线**。请求转成 OpenAI chat 形态后交给既有
// chatCompletions（选号、粘性、租约、错误分类、账号处置、用量记账全部复用），
// 响应再经 wire.go 的 sseTap 翻译回 Anthropic 形态。协议差异全部收敛在本文件。
//
// 有意不做的映射（宁可缺字段也不伪造协议语义）：
//   - `thinking` 请求块不转成 reasoning_effort：上游的思考档位有自己的枚举，
//     硬映射会把 Claude 的 budget_tokens 猜成错误档位。上游仍会按自己的
//     默认策略返回 reasoning_content，只是这里不回吐成 thinking 块。
//   - 响应里的 `reasoning_content` **不回吐**：Anthropic 的 thinking 块要求
//     带签名的完整块，网关没有签名可给；塞半块会让严格客户端直接报协议错误。
package server

import (
	"encoding/json"
	"strings"
)

// anthropicRequest Anthropic Messages 请求里本网关关心的字段。
type anthropicRequest struct {
	Model         string               `json:"model"`
	MaxTokens     int                  `json:"max_tokens"`
	Stream        bool                 `json:"stream"`
	System        json.RawMessage      `json:"system"`
	Messages      []anthropicMessage   `json:"messages"`
	Temperature   *float64             `json:"temperature"`
	TopP          *float64             `json:"top_p"`
	StopSequences []string             `json:"stop_sequences"`
	Tools         []anthropicTool      `json:"tools"`
	ToolChoice    *anthropicToolChoice `json:"tool_choice"`
}

// anthropicMessage 单条消息。content 既可能是字符串也可能是 block 数组。
type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// anthropicTool 工具声明。input_schema 即 JSON Schema，直接作为 OpenAI 的 parameters。
type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// anthropicToolChoice `{type:"auto"|"any"|"tool", name?}`。
type anthropicToolChoice struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

// anthropicSource 图片源（base64 或 url 两形态）。
type anthropicSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
	URL       string `json:"url"`
}

// anthropicBlock content 数组里的单个块。只声明本网关会转换的字段。
type anthropicBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
	// image
	Source *anthropicSource `json:"source"`
	// tool_use
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
	// tool_result
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

// parseAnthropicBlocks 把 content（字符串或 block 数组）归一成 block 列表。
func parseAnthropicBlocks(raw json.RawMessage) []anthropicBlock {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if s == "" {
			return nil
		}
		return []anthropicBlock{{Type: "text", Text: s}}
	}
	var blocks []anthropicBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return nil
	}
	return blocks
}

// anthropicSystemText 把 system（字符串或 text block 数组）拼成一段文本。
func anthropicSystemText(raw json.RawMessage) string {
	blocks := parseAnthropicBlocks(raw)
	parts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		if b.Type == "" || b.Type == "text" {
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
	}
	return strings.Join(parts, "\n")
}

// anthropicImagePart 图片块 → OpenAI image_url part。
func anthropicImagePart(b anthropicBlock) any {
	if b.Source == nil {
		return nil
	}
	var url string
	switch b.Source.Type {
	case "base64":
		mt := b.Source.MediaType
		if mt == "" {
			mt = "image/png"
		}
		url = "data:" + mt + ";base64," + b.Source.Data
	case "url":
		url = b.Source.URL
	}
	if url == "" {
		return nil
	}
	return map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}}
}

// anthropicToolResultText tool_result 的 content（字符串 / text block 数组 /
// 任意 JSON）→ 单段文本。role:"tool" 的 content 在 OpenAI 侧只能是字符串。
func anthropicToolResultText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []anthropicBlock
	if json.Unmarshal(raw, &blocks) == nil {
		parts := make([]string, 0, len(blocks))
		for _, b := range blocks {
			if (b.Type == "" || b.Type == "text") && b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	// 结构化 JSON：原样序列化比丢内容好（工具可能返回 JSON 结果）。
	return string(raw)
}

// partsOrString 单文本 part 退回字符串形态：OpenAI 侧 content 为字符串时兼容性最好，
// 数组形态留给真正带图的消息。
func partsOrString(parts []any) any {
	if len(parts) == 0 {
		return ""
	}
	allText := true
	var texts []string
	for _, p := range parts {
		m := asMap(p)
		if m == nil || asString(m["type"]) != "text" {
			allText = false
			break
		}
		texts = append(texts, asString(m["text"]))
	}
	if allText {
		if len(texts) == 1 {
			return texts[0]
		}
		return strings.Join(texts, "\n")
	}
	return parts
}

// anthropicMessagesToChat 把 Anthropic 的 messages 数组转成 OpenAI messages。
//
// 一条 Anthropic user 消息可能同时含 tool_result 与文本块，OpenAI 侧必须拆成
// 「若干 role:tool 消息 + 一条 role:user 消息」，且顺序不能反——tool 消息是
// 对上一轮 assistant tool_calls 的应答，必须紧跟在它后面。
func anthropicMessagesToChat(items []anthropicMessage) []any {
	out := make([]any, 0, len(items)+2)
	for _, m := range items {
		blocks := parseAnthropicBlocks(m.Content)
		if m.Role == "assistant" {
			var texts []string
			var calls []any
			for _, b := range blocks {
				switch b.Type {
				case "text":
					if b.Text != "" {
						texts = append(texts, b.Text)
					}
				case "tool_use":
					args := strings.TrimSpace(string(b.Input))
					if args == "" {
						args = "{}"
					}
					calls = append(calls, map[string]any{
						"id":   b.ID,
						"type": "function",
						"function": map[string]any{
							"name":      b.Name,
							"arguments": args,
						},
					})
				}
			}
			msg := map[string]any{"role": "assistant"}
			if len(texts) > 0 {
				msg["content"] = strings.Join(texts, "\n")
			}
			if len(calls) > 0 {
				msg["tool_calls"] = calls
				if len(texts) == 0 {
					msg["content"] = nil // 对齐 OpenAI：纯工具调用轮 content 为 null
				}
			}
			if _, ok := msg["content"]; !ok {
				msg["content"] = ""
			}
			out = append(out, msg)
			continue
		}

		var toolMsgs []any
		parts := make([]any, 0, len(blocks))
		for _, b := range blocks {
			switch b.Type {
			case "text":
				if b.Text != "" {
					parts = append(parts, map[string]any{"type": "text", "text": b.Text})
				}
			case "image":
				if p := anthropicImagePart(b); p != nil {
					parts = append(parts, p)
				}
			case "tool_result":
				toolMsgs = append(toolMsgs, map[string]any{
					"role":         "tool",
					"tool_call_id": b.ToolUseID,
					"content":      anthropicToolResultText(b.Content),
				})
			}
		}
		out = append(out, toolMsgs...)
		if len(parts) > 0 {
			out = append(out, map[string]any{"role": "user", "content": partsOrString(parts)})
		} else if len(toolMsgs) == 0 {
			// 空 user 消息：保留占位，避免上游看到「只有 system」的畸形请求。
			out = append(out, map[string]any{"role": "user", "content": ""})
		}
	}
	return out
}

// anthropicToChat 组装 OpenAI chat 请求体。
func anthropicToChat(req *anthropicRequest) map[string]any {
	out := map[string]any{
		"model":  req.Model,
		"stream": req.Stream,
	}
	if req.MaxTokens > 0 {
		out["max_tokens"] = req.MaxTokens
	}
	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		out["top_p"] = *req.TopP
	}
	if len(req.StopSequences) > 0 {
		out["stop"] = req.StopSequences
	}

	msgs := make([]any, 0, len(req.Messages)+1)
	if sys := anthropicSystemText(req.System); sys != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": sys})
	}
	msgs = append(msgs, anthropicMessagesToChat(req.Messages)...)
	out["messages"] = msgs

	if len(req.Tools) > 0 {
		tools := make([]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			fn := map[string]any{"name": t.Name}
			if t.Description != "" {
				fn["description"] = t.Description
			}
			if len(t.InputSchema) > 0 && string(t.InputSchema) != "null" {
				fn["parameters"] = t.InputSchema
			} else {
				fn["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			tools = append(tools, map[string]any{"type": "function", "function": fn})
		}
		out["tools"] = tools
	}
	if tc := req.ToolChoice; tc != nil {
		switch tc.Type {
		case "auto":
			out["tool_choice"] = "auto"
		case "any":
			out["tool_choice"] = "required"
		case "tool":
			if tc.Name != "" {
				out["tool_choice"] = map[string]any{
					"type":     "function",
					"function": map[string]any{"name": tc.Name},
				}
			}
		}
	}
	return out
}

// anthropicStop OpenAI finish_reason → Anthropic stop_reason。
